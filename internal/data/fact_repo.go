package data

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// memoryFactModel 对应 memory_facts 表：用户结构化事实版本。
type memoryFactModel struct {
	// ID 事实版本主键。
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// UserID 所属用户。
	UserID uuid.UUID `gorm:"type:uuid"`
	// SessionID 来源会话。
	SessionID *uuid.UUID `gorm:"type:uuid"`
	// FactKey 事实键。
	FactKey string
	// FactValue JSON 值。
	FactValue datatypes.JSON
	// Confidence 置信度。
	Confidence float64
	// SourceMessageID 来源消息。
	SourceMessageID *uuid.UUID `gorm:"type:uuid"`
	// SourceKind 来源类型。
	SourceKind string
	// Corrected 是否显式纠正。
	Corrected bool
	// Metadata 提取元数据。
	Metadata datatypes.JSON
	// ValidFrom 生效时间。
	ValidFrom time.Time
	// ValidTo 失效时间。
	ValidTo *time.Time
	// Version 版本号。
	Version int64
	// CreatedAt 创建时间。
	CreatedAt time.Time
	// UpdatedAt 更新时间。
	UpdatedAt time.Time
}

// TableName 指定事实表名。
func (memoryFactModel) TableName() string { return "memory_facts" }

// FactRepository 是结构化事实的 GORM 实现，所有查询强制携带 user_id。
type FactRepository struct {
	db *gorm.DB
}

// NewFactRepository 构造事实仓库。
func NewFactRepository(db *gorm.DB) *FactRepository {
	return &FactRepository{db: db}
}

// ListActiveFacts 按更新时间倒序列出当前生效事实。
func (r *FactRepository) ListActiveFacts(ctx context.Context, userID uuid.UUID, limit int) ([]biz.MemoryFact, error) {
	if userID == uuid.Nil {
		return nil, fmt.Errorf("%w: user id is required", biz.ErrInvalidConfig)
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	var records []memoryFactModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Where("user_id = ? AND valid_to IS NULL", userID).
			Order("updated_at DESC, id DESC").Limit(limit).Find(&records).Error
	})
	if err != nil {
		return nil, catalogError("list active facts", err)
	}
	return toBizFacts(records)
}

// GetActiveFact 读取指定键的生效事实。
func (r *FactRepository) GetActiveFact(ctx context.Context, userID uuid.UUID, key string) (biz.MemoryFact, bool, error) {
	if userID == uuid.Nil || key == "" {
		return biz.MemoryFact{}, false, fmt.Errorf("%w: user and fact key are required", biz.ErrInvalidConfig)
	}
	var record memoryFactModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Where("user_id = ? AND fact_key = ? AND valid_to IS NULL", userID, key).First(&record).Error
	})
	if errors.Is(err, biz.ErrResourceNotFound) || errors.Is(err, gorm.ErrRecordNotFound) {
		return biz.MemoryFact{}, false, nil
	}
	if err != nil {
		return biz.MemoryFact{}, false, catalogError("get active fact", err)
	}
	fact, err := toBizFact(record)
	if err != nil {
		return biz.MemoryFact{}, false, err
	}
	return fact, true, nil
}

// SaveFact 以期望版本守卫写入新版本并关闭旧版本；版本冲突时 applied=false。
func (r *FactRepository) SaveFact(ctx context.Context, fact biz.MemoryFact, expectedVersion int64) (biz.MemoryFact, bool, error) {
	if fact.UserID == uuid.Nil || fact.Key == "" || fact.ID == uuid.Nil {
		return biz.MemoryFact{}, false, fmt.Errorf("%w: fact identity is required", biz.ErrInvalidConfig)
	}
	if fact.Version != expectedVersion+1 || expectedVersion < 0 {
		return biz.MemoryFact{}, false, fmt.Errorf("%w: fact version must be expected + 1", biz.ErrInvalidConfig)
	}
	if len(fact.Value) == 0 || !json.Valid(fact.Value) || len(fact.Value) > 8*1024 {
		return biz.MemoryFact{}, false, fmt.Errorf("%w: fact value must be valid JSON under 8KB", biz.ErrInvalidConfig)
	}
	metadata, err := json.Marshal(nonNilMap(fact.Metadata))
	if err != nil {
		return biz.MemoryFact{}, false, err
	}
	now := time.Now().UTC()
	if fact.ValidFrom.IsZero() {
		fact.ValidFrom = now
	}
	if fact.CreatedAt.IsZero() {
		fact.CreatedAt = now
	}
	fact.UpdatedAt = now
	record := memoryFactModel{
		ID: fact.ID, UserID: fact.UserID, SessionID: fact.SessionID, FactKey: fact.Key,
		FactValue: datatypes.JSON(fact.Value), Confidence: fact.Confidence, SourceMessageID: fact.SourceMessageID,
		SourceKind: fact.SourceKind, Corrected: fact.Corrected, Metadata: datatypes.JSON(metadata),
		ValidFrom: fact.ValidFrom, Version: fact.Version, CreatedAt: fact.CreatedAt, UpdatedAt: now,
	}
	applied := false
	conflict := false
	err = WithScope(ctx, r.db, Scope{UserID: fact.UserID}, func(tx *gorm.DB) error {
		var active memoryFactModel
		lockErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("user_id = ? AND fact_key = ? AND valid_to IS NULL", fact.UserID, fact.Key).First(&active).Error
		if lockErr != nil && !errors.Is(lockErr, gorm.ErrRecordNotFound) {
			return lockErr
		}
		var latest int64
		if err := tx.Model(&memoryFactModel{}).
			Where("user_id = ? AND fact_key = ?", fact.UserID, fact.Key).
			Select("COALESCE(MAX(version), 0)").Scan(&latest).Error; err != nil {
			return err
		}
		// 期望版本必须仍是最大版本，防止并发任务用旧版本覆盖新事实。
		if latest != expectedVersion {
			return nil
		}
		if lockErr == nil {
			if err := tx.Model(&memoryFactModel{}).Where("id = ?", active.ID).
				Updates(map[string]any{"valid_to": now, "updated_at": now}).Error; err != nil {
				return err
			}
		}
		if err := tx.Create(&record).Error; err != nil {
			if errors.Is(err, gorm.ErrDuplicatedKey) {
				conflict = true
				return fmt.Errorf("%w: fact version already exists", biz.ErrResourceConflict)
			}
			return err
		}
		applied = true
		return nil
	})
	if err != nil {
		if conflict {
			return biz.MemoryFact{}, false, nil
		}
		return biz.MemoryFact{}, false, catalogError("save fact", err)
	}
	if !applied {
		return biz.MemoryFact{}, false, nil
	}
	return fact, true, nil
}

