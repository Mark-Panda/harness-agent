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

// executionStepModel 对应 execution_steps 表：执行树节点事实。
type executionStepModel struct {
	// ID 步骤主键。
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// RunID 所属 Run。
	RunID uuid.UUID `gorm:"type:uuid"`
	// UserID 所属用户，RLS 隔离键。
	UserID uuid.UUID `gorm:"type:uuid"`
	// StepSeq Run 内单调步骤序号。
	StepSeq int64
	// ParentStepID 父步骤。
	ParentStepID *uuid.UUID `gorm:"type:uuid"`
	// Kind 步骤类型。
	Kind string
	// Name 展示名称。
	Name string
	// Status 步骤状态。
	Status string
	// ToolName 工具名。
	ToolName string
	// SchemaVersion 载荷版本。
	SchemaVersion int
	// RiskLevel 风险等级。
	RiskLevel string
	// AttemptNo 尝试序号。
	AttemptNo int
	// IdempotencyKey 工具幂等键。
	IdempotencyKey string
	// ErrorCode 失败业务码。
	ErrorCode string
	// StartedAt 开始时间。
	StartedAt *time.Time
	// FinishedAt 结束时间。
	FinishedAt *time.Time
	// CreatedAt 创建时间。
	CreatedAt time.Time
}

// TableName 指定执行步骤表名。
func (executionStepModel) TableName() string { return "execution_steps" }

// executionPayloadModel 对应 execution_payloads 表：脱敏请求与响应载荷。
type executionPayloadModel struct {
	// ID 载荷主键。
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// RunID 所属 Run。
	RunID uuid.UUID `gorm:"type:uuid"`
	// StepID 关联步骤。
	StepID *uuid.UUID `gorm:"type:uuid"`
	// UserID 所属用户，RLS 隔离键。
	UserID uuid.UUID `gorm:"type:uuid"`
	// PayloadType 载荷类型。
	PayloadType string
	// ContentJSON 结构化脱敏载荷。
	ContentJSON datatypes.JSON
	// ContentText 脱敏文本载荷。
	ContentText *string
	// ContentType 内容类型。
	ContentType string
	// CaptureMode 采集模式。
	CaptureMode string
	// SchemaVersion 载荷版本。
	SchemaVersion int
	// RedactionVersion 脱敏规则版本。
	RedactionVersion int
	// OriginalSizeBytes 原始大小。
	OriginalSizeBytes int64
	// SavedSizeBytes 保存大小。
	SavedSizeBytes int64
	// Truncated 是否截断。
	Truncated bool
	// Partial 是否部分内容。
	Partial bool
	// CaptureError 采集失败原因。
	CaptureError string
	// PayloadHash 载荷指纹。
	PayloadHash []byte
	// CreatedAt 创建时间。
	CreatedAt time.Time
}

// TableName 指定执行载荷表名。
func (executionPayloadModel) TableName() string { return "execution_payloads" }

// ExecutionStepRepository 是执行步骤与载荷的 GORM 实现，所有查询强制携带 user_id。
type ExecutionStepRepository struct {
	db *gorm.DB
}

// NewExecutionStepRepository 构造执行步骤仓库。
func NewExecutionStepRepository(db *gorm.DB) *ExecutionStepRepository {
	return &ExecutionStepRepository{db: db}
}

// StartStep 在 Run 级 advisory lock 内分配单调 step_seq，避免并发步骤产生重复序号。
func (r *ExecutionStepRepository) StartStep(ctx context.Context, step biz.ExecutionStep) (biz.ExecutionStep, error) {
	if step.ID == uuid.Nil {
		step.ID = uuid.New()
	}
	if step.Status == "" {
		step.Status = biz.StepRunning
	}
	if step.SchemaVersion <= 0 {
		step.SchemaVersion = 1
	}
	// 数据库要求 attempt_no > 0；工具与系统步骤可能不隶属模型 Attempt，统一回退为 1。
	if step.AttemptNo <= 0 {
		step.AttemptNo = 1
	}
	// risk_level 是带枚举检查的非空列；plan/worker 等非工具步骤没有风险等级，
	// 必须显式回退为 low，否则 GORM 会写入空串并触发 check 约束失败。
	if step.RiskLevel == "" {
		step.RiskLevel = "low"
	}
	if step.CreatedAt.IsZero() {
		step.CreatedAt = time.Now().UTC()
	}
	var record executionStepModel
	err := WithScope(ctx, r.db, Scope{UserID: step.UserID}, func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtext(?))", "step:"+step.RunID.String()).Error; err != nil {
			return err
		}
		var maximum int64
		if err := tx.Model(&executionStepModel{}).Where("run_id = ?", step.RunID).
			Select("COALESCE(MAX(step_seq), 0)").Scan(&maximum).Error; err != nil {
			return err
		}
		record = executionStepModel{
			ID: step.ID, RunID: step.RunID, UserID: step.UserID, StepSeq: maximum + 1,
			ParentStepID: step.ParentStepID, Kind: string(step.Kind), Name: step.Name, Status: string(step.Status),
			ToolName: step.ToolName, SchemaVersion: step.SchemaVersion, RiskLevel: step.RiskLevel,
			AttemptNo: step.AttemptNo, IdempotencyKey: step.IdempotencyKey, ErrorCode: step.ErrorCode,
			StartedAt: step.StartedAt, FinishedAt: step.FinishedAt, CreatedAt: step.CreatedAt,
		}
		return tx.Create(&record).Error
	})
	if err != nil {
		return biz.ExecutionStep{}, catalogError("start execution step", err)
	}
	return toBizStep(record), nil
}

