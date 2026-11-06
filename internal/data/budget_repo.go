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

// usageCounterModel 对应 usage_counters 表：按维度聚合的每日预算计数。
type usageCounterModel struct {
	// Scope 聚合维度。
	Scope string `gorm:"primaryKey"`
	// ScopeID 维度实体 ID。
	ScopeID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// WindowStart 统计窗口起点。
	WindowStart time.Time `gorm:"primaryKey"`
	// InputTokens 累计输入 Token。
	InputTokens int64
	// OutputTokens 累计输出 Token。
	OutputTokens int64
	// EstimatedCost 累计估算费用。
	EstimatedCost float64
	// UpdatedAt 最近累加时间。
	UpdatedAt time.Time
}

// TableName 指定预算计数表名。
func (usageCounterModel) TableName() string { return "usage_counters" }

// BudgetRepository 读取每日预算聚合。
// usage_counters 只包含平台聚合数值、不含用户正文，因此不按 user_id 隔离。
type BudgetRepository struct {
	db *gorm.DB
}

// NewBudgetRepository 构造预算聚合仓库。
func NewBudgetRepository(db *gorm.DB) *BudgetRepository {
	return &BudgetRepository{db: db}
}

// GetUsage 读取指定维度在窗口内的累计用量；不存在时返回零值。
func (r *BudgetRepository) GetUsage(ctx context.Context, scope biz.BudgetScope, scopeID uuid.UUID, windowStart time.Time) (biz.BudgetUsage, error) {
	if scopeID == uuid.Nil {
		return biz.BudgetUsage{}, fmt.Errorf("%w: budget scope id is required", biz.ErrInvalidConfig)
	}
	var record usageCounterModel
	err := r.db.WithContext(ctx).
		Where("scope = ? AND scope_id = ? AND window_start = ?", string(scope), scopeID, windowStart).
		First(&record).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return biz.BudgetUsage{}, nil
		}
		return biz.BudgetUsage{}, catalogError("get budget usage", err)
	}
	return biz.BudgetUsage{
		InputTokens: record.InputTokens, OutputTokens: record.OutputTokens, EstimatedCost: record.EstimatedCost,
	}, nil
}

// RecordUsage 为后台模型调用累加用户与供应商预算计数；零用量不写库。
func (r *BudgetRepository) RecordUsage(ctx context.Context, userID uuid.UUID, agentID, providerID *uuid.UUID,
	inputTokens, outputTokens int64, estimatedCost float64, now time.Time,
) error {
	if userID == uuid.Nil {
		return fmt.Errorf("%w: user id is required", biz.ErrInvalidConfig)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	window := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := incrementUsage(tx, biz.BudgetScopeUser, userID, window, inputTokens, outputTokens, estimatedCost, now); err != nil {
			return err
		}
		if providerID != nil && *providerID != uuid.Nil {
			if err := incrementUsage(tx, biz.BudgetScopeProvider, *providerID, window, inputTokens, outputTokens, estimatedCost, now); err != nil {
				return err
			}
		}
		if agentID != nil && *agentID != uuid.Nil {
			if err := incrementUsage(tx, biz.BudgetScopeAgent, *agentID, window, inputTokens, outputTokens, estimatedCost, now); err != nil {
				return err
			}
		}
		return nil
	})
}

// incrementUsage 在结算事务内原子累加预算计数；零用量不写库，避免无意义更新。
func incrementUsage(tx *gorm.DB, scope biz.BudgetScope, scopeID uuid.UUID, windowStart time.Time, inputTokens, outputTokens int64, cost float64, now time.Time) error {
	if scopeID == uuid.Nil || (inputTokens == 0 && outputTokens == 0 && cost == 0) {
		return nil
	}
	return tx.Exec(`INSERT INTO usage_counters (scope, scope_id, window_start, input_tokens, output_tokens, estimated_cost, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (scope, scope_id, window_start) DO UPDATE SET
  input_tokens = usage_counters.input_tokens + EXCLUDED.input_tokens,
  output_tokens = usage_counters.output_tokens + EXCLUDED.output_tokens,
  estimated_cost = usage_counters.estimated_cost + EXCLUDED.estimated_cost,
  updated_at = EXCLUDED.updated_at`,
		string(scope), scopeID, windowStart, inputTokens, outputTokens, cost, now).Error
}

// 编译期断言接口实现。
var _ biz.BudgetRepository = (*BudgetRepository)(nil)
