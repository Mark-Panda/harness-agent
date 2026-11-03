package biz

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// 密码重置领域错误。
var (
	// ErrPasswordResetUnavailable 未装配密码重置存储。
	ErrPasswordResetUnavailable = errors.New("password reset is unavailable")
	// ErrPasswordResetInvalid 令牌不存在、已使用或已过期。
	ErrPasswordResetInvalid = errors.New("password reset token is invalid or expired")
)

// PasswordResetToken 是一次性短期密码重置令牌。
type PasswordResetToken struct {
	// ID 令牌主键。
	ID uuid.UUID
	// UserID 目标用户。
	UserID uuid.UUID
	// TokenHash 带 Pepper 的令牌哈希，不保存明文。
	TokenHash []byte
	// ExpiresAt 过期时间。
	ExpiresAt time.Time
	// CreatedAt 创建时间。
	CreatedAt time.Time
}

// PasswordResetRepository 定义一次性重置令牌的持久化契约。
type PasswordResetRepository interface {
	// CreatePasswordResetToken 写入令牌子并与审计事件同事务提交。
	CreatePasswordResetToken(ctx context.Context, token PasswordResetToken, event OutboxEvent) error
	// ConsumePasswordResetToken 原子消费未过期且未使用的令牌，返回目标用户。
	ConsumePasswordResetToken(ctx context.Context, hash []byte, now time.Time) (uuid.UUID, error)
}
