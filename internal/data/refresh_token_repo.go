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

// refreshTokenModel 对应 refresh_tokens 表：不透明刷新令牌与轮换状态。
type refreshTokenModel struct {
	// ID 令牌主键。
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// UserID 所属用户。
	UserID uuid.UUID `gorm:"type:uuid"`
	// FamilyID 令牌家族 ID。
	FamilyID uuid.UUID `gorm:"type:uuid"`
	// TokenHash 带 Pepper 的令牌哈希。
	TokenHash []byte
	// ParentTokenID 轮换来源令牌。
	ParentTokenID *uuid.UUID `gorm:"type:uuid"`
	// CredentialVersion 签发时用户凭据版本。
	CredentialVersion int64
	// ExpiresAt 过期时间。
	ExpiresAt time.Time
	// RotatedAt 已轮换时间。
	RotatedAt *time.Time
	// RevokedAt 撤销时间。
	RevokedAt *time.Time
	// ReuseDetectedAt 检测到重用的时间。
	ReuseDetectedAt *time.Time
	// CreatedAt 创建时间。
	CreatedAt time.Time
	// LastUsedAt 最近使用时间。
	LastUsedAt *time.Time
}

// TableName 指定刷新令牌表名。
func (refreshTokenModel) TableName() string {
	return "refresh_tokens"
}

// RefreshTokenRepository 是刷新令牌的 GORM 实现，使用认证专用连接池与受限 RLS 作用域。
type RefreshTokenRepository struct {
	authDB *gorm.DB
}

// NewRefreshTokenRepository 构造刷新令牌仓库。
func NewRefreshTokenRepository(authDB *gorm.DB) *RefreshTokenRepository {
	return &RefreshTokenRepository{authDB: authDB}
}

// CreateRefreshToken 创建令牌并与审计事件同事务提交。
func (r *RefreshTokenRepository) CreateRefreshToken(ctx context.Context, token biz.RefreshToken, event biz.OutboxEvent) error {
	model := toRefreshTokenModel(token)
	if model.ID == uuid.Nil {
		model.ID = uuid.New()
	}
	return WithScope(ctx, r.authDB, Scope{UserID: token.UserID}, func(tx *gorm.DB) error {
		if err := tx.Create(&model).Error; err != nil {
			return err
		}
		return appendOutbox(tx, event)
	})
}

