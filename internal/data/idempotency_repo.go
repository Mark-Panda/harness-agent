package data

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// idempotencyKeyModel 对应 idempotency_keys 表：写接口幂等记录。
type idempotencyKeyModel struct {
	// ID 记录主键。
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// UserID 所属用户，RLS 隔离键。
	UserID uuid.UUID `gorm:"type:uuid"`
	// Key 客户端幂等键，与用户共同唯一。
	Key string
	// RequestHash 请求指纹，32 字节。
	RequestHash []byte
	// ResponseJSON 已完成请求的响应信封。
	ResponseJSON datatypes.JSON
	// Status 状态：in_progress/completed/failed。
	Status string
	// ResourceType 资源类型。
	ResourceType string
	// ResourceID 资源标识。
	ResourceID string
	// CreatedAt 创建时间。
	CreatedAt time.Time
	// ExpiresAt 过期时间。
	ExpiresAt time.Time
}

// TableName 指定幂等表名。
func (idempotencyKeyModel) TableName() string { return "idempotency_keys" }

// IdempotencyRepository 是幂等记录的 GORM 实现。
type IdempotencyRepository struct {
	db *gorm.DB
}

// NewIdempotencyRepository 构造幂等仓库。
func NewIdempotencyRepository(db *gorm.DB) *IdempotencyRepository {
	return &IdempotencyRepository{db: db}
}

// Begin 在用户作用域事务内占位：新键插入，旧键校验指纹并回放或冲突。
func (r *IdempotencyRepository) Begin(ctx context.Context, userID uuid.UUID, key string, requestHash []byte, resourceType string, expiresAt time.Time) (biz.IdempotencyOutcome, error) {
	var outcome biz.IdempotencyOutcome
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		var record idempotencyKeyModel
		err := tx.First(&record, "user_id = ? AND key = ?", userID, key).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			record = idempotencyKeyModel{
				ID: uuid.New(), UserID: userID, Key: key, RequestHash: requestHash,
				Status: "in_progress", ResourceType: resourceType, ExpiresAt: expiresAt,
			}
			if err := tx.Create(&record).Error; err != nil {
				return err
			}
			return nil
		}
		if err != nil {
			return err
		}
		// 同键不同请求体必须拒绝，避免把 A 请求的结果回放给 B 请求。
		if !bytes.Equal(record.RequestHash, requestHash) {
			return biz.ErrIdempotencyConflict
		}
		switch record.Status {
		case "completed":
			replayed, err := biz.ReplayOutcome(record.ResponseJSON)
			if err != nil {
				return err
			}
			outcome = replayed
			return nil
		case "failed":
			// 失败记录允许同键重试：重置为处理中并刷新过期时间。
			return tx.Model(&idempotencyKeyModel{}).Where("user_id = ? AND key = ?", userID, key).
				Updates(map[string]any{"status": "in_progress", "response_json": nil, "expires_at": expiresAt}).Error
		default:
			return biz.ErrIdempotencyConflict
		}
	})
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		// 并发插入同键时视为冲突，客户端可稍后重试并拿到先完成的结果。
		return biz.IdempotencyOutcome{}, biz.ErrIdempotencyConflict
	}
	if err != nil {
		return biz.IdempotencyOutcome{}, catalogError("begin idempotency", err)
	}
	return outcome, nil
}

// Complete 持久化成功响应，供后续同键请求回放。
func (r *IdempotencyRepository) Complete(ctx context.Context, userID uuid.UUID, key string, envelope []byte, resourceID string) error {
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		result := tx.Model(&idempotencyKeyModel{}).Where("user_id = ? AND key = ? AND status = ?", userID, key, "in_progress").
			Updates(map[string]any{"status": "completed", "response_json": datatypes.JSON(envelope), "resource_id": resourceID})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return biz.ErrIdempotencyConflict
		}
		return nil
	})
	return catalogError("complete idempotency", err)
}

// Fail 标记失败，允许同键重试。
func (r *IdempotencyRepository) Fail(ctx context.Context, userID uuid.UUID, key string) error {
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Model(&idempotencyKeyModel{}).Where("user_id = ? AND key = ? AND status = ?", userID, key, "in_progress").
			Update("status", "failed").Error
	})
	return catalogError("fail idempotency", err)
}

var _ biz.IdempotencyRepository = (*IdempotencyRepository)(nil)
