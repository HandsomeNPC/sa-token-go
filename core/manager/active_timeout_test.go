package manager

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/HandsomeNPC/sa-token-go/core/config"
	"github.com/HandsomeNPC/sa-token-go/storage/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// staleActiveTime 把 token 的最后活跃时间改成 ago 秒前，用于伪造空闲。
func staleActiveTime(t *testing.T, mgr *Manager, token string, ago int64) {
	t.Helper()
	info, err := mgr.getTokenInfo(token, false)
	require.NoError(t, err)
	require.NotNil(t, info)
	info.ActiveTime = time.Now().Unix() - ago
	data, err := json.Marshal(info)
	require.NoError(t, err)
	require.NoError(t, mgr.storage.SetKeepTTL(mgr.getTokenKey(token), data))
}

func TestActiveTimeout_NoLimit_NeverFreeze(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ActiveTimeout = config.NoLimit
	mgr := NewManager(memory.NewStorage(), cfg)

	token, err := mgr.Login("1001")
	require.NoError(t, err)

	assert.Equal(t, NeverFreeze, mgr.GetTokenActiveTimeout(token))
	assert.False(t, mgr.IsFreeze(token))
	assert.NoError(t, mgr.CheckActiveTimeout(token))
	assert.True(t, mgr.IsLogin(token))
}

func TestActiveTimeout_FreshLogin_RemainingNearFull(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ActiveTimeout = 180
	mgr := NewManager(memory.NewStorage(), cfg)

	token, err := mgr.Login("1002")
	require.NoError(t, err)

	remaining := mgr.GetTokenActiveTimeout(token)
	assert.LessOrEqual(t, remaining, int64(180))
	assert.GreaterOrEqual(t, remaining, int64(175))
	assert.False(t, mgr.IsFreeze(token))
}

func TestActiveTimeout_IdleTooLong_Frozen(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ActiveTimeout = 10
	cfg.AutoRenew = false // 否则 IsLogin 会先续签，冻结不可观测
	mgr := NewManager(memory.NewStorage(), cfg)

	token, err := mgr.Login("1003")
	require.NoError(t, err)
	staleActiveTime(t, mgr, token, 60)

	assert.Equal(t, AlreadyFrozen, mgr.GetTokenActiveTimeout(token))
	assert.True(t, mgr.IsFreeze(token))
	assert.Error(t, mgr.CheckActiveTimeout(token))

	// 冻结后登录校验不再放行——这是旧实现完全缺失的行为
	assert.False(t, mgr.IsLogin(token))
	ok, err := mgr.CheckLoginWithState(token)
	assert.False(t, ok)
	assert.Error(t, err)
}

func TestActiveTimeout_ExactlyZeroRemaining_NotFrozen(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ActiveTimeout = 30
	mgr := NewManager(memory.NewStorage(), cfg)

	token, err := mgr.Login("1004")
	require.NoError(t, err)
	// 空闲时长恰好等于允许值：剩余 0，严格 < 0 才算冻结
	staleActiveTime(t, mgr, token, 30)

	assert.Equal(t, int64(0), mgr.GetTokenActiveTimeout(token))
	assert.False(t, mgr.IsFreeze(token))
}

func TestActiveTimeout_MissingActiveTime_TreatedAsFrozen(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ActiveTimeout = 60
	mgr := NewManager(memory.NewStorage(), cfg)

	token, err := mgr.Login("1005")
	require.NoError(t, err)

	// 抹掉活跃时间：须判为已冻结，而非永不冻结
	info, err := mgr.getTokenInfo(token, false)
	require.NoError(t, err)
	info.ActiveTime = 0
	data, err := json.Marshal(info)
	require.NoError(t, err)
	require.NoError(t, mgr.storage.SetKeepTTL(mgr.getTokenKey(token), data))

	assert.Equal(t, AlreadyFrozen, mgr.GetTokenActiveTimeout(token))
	assert.True(t, mgr.IsFreeze(token))
}

func TestActiveTimeout_UnknownToken_Frozen(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ActiveTimeout = 60
	mgr := NewManager(memory.NewStorage(), cfg)

	assert.Equal(t, AlreadyFrozen, mgr.GetTokenActiveTimeout("no-such-token"))
}