// FinishStep 以运行中状态为前置条件推进步骤终态，防止重复结算。
func (r *ExecutionStepRepository) FinishStep(ctx context.Context, userID, stepID uuid.UUID, update biz.StepUpdate) (biz.ExecutionStep, error) {
	if update.FinishedAt.IsZero() {
		update.FinishedAt = time.Now().UTC()
	}
	var record executionStepModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		result := tx.Model(&executionStepModel{}).
			Where("id = ? AND user_id = ? AND status IN ?", stepID, userID,
				[]string{string(biz.StepRunning), string(biz.StepWaitingApproval), string(biz.StepPending)}).
			Updates(map[string]any{
				"status": string(update.Status), "error_code": update.ErrorCode, "finished_at": update.FinishedAt,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("%w: execution step is missing or already finished", biz.ErrResourceConflict)
		}
		return tx.First(&record, "id = ? AND user_id = ?", stepID, userID).Error
	})
	if err != nil {
		return biz.ExecutionStep{}, catalogError("finish execution step", err)
	}
	return toBizStep(record), nil
}

// MarkStepWaitingApproval 把运行中的工具步骤置为等待审批；结束时间保持为空。
func (r *ExecutionStepRepository) MarkStepWaitingApproval(ctx context.Context, userID, stepID uuid.UUID) (biz.ExecutionStep, error) {
	var record executionStepModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		result := tx.Model(&executionStepModel{}).
			Where("id = ? AND user_id = ? AND status = ?", stepID, userID, string(biz.StepRunning)).
			Updates(map[string]any{"status": string(biz.StepWaitingApproval)})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("%w: execution step is missing or not running", biz.ErrResourceConflict)
		}
		return tx.First(&record, "id = ? AND user_id = ?", stepID, userID).Error
	})
	if err != nil {
		return biz.ExecutionStep{}, catalogError("mark execution step waiting approval", err)
	}
	return toBizStep(record), nil
}

// ListSteps 按 step_seq 升序返回执行步骤，limit 上限 500。
func (r *ExecutionStepRepository) ListSteps(ctx context.Context, userID, runID uuid.UUID, limit int) ([]biz.ExecutionStep, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	var records []executionStepModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Where("user_id = ? AND run_id = ?", userID, runID).
			Order("step_seq").Limit(limit).Find(&records).Error
	})
	if err != nil {
		return nil, catalogError("list execution steps", err)
	}
	steps := make([]biz.ExecutionStep, 0, len(records))
	for _, record := range records {
		steps = append(steps, toBizStep(record))
	}
	return steps, nil
}

