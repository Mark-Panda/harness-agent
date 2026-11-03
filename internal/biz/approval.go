package biz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// 审批相关错误，供上层决定暂停、恢复或进入人工对账。
var (
	// ErrApprovalPending Run 仍存在待决策审批，不得继续执行。
	ErrApprovalPending = errors.New("run has pending tool approvals")
	// ErrApprovalExecutionUnknown 审批已进入执行态但结果未知，必须人工对账，禁止盲目重试。
	ErrApprovalExecutionUnknown = errors.New("tool approval execution state is unknown")
	// ErrToolResultPersistence 工具副作用已执行但结果无法落库，必须人工对账，禁止自动重试导致重复副作用。
	ErrToolResultPersistence = errors.New("tool result persistence failed")
)

// ApprovalStatus 是工具审批的生命周期状态。
type ApprovalStatus string

const (
	// ApprovalPending 等待人工决策。
	ApprovalPending ApprovalStatus = "pending"
	// ApprovalApproved 已批准，等待 Worker 执行。
	ApprovalApproved ApprovalStatus = "approved"
	// ApprovalDenied 已拒绝，工具不得执行。
	ApprovalDenied ApprovalStatus = "denied"
	// ApprovalExecuting 已进入执行态；崩溃后结果未知，需对账。
	ApprovalExecuting ApprovalStatus = "executing"
	// ApprovalExecuted 工具执行成功，结果已保存。
	ApprovalExecuted ApprovalStatus = "executed"
	// ApprovalFailed 工具执行失败，已保存脱敏错误。
	ApprovalFailed ApprovalStatus = "failed"
)

// ApprovalDecision 是人工决策取值。
type ApprovalDecision string

const (
	// ApprovalDecisionApprove 批准执行。
	ApprovalDecisionApprove ApprovalDecision = "approve"
	// ApprovalDecisionDeny 拒绝执行。
	ApprovalDecisionDeny ApprovalDecision = "deny"
)

// ToolApproval 是一次中高风险工具调用的人工审批聚合。
type ToolApproval struct {
	// ID 审批唯一标识。
	ID uuid.UUID
	// RunID 所属 Run。
	RunID uuid.UUID
	// StepID 请求审批的工具步骤，可为空。
	StepID *uuid.UUID
	// UserID 所属用户，RLS 隔离键；决策人必须与之一致。
	UserID uuid.UUID
	// ToolCallID 模型产生的工具调用 ID。
	ToolCallID string
	// ToolName 规范化工具名。
	ToolName string
	// Arguments 待确认的工具参数快照，必须为合法 JSON。
	Arguments json.RawMessage
	// RiskLevel 风险等级：low/medium/high。
	RiskLevel string
	// Status 审批状态。
	Status ApprovalStatus
	// IdempotencyKey 工具执行幂等键，崩溃恢复时用于识别重复副作用。
	IdempotencyKey string
	// ResultJSON 工具执行成功的结果快照。
	ResultJSON json.RawMessage
	// ErrorCode 执行失败业务码。
	ErrorCode string
	// ErrorMessage 脱敏执行失败原因。
	ErrorMessage string
	// DecidedBy 决策人用户 ID。
	DecidedBy *uuid.UUID
	// DecidedAt 决策时间（UTC）。
	DecidedAt *time.Time
	// DeliveredAt 结果回填为 tool 消息的时间（UTC）。
	DeliveredAt *time.Time
	// CreatedAt 创建时间（UTC）。
	CreatedAt time.Time
	// UpdatedAt 更新时间（UTC）。
	UpdatedAt time.Time
}

// ToolApprovalRepository 定义审批持久化契约，所有方法强制携带 userID。
type ToolApprovalRepository interface {
	// CreateApproval 幂等创建审批；同一 Run 与 tool_call_id 重复时返回既有记录。
	CreateApproval(ctx context.Context, approval ToolApproval) (ToolApproval, error)
	// GetApproval 按用户与审批 ID 读取。
	GetApproval(ctx context.Context, userID, approvalID uuid.UUID) (ToolApproval, error)
	// ListApprovals 按创建时间升序返回 Run 的全部审批。
	ListApprovals(ctx context.Context, userID, runID uuid.UUID) ([]ToolApproval, error)
	// CountPendingApprovals 统计 Run 中仍待决策的审批数量。
	CountPendingApprovals(ctx context.Context, userID, runID uuid.UUID) (int, error)
	// DecideApproval 以 CAS 方式把 pending 推进到 approved/denied；重复相同决策幂等返回。
	DecideApproval(ctx context.Context, userID, approvalID uuid.UUID, decision ApprovalDecision, decidedBy uuid.UUID, now time.Time) (ToolApproval, error)
	// ClaimApprovalExecution 以 CAS 方式把 approved 推进到 executing，确保同一审批只有一个执行者。
	ClaimApprovalExecution(ctx context.Context, userID, approvalID uuid.UUID, now time.Time) (ToolApproval, error)
	// FinishApprovalExecution 以 CAS 方式把 executing 推进到 executed/failed，并保存结果或脱敏错误。
	FinishApprovalExecution(ctx context.Context, userID, approvalID uuid.UUID, status ApprovalStatus, result json.RawMessage, errorCode, errorMessage string, now time.Time) (ToolApproval, error)
	// MarkApprovalDelivered 记录结果已回填为 tool 消息，重复调用幂等。
	MarkApprovalDelivered(ctx context.Context, userID, approvalID uuid.UUID, now time.Time) (ToolApproval, error)
}

// TransitionApprovalStatus 校验审批状态迁移；非法迁移返回 ErrResourceConflict。
func TransitionApprovalStatus(current, next ApprovalStatus) error {
	allowed := false
	switch current {
	case ApprovalPending:
		allowed = next == ApprovalApproved || next == ApprovalDenied
	case ApprovalApproved:
		allowed = next == ApprovalExecuting
	case ApprovalExecuting:
		allowed = next == ApprovalExecuted || next == ApprovalFailed
	}
	if !allowed {
		return fmt.Errorf("%w: approval cannot transition from %s to %s", ErrResourceConflict, current, next)
	}
	return nil
}

// ApprovalTerminal 判断审批是否已结束，终态不得再被决策或执行推进。
func ApprovalTerminal(status ApprovalStatus) bool {
	return status == ApprovalExecuted || status == ApprovalFailed || status == ApprovalDenied
}

// ParseApprovalDecision 校验并转换人工决策取值。
func ParseApprovalDecision(raw string) (ApprovalDecision, error) {
	switch ApprovalDecision(raw) {
	case ApprovalDecisionApprove, ApprovalDecisionDeny:
		return ApprovalDecision(raw), nil
	default:
		return "", invalid("approval decision must be approve or deny")
	}
}

// approvalRating 判断风险等级是否必须人工审批；unknown 等级按高风险处理以失败关闭。
func approvalRequired(riskLevel string) bool {
	return riskLevel != "low"
}
