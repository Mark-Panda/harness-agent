package biz

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// StepKind 是执行步骤类型，对应执行树中的节点种类。
type StepKind string

// StepStatus 是执行步骤生命周期状态。
type StepStatus string

const (
	// StepKindModel 模型调用步骤。
	StepKindModel StepKind = "model"
	// StepKindTool 工具调用步骤。
	StepKindTool StepKind = "tool"
	// StepKindPlan 计划步骤。
	StepKindPlan StepKind = "plan"
	// StepKindWorker Supervisor 子任务步骤。
	StepKindWorker StepKind = "worker"
	// StepKindSystem 系统编排步骤。
	StepKindSystem StepKind = "system"
	// StepPending 步骤已创建但尚未开始。
	StepPending StepStatus = "pending"
	// StepRunning 步骤正在执行。
	StepRunning StepStatus = "running"
	// StepWaitingApproval 高风险工具等待人工审批。
	StepWaitingApproval StepStatus = "waiting_approval"
	// StepCompleted 步骤成功完成。
	StepCompleted StepStatus = "completed"
	// StepFailed 步骤失败。
	StepFailed StepStatus = "failed"
	// StepCancelled 步骤被取消。
	StepCancelled StepStatus = "cancelled"
	// StepReconciliationRequired 副作用结果未知，需要人工对账。
	StepReconciliationRequired StepStatus = "reconciliation_required"
)

// ExecutionStep 是 Run 执行树中的单个节点，记录模型、工具或编排步骤的事实。
type ExecutionStep struct {
	// ID 步骤唯一标识。
	ID uuid.UUID
	// RunID 所属 Run。
	RunID uuid.UUID
	// UserID 所属用户，RLS 隔离键。
	UserID uuid.UUID
	// StepSeq Run 内单调递增步骤序号。
	StepSeq int64
	// ParentStepID 父步骤，用于构建执行树。
	ParentStepID *uuid.UUID
	// Kind 步骤类型。
	Kind StepKind
	// Name 步骤展示名称，如模型名或工具名。
	Name string
	// Status 步骤状态。
	Status StepStatus
	// ToolName 工具名，仅工具步骤使用。
	ToolName string
	// SchemaVersion 步骤载荷版本。
	SchemaVersion int
	// RiskLevel 风险等级：low/medium/high。
	RiskLevel string
	// AttemptNo 所属 Attempt 序号。
	AttemptNo int
	// IdempotencyKey 工具副作用幂等键。
	IdempotencyKey string
	// ErrorCode 失败业务码。
	ErrorCode string
	// StartedAt 开始时间（UTC）。
	StartedAt *time.Time
	// FinishedAt 结束时间（UTC）。
	FinishedAt *time.Time
	// CreatedAt 创建时间（UTC）。
	CreatedAt time.Time
}

// StepUpdate 是步骤结束时的可变字段。
type StepUpdate struct {
	// Status 目标状态。
	Status StepStatus
	// ErrorCode 失败业务码。
	ErrorCode string
	// FinishedAt 结束时间（UTC）。
	FinishedAt time.Time
}

// PayloadType 是执行载荷类型，对应 execution_payloads 表。
type PayloadType string

const (
	// PayloadModelRequest 模型请求清单或脱敏请求。
	PayloadModelRequest PayloadType = "model_request"
	// PayloadModelResponse 模型完整或部分响应。
	PayloadModelResponse PayloadType = "model_response"
	// PayloadToolInput 工具脱敏入参。
	PayloadToolInput PayloadType = "tool_input"
	// PayloadToolOutput 工具脱敏返回。
	PayloadToolOutput PayloadType = "tool_output"
	// PayloadToolError 工具错误。
	PayloadToolError PayloadType = "tool_error"
	// PayloadExternalRequest 外部系统请求。
	PayloadExternalRequest PayloadType = "external_request"
	// PayloadExternalResponse 外部系统响应。
	PayloadExternalResponse PayloadType = "external_response"
)

// CaptureMode 表示载荷采集模式，默认只保存脱敏内容。
type CaptureMode string

const (
	// CaptureMetadataOnly 仅保存元数据，不保存正文。
	CaptureMetadataOnly CaptureMode = "metadata_only"
	// CaptureRedacted 保存脱敏后的有限正文。
	CaptureRedacted CaptureMode = "redacted"
)

// ExecutionPayload 是步骤的请求或响应载荷，必须脱敏且限制大小。
type ExecutionPayload struct {
	// ID 载荷唯一标识。
	ID uuid.UUID
	// RunID 所属 Run。
	RunID uuid.UUID
	// StepID 关联步骤，可为空。
	StepID *uuid.UUID
	// UserID 所属用户，RLS 隔离键。
	UserID uuid.UUID
	// Type 载荷类型。
	Type PayloadType
	// ContentJSON 结构化脱敏载荷。
	ContentJSON map[string]any
	// ContentText 脱敏文本载荷，与 ContentJSON 互斥。
	ContentText string
	// ContentType 内容类型，默认 application/json。
	ContentType string
	// CaptureMode 采集模式。
	CaptureMode CaptureMode
	// SchemaVersion 载荷版本。
	SchemaVersion int
	// RedactionVersion 脱敏规则版本。
	RedactionVersion int
	// OriginalSizeBytes 脱敏前大小。
	OriginalSizeBytes int64
	// SavedSizeBytes 脱敏后大小。
	SavedSizeBytes int64
	// Truncated 是否发生截断。
	Truncated bool
	// Partial 是否为部分内容。
	Partial bool
	// CaptureError 采集失败原因。
	CaptureError string
	// PayloadHash 载荷 SHA-256 指纹。
	PayloadHash []byte
	// CreatedAt 创建时间（UTC）。
	CreatedAt time.Time
}

// ExecutionStepRepository 定义执行步骤与载荷的持久化契约。
type ExecutionStepRepository interface {
	// StartStep 分配 Run 内单调 step_seq 并写入运行中步骤。
	StartStep(ctx context.Context, step ExecutionStep) (ExecutionStep, error)
	// FinishStep 以运行中状态为前置条件推进步骤终态。
	FinishStep(ctx context.Context, userID, stepID uuid.UUID, update StepUpdate) (ExecutionStep, error)
	// MarkStepWaitingApproval 把运行中的工具步骤置为等待审批，结束时间保持为空。
	MarkStepWaitingApproval(ctx context.Context, userID, stepID uuid.UUID) (ExecutionStep, error)
	// ListSteps 按 step_seq 升序返回 Run 的执行步骤。
	ListSteps(ctx context.Context, userID, runID uuid.UUID, limit int) ([]ExecutionStep, error)
	// SavePayload 保存脱敏请求或响应载荷。
	SavePayload(ctx context.Context, payload ExecutionPayload) (ExecutionPayload, error)
	// ListPayloads 返回 Run 或指定步骤的载荷，按创建时间升序。
	ListPayloads(ctx context.Context, userID, runID uuid.UUID, stepID *uuid.UUID, limit int) ([]ExecutionPayload, error)
}