// SavePayload 保存脱敏载荷；JSON 与文本互斥，二者都为空时仅保存元数据。
func (r *ExecutionStepRepository) SavePayload(ctx context.Context, payload biz.ExecutionPayload) (biz.ExecutionPayload, error) {
	if payload.ID == uuid.Nil {
		payload.ID = uuid.New()
	}
	if len(payload.PayloadHash) != 0 && len(payload.PayloadHash) != 32 {
		return biz.ExecutionPayload{}, fmt.Errorf("%w: payload hash must be a SHA-256 digest", biz.ErrInvalidConfig)
	}
	if payload.ContentType == "" {
		payload.ContentType = "application/json"
	}
	if payload.CaptureMode == "" {
		payload.CaptureMode = biz.CaptureRedacted
	}
	if payload.SchemaVersion <= 0 {
		payload.SchemaVersion = 1
	}
	if payload.RedactionVersion <= 0 {
		payload.RedactionVersion = 1
	}
	if payload.CreatedAt.IsZero() {
		payload.CreatedAt = time.Now().UTC()
	}
	record := executionPayloadModel{
		ID: payload.ID, RunID: payload.RunID, StepID: payload.StepID, UserID: payload.UserID,
		PayloadType: string(payload.Type), ContentType: payload.ContentType, CaptureMode: string(payload.CaptureMode),
		SchemaVersion: payload.SchemaVersion, RedactionVersion: payload.RedactionVersion,
		OriginalSizeBytes: payload.OriginalSizeBytes, SavedSizeBytes: payload.SavedSizeBytes,
		Truncated: payload.Truncated, Partial: payload.Partial, CaptureError: payload.CaptureError,
		PayloadHash: payload.PayloadHash, CreatedAt: payload.CreatedAt,
	}
	if payload.ContentJSON != nil {
		encoded, err := json.Marshal(payload.ContentJSON)
		if err != nil {
			return biz.ExecutionPayload{}, err
		}
		record.ContentJSON = datatypes.JSON(encoded)
		record.SavedSizeBytes = int64(len(encoded))
	}
	if payload.ContentText != "" {
		text := payload.ContentText
		record.ContentText = &text
		if record.SavedSizeBytes == 0 {
			record.SavedSizeBytes = int64(len(text))
		}
	}
	err := WithScope(ctx, r.db, Scope{UserID: payload.UserID}, func(tx *gorm.DB) error {
		return tx.Create(&record).Error
	})
	if err != nil {
		return biz.ExecutionPayload{}, catalogError("save execution payload", err)
	}
	return toBizPayload(record)
}

// ListPayloads 按创建时间升序返回 Run 或步骤载荷。
func (r *ExecutionStepRepository) ListPayloads(ctx context.Context, userID, runID uuid.UUID, stepID *uuid.UUID, limit int) ([]biz.ExecutionPayload, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	var records []executionPayloadModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		query := tx.Where("user_id = ? AND run_id = ?", userID, runID)
		if stepID != nil {
			query = query.Where("step_id = ?", *stepID)
		}
		return query.Order("created_at, id").Limit(limit).Find(&records).Error
	})
	if err != nil {
		return nil, catalogError("list execution payloads", err)
	}
	payloads := make([]biz.ExecutionPayload, 0, len(records))
	for _, record := range records {
		payload, err := toBizPayload(record)
		if err != nil {
			return nil, err
		}
		payloads = append(payloads, payload)
	}
	return payloads, nil
}

// toBizStep 将数据库行转换为领域步骤。
func toBizStep(record executionStepModel) biz.ExecutionStep {
	return biz.ExecutionStep{
		ID: record.ID, RunID: record.RunID, UserID: record.UserID, StepSeq: record.StepSeq,
		ParentStepID: record.ParentStepID, Kind: biz.StepKind(record.Kind), Name: record.Name,
		Status: biz.StepStatus(record.Status), ToolName: record.ToolName, SchemaVersion: record.SchemaVersion,
		RiskLevel: record.RiskLevel, AttemptNo: record.AttemptNo, IdempotencyKey: record.IdempotencyKey,
		ErrorCode: record.ErrorCode, StartedAt: record.StartedAt, FinishedAt: record.FinishedAt,
		CreatedAt: record.CreatedAt,
	}
}

// toBizPayload 将数据库行转换为领域载荷。
func toBizPayload(record executionPayloadModel) (biz.ExecutionPayload, error) {
	var content map[string]any
	if len(record.ContentJSON) > 0 {
		if err := json.Unmarshal(record.ContentJSON, &content); err != nil {
			return biz.ExecutionPayload{}, err
		}
	}
	text := ""
	if record.ContentText != nil {
		text = *record.ContentText
	}
	return biz.ExecutionPayload{
		ID: record.ID, RunID: record.RunID, StepID: record.StepID, UserID: record.UserID,
		Type: biz.PayloadType(record.PayloadType), ContentJSON: content, ContentText: text,
		ContentType: record.ContentType, CaptureMode: biz.CaptureMode(record.CaptureMode),
		SchemaVersion: record.SchemaVersion, RedactionVersion: record.RedactionVersion,
		OriginalSizeBytes: record.OriginalSizeBytes, SavedSizeBytes: record.SavedSizeBytes,
		Truncated: record.Truncated, Partial: record.Partial, CaptureError: record.CaptureError,
		PayloadHash: record.PayloadHash, CreatedAt: record.CreatedAt,
	}, nil
}

// 编译期断言接口实现。
var _ biz.ExecutionStepRepository = (*ExecutionStepRepository)(nil)
