package biz

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// PlanStatus 是 Plan 的生命周期状态。
type PlanStatus string

const (
	// PlanPlanning 正在生成计划。
	PlanPlanning PlanStatus = "planning"
	// PlanExecuting 正在执行步骤。
	PlanExecuting PlanStatus = "executing"
	// PlanWaitingApproval 等待工具审批。
	PlanWaitingApproval PlanStatus = "waiting_approval"
	// PlanPaused 人工或系统暂停。
	PlanPaused PlanStatus = "paused"
	// PlanCompleted 全部步骤成功。
	PlanCompleted PlanStatus = "completed"
	// PlanFailed 存在失败步骤且无法继续。
	PlanFailed PlanStatus = "failed"
	// PlanCancelled 人工终止或 Run 取消。
	PlanCancelled PlanStatus = "cancelled"
)

// PlanStepStatus 是 Plan 步骤的生命周期状态。
type PlanStepStatus string

const (
	// PlanStepPending 等待执行。
	PlanStepPending PlanStepStatus = "pending"
	// PlanStepRunning 执行中。
	PlanStepRunning PlanStepStatus = "running"
	// PlanStepWaitingApproval 等待工具审批。
	PlanStepWaitingApproval PlanStepStatus = "waiting_approval"
	// PlanStepCompleted 步骤成功。
	PlanStepCompleted PlanStepStatus = "completed"
	// PlanStepFailed 步骤失败。
	PlanStepFailed PlanStepStatus = "failed"
	// PlanStepSkipped 依赖失败导致跳过。
	PlanStepSkipped PlanStepStatus = "skipped"
	// PlanStepCancelled 计划取消导致终止。
	PlanStepCancelled PlanStepStatus = "cancelled"
	// PlanStepUnknown 副作用结果未知，需人工对账。
	PlanStepUnknown PlanStepStatus = "unknown"
)

// 计划领域错误。
var (
	// ErrPlanTerminal 计划已到达终态，禁止再次推进。
	ErrPlanTerminal = errors.New("plan is already terminal")
	// ErrPlanStepTerminal 步骤已到达终态，禁止迟到事件回写。
	ErrPlanStepTerminal = errors.New("plan step is already terminal")
	// ErrPlanRevisionConflict 计划 revision 或状态不匹配，通常由并发推进导致。
	ErrPlanRevisionConflict = errors.New("plan revision conflict")
)

// Plan 是一次 Plan-and-Execute 执行的持久化计划，按 Run 版本化。
type Plan struct {
	// ID 计划主键。
	ID uuid.UUID
	// UserID 所属用户，RLS 隔离键。
	UserID uuid.UUID
	// RunID 所属 Run。
	RunID uuid.UUID
	// SessionID 所属会话。
	SessionID uuid.UUID
	// Version 计划版本号，从 1 递增。
	Version int
	// ParentPlanID 修订来源计划。
	ParentPlanID *uuid.UUID
	// Status 计划状态。
	Status PlanStatus
	// Goal 计划目标。
	Goal string
	// ConfigSnapshot 计划生成时的配置快照。
	ConfigSnapshot map[string]any
	// MaxSteps 最大步骤数。
	MaxSteps int
	// TotalInputTokens 累计输入 Token。
	TotalInputTokens int64
	// TotalOutputTokens 累计输出 Token。
	TotalOutputTokens int64
	// EstimatedCost 累计估算费用。
	EstimatedCost float64
	// DeadlineAt 计划截止时间，不得晚于 Run 截止时间。
	DeadlineAt *time.Time
	// ErrorCode 失败业务码。
	ErrorCode string
	// ErrorMessage 脱敏失败原因。
	ErrorMessage string
	// Revision 乐观锁版本，每次推进递增。
	Revision int64
	// CreatedAt 创建时间。
	CreatedAt time.Time
	// UpdatedAt 更新时间。
	UpdatedAt time.Time
	// FinishedAt 终态时间。
	FinishedAt *time.Time
	// Steps 计划步骤，按 step_no 升序。
	Steps []PlanStep
}

