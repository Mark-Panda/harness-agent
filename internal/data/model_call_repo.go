package data

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// modelCallModel 对应 model_calls 表：单次模型 Attempt 的执行事实。
type modelCallModel struct {
	// ID 调用记录主键。
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// RunID 所属 Run。
	RunID uuid.UUID `gorm:"type:uuid"`
	// StepID 关联执行步骤，当前可为空。
	StepID *uuid.UUID `gorm:"type:uuid"`
	// UserID 所属用户，RLS 隔离键。
	UserID uuid.UUID `gorm:"type:uuid"`
	// AttemptNo Run 内单调尝试序号。
	AttemptNo int
	// ProviderID 供应商 ID。
	ProviderID *uuid.UUID `gorm:"type:uuid"`
	// ModelID 模型 ID。
	ModelID *uuid.UUID `gorm:"type:uuid"`
	// PolicyVersion 路由策略版本。
	PolicyVersion *int
	// RequestManifest 脱敏请求清单 JSON。
	RequestManifest datatypes.JSON
	// RequestPayloadID 完整请求载荷引用，当前未使用。
	RequestPayloadID *uuid.UUID `gorm:"type:uuid"`
	// ResponsePayloadID 完整响应载荷引用，当前未使用。
	ResponsePayloadID *uuid.UUID `gorm:"type:uuid"`
	// ProviderRequestID 供应商请求 ID。
	ProviderRequestID string
	// Status 调用状态。
	Status string
	// RequestHash 请求 SHA-256 指纹。
	RequestHash []byte
	// ResponseHash 响应 SHA-256 指纹。
	ResponseHash []byte
	// TTFTMS 首 Token 延迟毫秒，非流式调用为空。
	TTFTMS *int64 `gorm:"column:ttft_ms"`
	// DurationMS 调用耗时毫秒。
	DurationMS *int64 `gorm:"column:duration_ms"`
	// InputTokens 输入 Token 数。
	InputTokens int64
	// OutputTokens 输出 Token 数。
	OutputTokens int64
	// EstimatedCost 估算费用。
	EstimatedCost float64
	// FinishReason 模型结束原因。
	FinishReason string
	// SwitchReason 切换原因。
	SwitchReason string
	// ErrorClass 错误分类。
	ErrorClass string
	// ErrorMessage 脱敏错误摘要。
	ErrorMessage string
	// CreatedAt 创建时间。
	CreatedAt time.Time
	// CompletedAt 完成时间。
	CompletedAt *time.Time
}

// TableName 指定模型调用表名。
func (modelCallModel) TableName() string { return "model_calls" }

// ModelCallRepository 是 model_calls 的 GORM 实现，所有查询强制携带 user_id。
type ModelCallRepository struct {
	db *gorm.DB
}

// NewModelCallRepository 构造模型调用仓库。
func NewModelCallRepository(db *gorm.DB) *ModelCallRepository {
	return &ModelCallRepository{db: db}
}

// CreateModelCall 在调用前写入 running 记录；同 Run 的 attempt_no 冲突返回业务冲突。
func (r *ModelCallRepository) CreateModelCall(ctx context.Context, call biz.ModelCall) (biz.ModelCall, error) {
	manifest, err := json.Marshal(nonNilMap(call.RequestManifest))
	if err != nil {
		return biz.ModelCall{}, err
	}
	if len(call.RequestHash) != 0 && len(call.RequestHash) != 32 {
		return biz.ModelCall{}, fmt.Errorf("%w: request hash must be a SHA-256 digest", biz.ErrInvalidConfig)
	}
	now := call.CreatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	record := modelCallModel{
		ID: call.ID, RunID: call.RunID, StepID: call.StepID, UserID: call.UserID, AttemptNo: call.AttemptNo,
		ProviderID: call.ProviderID, ModelID: call.ModelID, PolicyVersion: call.PolicyVersion,
		RequestManifest: datatypes.JSON(manifest), ProviderRequestID: call.ProviderRequestID,
		Status: string(call.Status), RequestHash: call.RequestHash, ResponseHash: call.ResponseHash,
		DurationMS: call.DurationMS, InputTokens: call.InputTokens, OutputTokens: call.OutputTokens,
		EstimatedCost: call.EstimatedCost, FinishReason: call.FinishReason, SwitchReason: call.SwitchReason,
		ErrorClass: call.ErrorClass, ErrorMessage: call.ErrorMessage, CreatedAt: now,
	}
	if record.ID == uuid.Nil {
		record.ID = uuid.New()
	}
	if record.Status == "" {
		record.Status = string(biz.ModelCallRunning)
	}
	err = WithScope(ctx, r.db, Scope{UserID: call.UserID}, func(tx *gorm.DB) error {
		return tx.Create(&record).Error
	})
	if err != nil {
		return biz.ModelCall{}, catalogError("create model call", err)
	}
	return toBizModelCall(record)
}

