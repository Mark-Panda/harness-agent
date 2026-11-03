package biz

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// 刷新令牌领域错误。
var (
	// ErrRefreshTokenNotFound 令牌不存在或已不可用。
	ErrRefreshTokenNotFound = errors.New("refresh token not found")
	// ErrRefreshTokenConflict 轮换并发冲突，通常意味着令牌被重复使用。
	ErrRefreshTokenConflict = errors.New("refresh token rotation conflict")
)

// RefreshToken 是不透明刷新令牌的持久化模型，按 Family 管理轮换与重用检测。
type RefreshToken struct {
	// ID 令牌唯一标识。
	ID uuid.UUID
	// UserID 所属用户。
	UserID uuid.UUID
	// FamilyID 令牌家族 ID，轮换产生的令牌共享同一家族。
	FamilyID uuid.UUID
	// TokenHash 令牌哈希（带 Pepper），不保存明文。
	TokenHash []byte
	// ParentTokenID 轮换来源令牌 ID。
	ParentTokenID *uuid.UUID
	// CredentialVersion 签发时的用户凭据版本，版本提升即全端失效。
	CredentialVersion int64
	// ExpiresAt 过期时间（UTC）。
	ExpiresAt time.Time
	// RotatedAt 轮换时间，非空表示已使用。
	RotatedAt *time.Time
	// RevokedAt 主动撤销时间。
	RevokedAt *time.Time
	// ReuseDetectedAt 检测到重复使用的时间，命中后整族撤销。
	ReuseDetectedAt *time.Time
	// CreatedAt 创建时间（UTC）。
	CreatedAt time.Time
	// LastUsedAt 最近使用时间。
	LastUsedAt *time.Time
}

// RefreshTokenRepository 定义刷新令牌持久化契约，轮换与撤销需原子执行。
type RefreshTokenRepository interface {
	// CreateRefreshToken 创建令牌并写入审计事件。
	CreateRefreshToken(ctx context.Context, token RefreshToken, event OutboxEvent) error
	// GetRefreshTokenByHash 按哈希读取令牌。
	GetRefreshTokenByHash(ctx context.Context, hash []byte) (RefreshToken, error)
	// RotateRefreshToken 原子轮换：旧令牌标记已用并创建新令牌。
	RotateRefreshToken(ctx context.Context, userID, oldTokenID uuid.UUID, oldTokenHash []byte, newToken RefreshToken, event OutboxEvent) error
	// RevokeRefreshToken 撤销单个令牌。
	RevokeRefreshToken(ctx context.Context, userID, tokenID uuid.UUID, at time.Time, event OutboxEvent) error
	// RevokeFamily 撤销整个令牌家族，reuseDetected 标记重用检测。
	RevokeFamily(ctx context.Context, userID, familyID uuid.UUID, at time.Time, reuseDetected bool, event OutboxEvent) error
	// RevokeAllUserTokens 撤销用户全部令牌，用于全端注销。
	RevokeAllUserTokens(ctx context.Context, userID uuid.UUID, at time.Time, event OutboxEvent) error
}
