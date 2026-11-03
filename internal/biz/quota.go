package biz

import "errors"

// ErrQuotaExceeded 表示用户级资源配额已耗尽。
var ErrQuotaExceeded = errors.New("quota exceeded")

// QuotaLimits 是用户级资源配额上限，非正值时使用安全默认值。
type QuotaLimits struct {
	// MaxAgentsPerUser 单用户 Agent 版本总数上限。
	MaxAgentsPerUser int
	// MaxSessionsPerUser 单用户会话总数上限。
	MaxSessionsPerUser int
}

// 默认配额，防止单用户无限创建资源拖垮存储与服务。
const (
	DefaultMaxAgentsPerUser   = 50
	DefaultMaxSessionsPerUser = 200
)

// Normalize 把未配置或非法上限替换为默认值。
func (l QuotaLimits) Normalize() QuotaLimits {
	if l.MaxAgentsPerUser <= 0 {
		l.MaxAgentsPerUser = DefaultMaxAgentsPerUser
	}
	if l.MaxSessionsPerUser <= 0 {
		l.MaxSessionsPerUser = DefaultMaxSessionsPerUser
	}
	return l
}
