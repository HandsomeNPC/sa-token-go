package manager

import (
	"encoding/json"
	"time"

	"github.com/HandsomeNPC/sa-token-go/core/errs"
	"github.com/HandsomeNPC/sa-token-go/core/session"
)

// 活跃超时判定的两个哨兵值，与存储层 TTL 语义一致。
const (
	// NeverFreeze 永不冻结。
	NeverFreeze int64 = -1
	// AlreadyFrozen 无此值或已冻结。
	AlreadyFrozen int64 = -2
)

// isOpenCheckActiveTimeout 全局是否开启活跃超时校验。
//
// 只有配了正数秒数、或开了动态开关（此时各 token 可自带超时）才算开启。
// 非正数（含手工构造 Config 时的零值）一律视为未配置而非「0 秒即冻结」，
// 否则漏配会把所有人瞬间锁在门外。
func (m *Manager) isOpenCheckActiveTimeout() bool {
	return m.config.ActiveTimeout > 0 || m.config.DynamicActiveTimeout
}

// UpdateLastActiveToNow refreshes TokenInfo.ActiveTime | 更新最后活跃时间
//
// 即使 token 已冻结也会续签成功（即解冻）；需要区分时先调 CheckActiveTimeout。
func (m *Manager) UpdateLastActiveToNow(tokenValue string) error {
	info, err := m.getTokenInfo(tokenValue, false)
	if err != nil {
		return err
	}
	if info == nil {
		return ErrInvalidTokenData
	}
	info.ActiveTime = time.Now().Unix()
	data, err := json.Marshal(info)
	if err != nil {
		return errs.ErrMarshalTokenInfo(err)
	}
	return m.storage.SetKeepTTL(m.getTokenKey(m.CutTokenPrefix(tokenValue)), data)
}

// GetTokenLastActiveTime returns TokenInfo.ActiveTime | 最后活跃时间戳
func (m *Manager) GetTokenLastActiveTime(tokenValue string) (int64, error) {
	info, err := m.getTokenInfo(tokenValue, false)
	if err != nil {
		return 0, err
	}
	if info == nil {
		return 0, ErrInvalidTokenData
	}
	return info.ActiveTime, nil
}

// GetTokenUseActiveTimeoutOrGlobalConfig 返回该 token 实际生效的活跃超时（秒）。
// 未开动态开关、或该 token 没带覆盖值时，回落全局配置。
func (m *Manager) GetTokenUseActiveTimeoutOrGlobalConfig(tokenValue string) int64 {
	if !m.config.DynamicActiveTimeout {
		return m.config.ActiveTimeout
	}
	info, err := m.getTokenInfo(tokenValue, false)
	if err != nil || info == nil || info.ActiveTimeout == nil {
		return m.config.ActiveTimeout
	}
	return *info.ActiveTimeout
}

// GetTokenActiveTimeout 返回 token 距离被冻结的剩余秒数。
// -1 永不冻结；-2 无此值或已冻结；0 代表恰好用尽但尚未冻结。
func (m *Manager) GetTokenActiveTimeout(tokenValue string) int64 {
	// 全局未开启则无需读存储
	if !m.isOpenCheckActiveTimeout() {
		return NeverFreeze
	}
	info, err := m.getTokenInfo(tokenValue, false)
	if err != nil || info == nil {
		return AlreadyFrozen
	}
	return m.activeTimeoutOf(tokenValue, info)
}

// activeTimeoutOf 在已持有 TokenInfo 时计算剩余活跃时间，避免重复读存储。
func (m *Manager) activeTimeoutOf(tokenValue string, info *TokenInfo) int64 {
	if !m.isOpenCheckActiveTimeout() {
		return NeverFreeze
	}

	// 活跃时间缺失视为已冻结，而非永不冻结——宁可误杀，不可漏放
	if info.ActiveTime <= 0 {
		return AlreadyFrozen
	}

	allow := m.config.ActiveTimeout
	if m.config.DynamicActiveTimeout && info.ActiveTimeout != nil {
		allow = *info.ActiveTimeout
	}
	// 该 token 未配具体秒数（-1 永不冻结，或开了动态开关却没带值）则不冻结
	if allow <= 0 {
		return NeverFreeze
	}

	remaining := allow - (time.Now().Unix() - info.ActiveTime)
	if remaining < 0 {
		return AlreadyFrozen
	}
	return remaining
}