func TestActiveTimeout_PerTokenOverride_Wins(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ActiveTimeout = 1800
	cfg.DynamicActiveTimeout = true
	cfg.AutoRenew = false
	mgr := NewManager(memory.NewStorage(), cfg)

	// 本 token 只给 10 秒，应比全局 1800 更严
	p := NewLoginParameter(cfg).SetActiveTimeout(10)
	token, err := mgr.LoginWithParameter("1006", p)
	require.NoError(t, err)

	info, err := mgr.getTokenInfo(token, false)
	require.NoError(t, err)
	require.NotNil(t, info.ActiveTimeout)
	assert.Equal(t, int64(10), *info.ActiveTimeout)
	assert.Equal(t, int64(10), mgr.GetTokenUseActiveTimeoutOrGlobalConfig(token))

	staleActiveTime(t, mgr, token, 60)
	assert.True(t, mgr.IsFreeze(token))
}

func TestActiveTimeout_PerTokenNeverFreeze_OverridesGlobal(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ActiveTimeout = 10
	cfg.DynamicActiveTimeout = true
	cfg.AutoRenew = false
	mgr := NewManager(memory.NewStorage(), cfg)

	// 该 token 声明永不冻结，优先于全局的 10 秒
	p := NewLoginParameter(cfg).SetActiveTimeout(config.NoLimit)
	token, err := mgr.LoginWithParameter("1007", p)
	require.NoError(t, err)

	staleActiveTime(t, mgr, token, 600)
	assert.Equal(t, NeverFreeze, mgr.GetTokenActiveTimeout(token))
	assert.False(t, mgr.IsFreeze(token))
	assert.True(t, mgr.IsLogin(token))
}

func TestActiveTimeout_DynamicDisabled_PerTokenIgnored(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ActiveTimeout = config.NoLimit
	cfg.DynamicActiveTimeout = false // 未开动态开关
	mgr := NewManager(memory.NewStorage(), cfg)

	p := NewLoginParameter(cfg).SetActiveTimeout(10)
	token, err := mgr.LoginWithParameter("1008", p)
	require.NoError(t, err)

	// 传入值被丢弃，不写进 TokenInfo，也不生效
	info, err := mgr.getTokenInfo(token, false)
	require.NoError(t, err)
	assert.Nil(t, info.ActiveTimeout)

	staleActiveTime(t, mgr, token, 600)
	assert.Equal(t, NeverFreeze, mgr.GetTokenActiveTimeout(token))
}

func TestActiveTimeout_UnsetConfig_FailsOpen(t *testing.T) {
	// 手工构造 Config（不走 DefaultConfig）时 ActiveTimeout 是零值 0。
	// 这必须视为「未配置」而非「0 秒即冻结」，否则漏配会把所有人瞬间锁在门外。
	cfg := &config.Config{
		TokenName:     "satoken",
		Timeout:       3600,
		IsConcurrent:  true,
		IsShare:       true,
		MaxLoginCount: -1,
	}
	mgr := NewManager(memory.NewStorage(), cfg)

	token, err := mgr.Login("2001")
	require.NoError(t, err)

	staleActiveTime(t, mgr, token, 100000)
	assert.Equal(t, NeverFreeze, mgr.GetTokenActiveTimeout(token))
	assert.False(t, mgr.IsFreeze(token))
	assert.True(t, mgr.IsLogin(token))
}

func TestActiveTimeout_DynamicOnWithoutPerTokenValue_FailsOpen(t *testing.T) {
	// 开了动态开关但全局与 token 都没配具体秒数：同样不应冻结
	cfg := config.DefaultConfig()
	cfg.ActiveTimeout = config.NoLimit
	cfg.DynamicActiveTimeout = true
	mgr := NewManager(memory.NewStorage(), cfg)

	token, err := mgr.LoginWithParameter("2002", NewLoginParameter(cfg))
	require.NoError(t, err)

	staleActiveTime(t, mgr, token, 100000)
	assert.Equal(t, NeverFreeze, mgr.GetTokenActiveTimeout(token))
	assert.True(t, mgr.IsLogin(token))
}

func TestActiveTimeout_RenewThaws(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ActiveTimeout = 10
	cfg.AutoRenew = false
	mgr := NewManager(memory.NewStorage(), cfg)

	token, err := mgr.Login("1009")
	require.NoError(t, err)
	staleActiveTime(t, mgr, token, 60)
	require.True(t, mgr.IsFreeze(token))

	// 冻结是软状态，续签即解冻
	require.NoError(t, mgr.UpdateLastActiveToNow(token))
	assert.False(t, mgr.IsFreeze(token))
	assert.True(t, mgr.IsLogin(token))
}

