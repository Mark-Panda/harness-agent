package data

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// passwordResetTokenModel 对应 password_reset_tokens 表。
type passwordResetTokenModel struct {
	// ID 令牌主键。
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// UserID 目标用户。
	UserID uuid.UUID `gorm:"type:uuid"`
	// TokenHash 令牌哈希。
	TokenHash []byte
	// ExpiresAt 过期时间。
	ExpiresAt time.Time
	// UsedAt 使用时间。
	UsedAt *time.Time
	// CreatedAt 创建时间。
	CreatedAt time.Time
}

// TableName 指定重置令牌表名。
func (passwordResetTokenModel) TableName() string { return "password_reset_tokens" }

// PasswordResetRepository 是重置令牌的 GORM 实现，仅使用受限认证连接池。
type PasswordResetRepository struct {
	db *gorm.DB
}

// NewPasswordResetRepository 构造重置令牌仓库。
func NewPasswordResetRepository(authDB *gorm.DB) *PasswordResetRepository {
	return &PasswordResetRepository{db: authDB}
}

// CreatePasswordResetToken 写入令牌并与审计事件同事务提交。
func (r *PasswordResetRepository) CreatePasswordResetToken(ctx context.Context, token biz.PasswordResetToken, event biz.OutboxEvent) error {
	if token.UserID == uuid.Nil || len(token.TokenHash) != 32 || token.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: password reset token identity is required", biz.ErrInvalidConfig)
	}
	record := passwordResetTokenModel{
		ID: token.ID, UserID: token.UserID, TokenHash: token.TokenHash,
		ExpiresAt: token.ExpiresAt, CreatedAt: token.CreatedAt,
	}
	if record.ID == uuid.Nil {
		record.ID = uuid.New()
	}
	err := WithScope(ctx, r.db, Scope{UserID: token.UserID}, func(tx *gorm.DB) error {
		if err := tx.Create(&record).Error; err != nil {
			return err
		}
		return appendOutbox(tx, event)
	})
	if err != nil {
		return fmt.Errorf("create password reset token: %w", err)
	}
	return nil
}

// ConsumePasswordResetToken 原子消费未使用且未过期的令牌，返回目标用户。
func (r *PasswordResetRepository) ConsumePasswordResetToken(ctx context.Context, hash []byte, now time.Time) (uuid.UUID, error) {
	if len(hash) != 32 {
		return uuid.Nil, biz.ErrPasswordResetInvalid
	}
	var userIDText string
	err := WithScope(ctx, r.db, Scope{}, func(tx *gorm.DB) error {
		result := tx.Raw(`UPDATE password_reset_tokens SET used_at = ?
			WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?
			RETURNING user_id::text`, now, hash, now).Scan(&userIDText)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 || userIDText == "" {
			return biz.ErrPasswordResetInvalid
		}
		return nil
	})
	if errors.Is(err, biz.ErrPasswordResetInvalid) {
		return uuid.Nil, biz.ErrPasswordResetInvalid
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("consume password reset token: %w", err)
	}
	userID, err := uuid.Parse(userIDText)
	if err != nil || userID == uuid.Nil {
		return uuid.Nil, biz.ErrPasswordResetInvalid
	}
	return userID, nil
}