// IsFreeze 判断 token 是否已被冻结。
func (m *Manager) IsFreeze(tokenValue string) bool {
	return m.GetTokenActiveTimeout(tokenValue) == AlreadyFrozen
}

// CheckActiveTimeout 已冻结则返回活跃超时错误。
//
// 冻结是纯计算出的软状态：不删任何键，UpdateLastActiveToNow 可解冻。
func (m *Manager) CheckActiveTimeout(tokenValue string) error {
	if m.IsFreeze(tokenValue) {
		return errs.ErrActiveTimeoutWithToken(tokenValue)
	}
	return nil
}

// passActiveTimeout 校验活跃超时并按需续签，返回 token 是否仍可用。
//
// 先判冻结再续签，顺序不可反——反了会让冻结永不可达。
// 调用方须已持有 info 且 tokenValue 已去前缀。
func (m *Manager) passActiveTimeout(tokenValue string, info *TokenInfo) bool {
	if !m.isOpenCheckActiveTimeout() {
		return true
	}
	if m.activeTimeoutOf(tokenValue, info) == AlreadyFrozen {
		return false
	}
	if m.config.AutoRenew {
		// RenewInterval 内只续一次，避免高频请求每次都写存储
		if m.config.RenewInterval <= 0 || !m.storage.Exists(m.getRenewKey(tokenValue)) {
			renewFunc := func() { m.renewToken(tokenValue) }
			if m.renewPool != nil {
				_ = m.renewPool.Submit(renewFunc)
			} else {
				renewFunc()
			}
		}
	}
	return true
}

// GetTokenTimeout returns remaining TTL seconds for token mapping (-1 no limit / key missing semantics) | Token 剩余 TTL
func (m *Manager) GetTokenTimeout(tokenValue string) (int64, error) {
	if tokenValue == "" {
		return -2, nil
	}
	d, err := m.storage.TTL(m.getTokenKey(tokenValue))
	if err != nil {
		return -2, err
	}
	sec := int64(d.Seconds())
	if d < 0 {
		return -1, nil
	}
	return sec, nil
}

// GetSessionTimeout returns TTL for Account-Session | 账号 Session TTL
func (m *Manager) GetSessionTimeout(loginID string) (int64, error) {
	key := m.prefix + session.AccountSessionKeyPrefix + loginID
	d, err := m.storage.TTL(key)
	if err != nil {
		return -2, err
	}
	if d < 0 {
		return -1, nil
	}
	return int64(d.Seconds()), nil
}

// GetTokenSessionTimeout returns TTL for Token-Session storage | Token-Session TTL
func (m *Manager) GetTokenSessionTimeout(tokenValue string) (int64, error) {
	key := m.prefix + session.TokenSessionKeyPrefix + tokenValue
	d, err := m.storage.TTL(key)
	if err != nil {
		return -2, err
	}
	if d < 0 {
		return -1, nil
	}
	return int64(d.Seconds()), nil
}

// RenewTimeout renews token + account + account-session TTL | 显式续期固定有效期
//
// 这是调用方主动延长绝对有效期的入口；登录校验路径不会自动走到这里。
func (m *Manager) RenewTimeout(tokenValue string, seconds int64) error {
	if tokenValue == "" || seconds <= 0 {
		return nil
	}
	dur := time.Duration(seconds) * time.Second
	info, err := m.getTokenInfo(tokenValue, false)
	if err != nil || info == nil {
		return err
	}
	_ = m.storage.Expire(m.getTokenKey(tokenValue), dur)
	_ = m.storage.Expire(m.getAccountKey(info.LoginID, info.Device), dur)
	if sess, err := m.GetSession(info.LoginID); err == nil && sess != nil {
		_ = sess.Renew(dur)
	}
	return nil
}