// GetRefreshTokenByHash 通过哈希作用域读取令牌，读取范围由 RLS 限制为该哈希。
func (r *RefreshTokenRepository) GetRefreshTokenByHash(ctx context.Context, hash []byte) (biz.RefreshToken, error) {
	var model refreshTokenModel
	err := WithScope(ctx, r.authDB, Scope{RefreshTokenHash: hash}, func(tx *gorm.DB) error {
		return tx.First(&model, "token_hash = ?", hash).Error
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return biz.RefreshToken{}, biz.ErrRefreshTokenNotFound
	}
	if err != nil {
		return biz.RefreshToken{}, fmt.Errorf("get refresh token: %w", err)
	}
	return toBizRefreshToken(model), nil
}

// RotateRefreshToken 原子轮换：条件更新旧令牌（未轮换且未撤销）后创建新令牌。
// 关键步骤：RowsAffected != 1 说明并发轮换或重放，返回冲突由上层触发整族撤销。
func (r *RefreshTokenRepository) RotateRefreshToken(ctx context.Context, userID, oldTokenID uuid.UUID, oldTokenHash []byte, newToken biz.RefreshToken, event biz.OutboxEvent) error {
	return WithScope(ctx, r.authDB, Scope{UserID: userID, RefreshTokenHash: oldTokenHash}, func(tx *gorm.DB) error {
		now := newToken.CreatedAt.UTC()
		result := tx.Model(&refreshTokenModel{}).
			Where("id = ? AND token_hash = ? AND rotated_at IS NULL AND revoked_at IS NULL", oldTokenID, oldTokenHash).
			Updates(map[string]any{"rotated_at": now, "last_used_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return biz.ErrRefreshTokenConflict
		}
		model := toRefreshTokenModel(newToken)
		if model.ID == uuid.Nil {
			model.ID = uuid.New()
		}
		if err := tx.Create(&model).Error; err != nil {
			return err
		}
		return appendOutbox(tx, event)
	})
}

// RevokeRefreshToken 撤销单个令牌，重复撤销保持幂等。
func (r *RefreshTokenRepository) RevokeRefreshToken(ctx context.Context, userID, tokenID uuid.UUID, at time.Time, event biz.OutboxEvent) error {
	return WithScope(ctx, r.authDB, Scope{UserID: userID}, func(tx *gorm.DB) error {
		if err := tx.Model(&refreshTokenModel{}).Where("id = ? AND user_id = ? AND revoked_at IS NULL", tokenID, userID).
			Update("revoked_at", at.UTC()).Error; err != nil {
			return err
		}
		return appendOutbox(tx, event)
	})
}

// RevokeFamily 撤销整个令牌家族；reuseDetected 为真时额外记录重用检测时间。
func (r *RefreshTokenRepository) RevokeFamily(ctx context.Context, userID, familyID uuid.UUID, at time.Time, reuseDetected bool, event biz.OutboxEvent) error {
	return WithScope(ctx, r.authDB, Scope{UserID: userID}, func(tx *gorm.DB) error {
		updates := map[string]any{"revoked_at": at.UTC()}
		if reuseDetected {
			updates["reuse_detected_at"] = at.UTC()
		}
		if err := tx.Model(&refreshTokenModel{}).Where("user_id = ? AND family_id = ? AND revoked_at IS NULL", userID, familyID).Updates(updates).Error; err != nil {
			return err
		}
		return appendOutbox(tx, event)
	})
}

// RevokeAllUserTokens 全端注销：同一事务内提升凭据版本并撤销全部未撤销令牌。
func (r *RefreshTokenRepository) RevokeAllUserTokens(ctx context.Context, userID uuid.UUID, at time.Time, event biz.OutboxEvent) error {
	return WithScope(ctx, r.authDB, Scope{UserID: userID}, func(tx *gorm.DB) error {
		result := tx.Exec("UPDATE users SET credential_version = credential_version + 1, updated_at = ? WHERE id = ?", at.UTC(), userID)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return biz.ErrUserNotFound
		}
		if err := tx.Model(&refreshTokenModel{}).Where("user_id = ? AND revoked_at IS NULL", userID).Update("revoked_at", at.UTC()).Error; err != nil {
			return err
		}
		return appendOutbox(tx, event)
	})
}

// toRefreshTokenModel 将领域令牌转换为数据库行。
func toRefreshTokenModel(token biz.RefreshToken) refreshTokenModel {
	return refreshTokenModel{
		ID: token.ID, UserID: token.UserID, FamilyID: token.FamilyID, TokenHash: token.TokenHash,
		ParentTokenID: token.ParentTokenID, CredentialVersion: token.CredentialVersion, ExpiresAt: token.ExpiresAt,
		RotatedAt: token.RotatedAt, RevokedAt: token.RevokedAt, ReuseDetectedAt: token.ReuseDetectedAt,
		CreatedAt: token.CreatedAt, LastUsedAt: token.LastUsedAt,
	}
}

// toBizRefreshToken 将数据库行转换为领域令牌。
func toBizRefreshToken(model refreshTokenModel) biz.RefreshToken {
	return biz.RefreshToken{
		ID: model.ID, UserID: model.UserID, FamilyID: model.FamilyID, TokenHash: model.TokenHash,
		ParentTokenID: model.ParentTokenID, CredentialVersion: model.CredentialVersion, ExpiresAt: model.ExpiresAt,
		RotatedAt: model.RotatedAt, RevokedAt: model.RevokedAt, ReuseDetectedAt: model.ReuseDetectedAt,
		CreatedAt: model.CreatedAt, LastUsedAt: model.LastUsedAt,
	}
}

var _ biz.RefreshTokenRepository = (*RefreshTokenRepository)(nil)