// FinishModelCall 以 running 为前置条件更新结果，避免重复结算或覆盖终态。
func (r *ModelCallRepository) FinishModelCall(ctx context.Context, userID, callID uuid.UUID, update biz.ModelCallUpdate) (biz.ModelCall, error) {
	if len(update.ResponseHash) != 0 && len(update.ResponseHash) != 32 {
		return biz.ModelCall{}, fmt.Errorf("%w: response hash must be a SHA-256 digest", biz.ErrInvalidConfig)
	}
	if update.CompletedAt.IsZero() {
		update.CompletedAt = time.Now().UTC()
	}
	var record modelCallModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		result := tx.Model(&modelCallModel{}).
			Where("id = ? AND user_id = ? AND status = ?", callID, userID, string(biz.ModelCallRunning)).
			Updates(map[string]any{
				"status": update.Status, "provider_request_id": update.ProviderRequestID,
				"finish_reason": update.FinishReason, "switch_reason": update.SwitchReason,
				"error_class": update.ErrorClass, "error_message": update.ErrorMessage,
				"input_tokens": update.InputTokens, "output_tokens": update.OutputTokens,
				"estimated_cost": update.EstimatedCost, "duration_ms": update.DurationMS,
				"response_hash": update.ResponseHash, "completed_at": update.CompletedAt,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("%w: model call is missing or already finished", biz.ErrResourceConflict)
		}
		if err := tx.First(&record, "id = ? AND user_id = ?", callID, userID).Error; err != nil {
			return err
		}
		// 预算计数与调用结算同事务累加，避免出现已计费但未计入预算的窗口。
		return incrementRunUsage(tx, record, update)
	})
	if err != nil {
		return biz.ModelCall{}, catalogError("finish model call", err)
	}
	return toBizModelCall(record)
}

// incrementRunUsage 在结算事务内按用户、Agent 与供应商累加每日预算计数。
// 只累计非零用量，避免失败但未产生计费的 Attempt 触发无意义写入。
func incrementRunUsage(tx *gorm.DB, call modelCallModel, update biz.ModelCallUpdate) error {
	if update.InputTokens == 0 && update.OutputTokens == 0 && update.EstimatedCost == 0 {
		return nil
	}
	completed := update.CompletedAt.UTC()
	window := time.Date(completed.Year(), completed.Month(), completed.Day(), 0, 0, 0, 0, time.UTC)
	if err := incrementUsage(tx, biz.BudgetScopeUser, call.UserID, window,
		update.InputTokens, update.OutputTokens, update.EstimatedCost, completed); err != nil {
		return err
	}
	if call.ProviderID != nil {
		if err := incrementUsage(tx, biz.BudgetScopeProvider, *call.ProviderID, window,
			update.InputTokens, update.OutputTokens, update.EstimatedCost, completed); err != nil {
			return err
		}
	}
	// Agent 维度需要从 Run 读取，查询仍在用户 RLS 作用域内。
	if call.RunID == uuid.Nil {
		return nil
	}
	var run struct {
		AgentID *uuid.UUID
	}
	if err := tx.Model(&runModel{}).Select("agent_id").
		Where("id = ? AND user_id = ?", call.RunID, call.UserID).Take(&run).Error; err != nil {
		return err
	}
	if run.AgentID != nil {
		if err := incrementUsage(tx, biz.BudgetScopeAgent, *run.AgentID, window,
			update.InputTokens, update.OutputTokens, update.EstimatedCost, completed); err != nil {
			return err
		}
	}
	return nil
}

// ListModelCalls 按 attempt_no 升序读取 Run 的模型尝试记录，强制 user_id 隔离。
func (r *ModelCallRepository) ListModelCalls(ctx context.Context, userID, runID uuid.UUID, limit int) ([]biz.ModelCall, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	var records []modelCallModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Where("user_id = ? AND run_id = ?", userID, runID).
			Order("attempt_no").Limit(limit).Find(&records).Error
	})
	if err != nil {
		return nil, fmt.Errorf("list model calls: %w", err)
	}
	calls := make([]biz.ModelCall, 0, len(records))
	for _, record := range records {
		call, err := toBizModelCall(record)
		if err != nil {
			return nil, err
		}
		calls = append(calls, call)
	}
	return calls, nil
}

// toBizModelCall 将数据库行转换为领域模型调用记录。
func toBizModelCall(record modelCallModel) (biz.ModelCall, error) {
	var manifest map[string]any
	if err := json.Unmarshal(record.RequestManifest, &manifest); err != nil {
		return biz.ModelCall{}, err
	}
	return biz.ModelCall{
		ID: record.ID, RunID: record.RunID, StepID: record.StepID, UserID: record.UserID, AttemptNo: record.AttemptNo,
		ProviderID: record.ProviderID, ModelID: record.ModelID, PolicyVersion: record.PolicyVersion,
		RequestManifest: manifest, ProviderRequestID: record.ProviderRequestID, Status: biz.ModelCallStatus(record.Status),
		RequestHash: record.RequestHash, ResponseHash: record.ResponseHash, DurationMS: record.DurationMS,
		InputTokens: record.InputTokens, OutputTokens: record.OutputTokens, EstimatedCost: record.EstimatedCost,
		FinishReason: record.FinishReason, SwitchReason: record.SwitchReason, ErrorClass: record.ErrorClass,
		ErrorMessage: record.ErrorMessage, CreatedAt: record.CreatedAt, CompletedAt: record.CompletedAt,
	}, nil
}

// 编译期断言接口实现，避免装配阶段才发现契约不一致。
var _ biz.ModelCallRepository = (*ModelCallRepository)(nil)