// PlanStep 是计划中的单个步骤，保存依赖、工具参数与执行事实。
type PlanStep struct {
	// ID 步骤主键。
	ID uuid.UUID
	// PlanID 所属计划。
	PlanID uuid.UUID
	// UserID 所属用户。
	UserID uuid.UUID
	// StepNo 步骤序号，从 1 连续编号。
	StepNo int
	// Title 步骤标题。
	Title string
	// Description 步骤说明。
	Description string
	// DependsOn 依赖的前置步骤序号。
	DependsOn []int
	// ToolName 可选工具名；为空表示纯模型步骤。
	ToolName string
	// Arguments 工具参数。
	Arguments map[string]any
	// IdempotencyKey 副作用步骤业务幂等键。
	IdempotencyKey string
	// Status 步骤状态。
	Status PlanStepStatus
	// Attempts 已领取次数。
	Attempts int
	// MaxAttempts 最大尝试次数。
	MaxAttempts int
	// FenceToken 步骤执行栅栏令牌。
	FenceToken int64
	// LeaseOwner 当前执行 Worker。
	LeaseOwner string
	// ExecutionStepID 关联的通用执行步骤。
	ExecutionStepID *uuid.UUID
	// Result 步骤结果快照。
	Result map[string]any
	// ErrorCode 失败业务码。
	ErrorCode string
	// ErrorMessage 脱敏失败原因。
	ErrorMessage string
	// StartedAt 开始时间。
	StartedAt *time.Time
	// FinishedAt 结束时间。
	FinishedAt *time.Time
	// CreatedAt 创建时间。
	CreatedAt time.Time
	// UpdatedAt 更新时间。
	UpdatedAt time.Time
}

// PlanFence 是步骤执行的栅栏，绑定 Worker 标识与令牌。
type PlanFence struct {
	// Owner Worker 标识。
	Owner string
	// Token 单调递增令牌。
	Token int64
}

// PlanUpdate 是计划状态推进时的可变字段。
type PlanUpdate struct {
	// ErrorCode 失败业务码。
	ErrorCode string
	// ErrorMessage 脱敏失败原因。
	ErrorMessage string
	// InputTokensDelta 本次新增输入 Token。
	InputTokensDelta int64
	// OutputTokensDelta 本次新增输出 Token。
	OutputTokensDelta int64
	// EstimatedCostDelta 本次新增估算费用。
	EstimatedCostDelta float64
	// FinishedAt 终态时间。
	FinishedAt *time.Time
}

// PlanStepUpdate 是步骤状态推进时的可变字段。
type PlanStepUpdate struct {
	// Status 目标状态。
	Status PlanStepStatus
	// Result 结果快照。
	Result map[string]any
	// ErrorCode 失败业务码。
	ErrorCode string
	// ErrorMessage 脱敏失败原因。
	ErrorMessage string
	// ExecutionStepID 关联执行步骤。
	ExecutionStepID *uuid.UUID
	// FinishedAt 结束时间。
	FinishedAt *time.Time
}