// ListFactVersions 按版本倒序列出事实历史。
func (r *FactRepository) ListFactVersions(ctx context.Context, userID uuid.UUID, key string, limit int) ([]biz.MemoryFact, error) {
	if userID == uuid.Nil || key == "" {
		return nil, fmt.Errorf("%w: user and fact key are required", biz.ErrInvalidConfig)
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	var records []memoryFactModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Where("user_id = ? AND fact_key = ?", userID, key).
			Order("version DESC").Limit(limit).Find(&records).Error
	})
	if err != nil {
		return nil, catalogError("list fact versions", err)
	}
	return toBizFacts(records)
}

// ListFacts 按更新时间倒序列出用户全部事实版本，供导出与审计。
func (r *FactRepository) ListFacts(ctx context.Context, userID uuid.UUID, limit int) ([]biz.MemoryFact, error) {
	if userID == uuid.Nil {
		return nil, fmt.Errorf("%w: user id is required", biz.ErrInvalidConfig)
	}
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	var records []memoryFactModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Where("user_id = ?", userID).Order("updated_at DESC, id DESC").Limit(limit).Find(&records).Error
	})
	if err != nil {
		return nil, catalogError("list facts", err)
	}
	return toBizFacts(records)
}

// DeleteFact 关闭生效事实，保留历史版本用于审计。
func (r *FactRepository) DeleteFact(ctx context.Context, userID uuid.UUID, key string, now time.Time) error {
	if userID == uuid.Nil || key == "" {
		return fmt.Errorf("%w: user and fact key are required", biz.ErrInvalidConfig)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	result := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Model(&memoryFactModel{}).
			Where("user_id = ? AND fact_key = ? AND valid_to IS NULL", userID, key).
			Updates(map[string]any{"valid_to": now, "updated_at": now}).Error
	})
	if result != nil {
		return catalogError("delete fact", result)
	}
	return nil
}

// toBizFact 把事实行转换为领域事实。
func toBizFact(record memoryFactModel) (biz.MemoryFact, error) {
	var value json.RawMessage
	if len(record.FactValue) > 0 {
		value = append(value, record.FactValue...)
	}
	var metadata map[string]any
	if len(record.Metadata) > 0 {
		if err := json.Unmarshal(record.Metadata, &metadata); err != nil {
			return biz.MemoryFact{}, err
		}
	}
	return biz.MemoryFact{
		ID: record.ID, UserID: record.UserID, SessionID: record.SessionID, Key: record.FactKey,
		Value: value, Confidence: record.Confidence, SourceKind: record.SourceKind,
		Corrected: record.Corrected, SourceMessageID: record.SourceMessageID, Metadata: metadata,
		ValidFrom: record.ValidFrom, ValidTo: record.ValidTo, Version: record.Version,
		CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
	}, nil
}

// toBizFacts 批量转换事实行。
func toBizFacts(records []memoryFactModel) ([]biz.MemoryFact, error) {
	facts := make([]biz.MemoryFact, 0, len(records))
	for _, record := range records {
		fact, err := toBizFact(record)
		if err != nil {
			return nil, err
		}
		facts = append(facts, fact)
	}
	return facts, nil
}

// 编译期断言事实仓储实现。
var _ biz.FactRepository = (*FactRepository)(nil)
