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
	"gorm.io/gorm/clause"
)

// idempotencyInProgressTimeout 是 in_progress 记录被视为僵死的最短时长。
// 处理进程崩溃或请求超时后键会停留在 in_progress；超过该时长允许同键重新占位，
// 避免客户端被永久拒绝，同时给正常慢请求留出足够窗口。
const idempotencyInProgressTimeout = 5 * time.Minute

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
		// 加行锁串行化同键并发请求：避免两个请求同时看到 in_progress 或过期行后都继续执行。
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&record, "user_id = ? AND key = ?", userID, key).Error
		now := time.Now().UTC()
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
		// 过期记录不能回放：它可能对应早已变化的资源状态，且会阻止新请求占位。
		// 复用同一行重置为新的处理中记录，保持 (user_id, key) 唯一约束不变。
		if !record.ExpiresAt.After(now) {
			return tx.Model(&idempotencyKeyModel{}).Where("id = ?", record.ID).Updates(map[string]any{
				"request_hash": requestHash, "status": "in_progress", "response_json": nil,
				"resource_type": resourceType, "resource_id": "", "expires_at": expiresAt, "created_at": now,
			}).Error
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
				Updates(map[string]any{"status": "in_progress", "response_json": nil, "expires_at": expiresAt, "created_at": now}).Error
		case "in_progress":
			if now.Sub(record.CreatedAt) <= idempotencyInProgressTimeout {
				return biz.ErrIdempotencyConflict
			}
			// 僵死的处理中记录（例如进程崩溃）允许同键重新占位，并重置处理计时。
			return tx.Model(&idempotencyKeyModel{}).Where("id = ? AND status = ?", record.ID, "in_progress").
				Updates(map[string]any{
					"request_hash": requestHash, "response_json": nil, "resource_type": resourceType,
					"resource_id": "", "expires_at": expiresAt, "created_at": now,
				}).Error
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