// PlanRepository 定义计划持久化契约；所有查询与写入强制携带 user_id。
type PlanRepository interface {
	// CreatePlan 在同一事务内创建计划与全部步骤，并写入审计事件。
	CreatePlan(ctx context.Context, userID uuid.UUID, plan Plan, event OutboxEvent) (Plan, error)
	// GetPlanByRun 读取 Run 的最新版本计划。
	GetPlanByRun(ctx context.Context, userID, runID uuid.UUID) (Plan, error)
	// GetPlan 按 ID 读取计划及步骤。
	GetPlan(ctx context.Context, userID, planID uuid.UUID) (Plan, error)
	// ListPlanVersions 列出 Run 的全部计划版本（不含步骤）。
	ListPlanVersions(ctx context.Context, userID, runID uuid.UUID) ([]Plan, error)
	// UpdatePlan 以 revision 与当前状态为前置条件推进计划；fence 非空时额外校验 Run 租约。
	UpdatePlan(ctx context.Context, userID, planID uuid.UUID, from, to PlanStatus, expectedRevision int64, fence PlanFence, update PlanUpdate) (Plan, error)
	// AddPlanUsage 累加计划的 Token 与费用，不改变状态。
	AddPlanUsage(ctx context.Context, userID, planID uuid.UUID, inputTokens, outputTokens int64, estimatedCost float64, now time.Time) error
	// ClaimPlanStep 以 attempts 为前置条件领取步骤，写入 fencing、执行步骤与开始时间。
	ClaimPlanStep(ctx context.Context, userID, planID, stepID uuid.UUID, executionStepID *uuid.UUID, fence PlanFence, now time.Time) (PlanStep, error)
	// UpdatePlanStep 以 running 状态与 fencing 为前置条件推进步骤。
	UpdatePlanStep(ctx context.Context, userID, planID, stepID uuid.UUID, fence PlanFence, update PlanStepUpdate) (PlanStep, error)
	// ResolvePlanStep 以当前状态为前置条件推进等待审批或待执行步骤，不要求 fencing。
	ResolvePlanStep(ctx context.Context, userID, planID, stepID uuid.UUID, from PlanStepStatus, update PlanStepUpdate) (PlanStep, error)
	// FindPausedPlanBySession 读取会话中最近一个暂停中的计划，用于新消息自动续跑。
	FindPausedPlanBySession(ctx context.Context, userID, sessionID uuid.UUID) (Plan, error)
	// RebindPlanRun 把暂停中的计划迁移到新的 Run 并恢复执行，保留已完成步骤。
	RebindPlanRun(ctx context.Context, userID, planID, runID uuid.UUID, expectedRevision int64, deadlineAt *time.Time, now time.Time) (Plan, error)
}

// PlanTerminal 判断计划是否处于终态。
func PlanTerminal(status PlanStatus) bool {
	switch status {
	case PlanCompleted, PlanFailed, PlanCancelled:
		return true
	default:
		return false
	}
}

// PlanStepTerminal 判断步骤是否处于不可再推进的终态；failed 允许在重试预算内重新领取。
func PlanStepTerminal(status PlanStepStatus) bool {
	switch status {
	case PlanStepCompleted, PlanStepSkipped, PlanStepCancelled:
		return true
	default:
		return false
	}
}

// TransitionPlanStatus 校验计划状态迁移；终态不可回写。
func TransitionPlanStatus(from, to PlanStatus) error {
	if PlanTerminal(from) {
		return fmt.Errorf("%w: %s cannot move to %s", ErrPlanTerminal, from, to)
	}
	allowed := map[PlanStatus][]PlanStatus{
		PlanPlanning:        {PlanExecuting, PlanPaused, PlanFailed, PlanCancelled},
		PlanExecuting:       {PlanWaitingApproval, PlanPaused, PlanCompleted, PlanFailed, PlanCancelled},
		PlanWaitingApproval: {PlanExecuting, PlanPaused, PlanFailed, PlanCancelled},
		PlanPaused:          {PlanExecuting, PlanFailed, PlanCancelled},
	}
	for _, candidate := range allowed[from] {
		if candidate == to {
			return nil
		}
	}
	return fmt.Errorf("%w: unsupported plan transition %s -> %s", ErrResourceConflict, from, to)
}

// TransitionPlanStepStatus 校验步骤状态迁移；终态不可回写。
func TransitionPlanStepStatus(from, to PlanStepStatus) error {
	if PlanStepTerminal(from) {
		return fmt.Errorf("%w: %s cannot move to %s", ErrPlanStepTerminal, from, to)
	}
	allowed := map[PlanStepStatus][]PlanStepStatus{
		PlanStepPending:         {PlanStepRunning, PlanStepSkipped, PlanStepCancelled},
		PlanStepRunning:         {PlanStepWaitingApproval, PlanStepCompleted, PlanStepFailed, PlanStepUnknown, PlanStepCancelled},
		PlanStepWaitingApproval: {PlanStepRunning, PlanStepCompleted, PlanStepFailed, PlanStepCancelled},
		PlanStepFailed:          {PlanStepRunning, PlanStepCancelled},
		PlanStepUnknown:         {PlanStepFailed, PlanStepCancelled},
	}
	for _, candidate := range allowed[from] {
		if candidate == to {
			return nil
		}
	}
	return fmt.Errorf("%w: unsupported plan step transition %s -> %s", ErrResourceConflict, from, to)
}