func TestActiveTimeout_AutoRenewOnIsLogin(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ActiveTimeout = 60
	cfg.AutoRenew = true
	cfg.RenewPoolConfig = nil // 同步续签，避免测试里等协程
	mgr := NewManager(memory.NewStorage(), cfg)

	token, err := mgr.Login("1010")
	require.NoError(t, err)
	staleActiveTime(t, mgr, token, 30)
	require.Equal(t, int64(30), mgr.GetTokenActiveTimeout(token))

	// 一次登录校验应把活跃窗口推回满额
	require.True(t, mgr.IsLogin(token))
	assert.Greater(t, mgr.GetTokenActiveTimeout(token), int64(55))
}

// 固定 Timeout 必须是绝对的：到点即失效，不被活跃续期延长。
// 旧实现在 IsLogin 里把 TTL 重置回满额，导致偶尔活跃即永不过期。
func TestFixedTimeout_NotExtendedByActivity(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Timeout = 2
	cfg.ActiveTimeout = 60
	cfg.AutoRenew = true
	cfg.MaxRefresh = config.NoLimit
	cfg.RenewPoolConfig = nil
	mgr := NewManager(memory.NewStorage(), cfg)

	token, err := mgr.Login("1011")
	require.NoError(t, err)

	// 保持活跃并确认 TTL 单调递减——旧实现会在此把 TTL 拉回满额
	var prev int64 = 1 << 62
	for range 3 {
		time.Sleep(600 * time.Millisecond)
		require.True(t, mgr.IsLogin(token))
		ttl, err := mgr.GetTokenTimeout(token)
		require.NoError(t, err)
		assert.LessOrEqual(t, ttl, prev, "固定 TTL 不应因活跃而回升")
		prev = ttl
	}

	// 过固定有效期后，无论多活跃都必须失效。
	// 内存存储按秒取整且判定为严格大于，故留足余量避免边界误判。
	time.Sleep(2 * time.Second)
	assert.False(t, mgr.IsLogin(token))
}

func TestLoginWithParameter_PerLoginTimeout(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Timeout = 3600
	mgr := NewManager(memory.NewStorage(), cfg)

	p := NewLoginParameter(cfg)
	p.Timeout = 60 // 本次登录只给 60 秒
	token, err := mgr.LoginWithParameter("1012", p)
	require.NoError(t, err)

	ttl, err := mgr.GetTokenTimeout(token)
	require.NoError(t, err)
	assert.LessOrEqual(t, ttl, int64(60))
	assert.Greater(t, ttl, int64(50))
}

func TestLoginWithParameter_NilFallsBackToConfig(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Timeout = 120
	mgr := NewManager(memory.NewStorage(), cfg)

	token, err := mgr.LoginWithParameter("1013", nil)
	require.NoError(t, err)

	ttl, err := mgr.GetTokenTimeout(token)
	require.NoError(t, err)
	assert.LessOrEqual(t, ttl, int64(120))
	assert.Greater(t, ttl, int64(110))

	info, err := mgr.getTokenInfo(token, false)
	require.NoError(t, err)
	assert.Equal(t, DefaultDevice, info.Device)
}

// Token-Session 的 TTL 须跟随 token 自身，而非全局配置，
// 否则按次登录覆盖 Timeout 后两者失效时间不一致、session 残留 Redis。
func TestTokenSession_TTLFollowsToken(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Timeout = 86400 // 全局 1 天
	mgr := NewManager(memory.NewStorage(), cfg)

	p := NewLoginParameter(cfg)
	p.Timeout = 60 // 本次登录只给 60 秒
	token, err := mgr.LoginWithParameter("1014", p)
	require.NoError(t, err)

	sess, err := mgr.GetTokenSession(token, true)
	require.NoError(t, err)
	require.NotNil(t, sess)

	ttl, err := mgr.GetTokenSessionTimeout(token)
	require.NoError(t, err)
	assert.LessOrEqual(t, ttl, int64(60), "Token-Session 不应沿用全局 Timeout")
	assert.Greater(t, ttl, int64(50))
}
