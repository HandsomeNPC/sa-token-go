package manager

import (
	"log"

	"github.com/HandsomeNPC/sa-token-go/core/config"
)

// LoginParameter 单次登录的覆盖性参数，未显式赋值的字段回落全局配置。
//
// 对应 Java SaLoginParameter。用于「同一进程内不同客户端用不同超时」的场景
// （如 RuoYi 的 sys_client 按客户端配 timeout / active_timeout）。
type LoginParameter struct {
	// Device 本次登录的设备类型，空串取 DefaultDevice。
	Device string

	// Timeout 本次登录的 token 固定有效期（秒）。0 表示未指定、用全局配置；-1 永不过期。
	//
	// 该有效期是绝对的：到点即失效，不会被活跃续期延长。
	Timeout int64

	// ActiveTimeout 本次登录的 token 最低活跃频率（秒）。
	//
	// 刻意用指针保留三态：nil=未指定(读取时回落全局) / -1=此 token 永不冻结 / >0=具体秒数。
	// 用 int64 会让「未指定」与 0 无法区分。仅在 Config.DynamicActiveTimeout 开启时生效。
	ActiveTimeout *int64

	// IsRememberMe 记住我模式，TTL 取 Config.RememberMeTimeout。
	IsRememberMe bool
}

// NewLoginParameter 按配置构造登录参数。
//
// 对照 Java SaLoginParameter.setDefaultValues：Timeout 在此即时填入全局值，
// 而 ActiveTimeout 刻意留 nil——缓存中无值时读取侧自会回落全局配置。
func NewLoginParameter(cfg *config.Config) *LoginParameter {
	if cfg == nil {
		cfg = config.DefaultConfig()
	}
	return &LoginParameter{
		Device:  DefaultDevice,
		Timeout: cfg.Timeout,
	}
}

// SetActiveTimeout 设置本次登录的活跃超时，免去调用方自行取址。
func (p *LoginParameter) SetActiveTimeout(seconds int64) *LoginParameter {
	p.ActiveTimeout = &seconds
	return p
}

// normalize 兜底空值并剔除不生效的字段，返回自身便于链式调用。
func (p *LoginParameter) normalize(cfg *config.Config) *LoginParameter {
	if p.Device == "" {
		p.Device = DefaultDevice
	}
	if p.Timeout == 0 {
		p.Timeout = cfg.Timeout
	}
	// 对照 Java checkLoginArgs：未开动态开关却传了值，丢弃并告警，避免调用方误以为已生效。
	if !cfg.DynamicActiveTimeout && p.ActiveTimeout != nil {
		if cfg.IsLog {
			log.Printf("[sa-token] DynamicActiveTimeout 未开启，本次登录传入的 ActiveTimeout(%d) 已被忽略", *p.ActiveTimeout)
		}
		p.ActiveTimeout = nil
	}
	return p
}

// expiration 返回本次登录写入存储时使用的 TTL。0 表示永不过期（对齐 storage.Set 语义）。
func (p *LoginParameter) expiration(cfg *config.Config) int64 {
	if p.IsRememberMe && cfg.RememberMeTimeout > 0 {
		return cfg.RememberMeTimeout
	}
	return p.Timeout
}
