package biz

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// ModelCallStatus 是单次模型调用的生命周期状态。
type ModelCallStatus string

const (
	// ModelCallRunning 调用已发起，结果尚未落库。
	ModelCallRunning ModelCallStatus = "running"
	// ModelCallSucceeded 调用成功并记录 Token 与费用。
	ModelCallSucceeded ModelCallStatus = "succeeded"
	// ModelCallFailed 调用失败并记录脱敏错误分类。
	ModelCallFailed ModelCallStatus = "failed"
	// ModelCallCancelled 调用因用户取消或 Run Deadline 中止。
	ModelCallCancelled ModelCallStatus = "cancelled"
)

// ModelCall 是一次模型 Attempt 的执行事实，对应 model_calls 表。
// 只保存脱敏元数据与请求指纹，禁止保存凭证或隐藏思维链。
type ModelCall struct {
	// ID 调用记录唯一标识。
	ID uuid.UUID
	// RunID 所属 Run。
	RunID uuid.UUID
	// UserID 所属用户，RLS 隔离键。
	UserID uuid.UUID
	// StepID 关联的执行步骤，当前 ReAct 直接调用时为空。
	StepID *uuid.UUID
	// AttemptNo Run 内单调递增的尝试序号。
	AttemptNo int
	// ProviderID 供应商 ID，用于熔断与配额归集。
	ProviderID *uuid.UUID
	// ModelID 模型 ID，用于切换与费用分析。
	ModelID *uuid.UUID
	// PolicyVersion 路由策略版本。
	PolicyVersion *int
	// RequestManifest 脱敏请求清单，如模型名、步数、预算与工具名，不含用户正文。
	RequestManifest map[string]any
	// ProviderRequestID 供应商请求 ID，用于对账。
	ProviderRequestID string
	// Status 调用状态。
	Status ModelCallStatus
	// RequestHash 规范化请求的 SHA-256 指纹。
	RequestHash []byte
	// ResponseHash 响应的 SHA-256 指纹，可为空。
	ResponseHash []byte
	// DurationMS 调用耗时毫秒。
	DurationMS *int64
	// InputTokens 输入 Token 数。
	InputTokens int64
	// OutputTokens 输出 Token 数。
	OutputTokens int64
	// EstimatedCost 估算费用，单位与价格配置一致。
	EstimatedCost float64
	// FinishReason 模型结束原因。
	FinishReason string
	// SwitchReason 切换到下一候选的原因，仅由路由层填写。
	SwitchReason string
	// ErrorClass 脱敏错误分类。
	ErrorClass string
	// ErrorMessage 脱敏错误摘要。
	ErrorMessage string
	// CreatedAt 创建时间（UTC）。
	CreatedAt time.Time
	// CompletedAt 完成时间（UTC），运行中为空。
	CompletedAt *time.Time
}

// ModelCallUpdate 是一次 Attempt 结束时的可变字段集合。
type ModelCallUpdate struct {
	// Status 目标状态：succeeded/failed/cancelled。
	Status ModelCallStatus
	// ProviderRequestID 供应商请求 ID。
	ProviderRequestID string
	// FinishReason 结束原因。
	FinishReason string
	// SwitchReason 切换原因。
	SwitchReason string
	// ErrorClass 错误分类。
	ErrorClass string
	// ErrorMessage 脱敏错误摘要。
	ErrorMessage string
	// InputTokens 输入 Token。
	InputTokens int64
	// OutputTokens 输出 Token。
	OutputTokens int64
	// EstimatedCost 估算费用。
	EstimatedCost float64
	// DurationMS 调用耗时毫秒。
	DurationMS *int64
	// ResponseHash 响应指纹。
	ResponseHash []byte
	// CompletedAt 完成时间（UTC）。
	CompletedAt time.Time
}

// ModelCallRepository 定义模型 Attempt 的持久化契约，所有查询强制携带 user_id。
type ModelCallRepository interface {
	// CreateModelCall 在调用前写入 running 记录；同 Run 的 attempt_no 必须唯一。
	CreateModelCall(ctx context.Context, call ModelCall) (ModelCall, error)
	// FinishModelCall 以 running 状态为前置条件更新结果，防止重复结算。
	FinishModelCall(ctx context.Context, userID, callID uuid.UUID, update ModelCallUpdate) (ModelCall, error)
	// ListModelCalls 按 Run 顺序读取 Attempt 记录，用于审计与切换分析。
	ListModelCalls(ctx context.Context, userID, runID uuid.UUID, limit int) ([]ModelCall, error)
}
