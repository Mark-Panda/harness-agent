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

// planModel 对应 plans 表：版本化计划与预算事实。
type planModel struct {
	// ID 计划主键。
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// UserID 所属用户，RLS 隔离键。
	UserID uuid.UUID `gorm:"type:uuid"`
	// RunID 所属 Run。
	RunID uuid.UUID `gorm:"type:uuid"`
	// SessionID 所属会话。
	SessionID uuid.UUID `gorm:"type:uuid"`
	// Version 计划版本。
	Version int
	// ParentPlanID 修订来源。
	ParentPlanID *uuid.UUID `gorm:"type:uuid"`
	// Status 计划状态。
	Status string
	// Goal 计划目标。
	Goal string
	// ConfigSnapshot 配置快照。
	ConfigSnapshot datatypes.JSON
	// MaxSteps 最大步骤数。
	MaxSteps int
	// TotalInputTokens 累计输入 Token。
	TotalInputTokens int64
	// TotalOutputTokens 累计输出 Token。
	TotalOutputTokens int64
	// EstimatedCost 累计估算费用。
	EstimatedCost float64
	// DeadlineAt 计划截止时间。
	DeadlineAt *time.Time
	// ErrorCode 失败业务码。
	ErrorCode string
	// ErrorMessage 脱敏失败原因。
	ErrorMessage string
	// Revision 乐观锁版本。
	Revision int64
	// CreatedAt 创建时间。
	CreatedAt time.Time
	// UpdatedAt 更新时间。
	UpdatedAt time.Time
	// FinishedAt 终态时间。
	FinishedAt *time.Time
}

// TableName 指定计划表名。
func (planModel) TableName() string { return "plans" }

// planStepModel 对应 plan_steps 表：步骤依赖、工具参数与执行事实。
type planStepModel struct {
	// ID 步骤主键。
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// PlanID 所属计划。
	PlanID uuid.UUID `gorm:"type:uuid"`
	// UserID 所属用户。
	UserID uuid.UUID `gorm:"type:uuid"`
	// StepNo 步骤序号。
	StepNo int
	// Title 标题。
	Title string
	// Description 说明。
	Description string
	// DependsOn 依赖序号 JSON。
	DependsOn datatypes.JSON
	// ToolName 工具名。
	ToolName string
	// Arguments 工具参数 JSON。
	Arguments datatypes.JSON
	// IdempotencyKey 业务幂等键。
	IdempotencyKey string
	// Status 步骤状态。
	Status string
	// Attempts 已领取次数。
	Attempts int
	// MaxAttempts 最大尝试次数。
	MaxAttempts int
	// FenceToken 栅栏令牌。
	FenceToken int64
	// LeaseOwner 当前 Worker。
	LeaseOwner string
	// ExecutionStepID 关联执行步骤。
	ExecutionStepID *uuid.UUID `gorm:"type:uuid"`
	// Result 结果快照。
	Result datatypes.JSON `gorm:"column:result_json"`
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

// TableName 指定计划步骤表名。
func (planStepModel) TableName() string { return "plan_steps" }

// PlanRepository 是 plans 与 plan_steps 的 GORM 实现，使用 RLS 作用域强制用户隔离。
type PlanRepository struct {
	db *gorm.DB
}

// NewPlanRepository 构造计划仓库。
func NewPlanRepository(db *gorm.DB) *PlanRepository {
	return &PlanRepository{db: db}
}

// CreatePlan 在同一事务内创建初始计划、全部步骤与审计事件。
func (r *PlanRepository) CreatePlan(ctx context.Context, userID uuid.UUID, plan biz.Plan, event biz.OutboxEvent) (biz.Plan, error) {
	if plan.Version != 1 || plan.ParentPlanID != nil {
		return biz.Plan{}, fmt.Errorf("%w: only the initial plan can be created without replacing its parent", biz.ErrInvalidConfig)
	}
	return r.writePlan(ctx, userID, plan, event, nil)
}

// ReplacePlan 只有在锁定 Run 和最新父版本并验证状态后才发布后继版本。
func (r *PlanRepository) ReplacePlan(ctx context.Context, userID uuid.UUID, plan biz.Plan, parentRevision int64, fence biz.PlanFence, event biz.OutboxEvent) (biz.Plan, error) {
	if plan.Version <= 1 || plan.ParentPlanID == nil || plan.Status != biz.PlanExecuting ||
		(fence.Owner == "" && fence.Token != 0) || (fence.Owner != "" && fence.Token <= 0) {
		return biz.Plan{}, fmt.Errorf("%w: invalid plan replacement", biz.ErrInvalidConfig)
	}
	return r.writePlan(ctx, userID, plan, event, &planReplacement{revision: parentRevision, fence: fence})
}

type planReplacement struct {
	revision int64
	fence    biz.PlanFence
}

func (r *PlanRepository) writePlan(ctx context.Context, userID uuid.UUID, plan biz.Plan, event biz.OutboxEvent, replacement *planReplacement) (biz.Plan, error) {
	planRecord, err := fromBizPlan(plan)
	if err != nil {
		return biz.Plan{}, err
	}
	var steps []planStepModel
	if err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		if replacement == nil {
			if err := lockPlanUsageRun(tx, userID, plan.RunID); err != nil {
				return err
			}
			usage, err := sumModelCallUsage(tx, userID, plan.RunID)
			if err != nil {
				return err
			}
			planRecord.TotalInputTokens = max(planRecord.TotalInputTokens, usage.InputTokens)
			planRecord.TotalOutputTokens = max(planRecord.TotalOutputTokens, usage.OutputTokens)
			planRecord.EstimatedCost = max(planRecord.EstimatedCost, usage.EstimatedCost)
		} else {
			parent, err := supersedePlan(tx, userID, &planRecord, *replacement)
			if err != nil {
				return err
			}
			olderTools, err := completedAncestorTools(tx, userID, parent)
			if err != nil {
				return err
			}
			plan.Steps = biz.CarryCompletedPlanSteps(plan.Steps, parent, replacement.fence.Owner == "", olderTools...)
		}
		steps, err = fromBizPlanSteps(plan.ID, userID, plan.Steps)
		if err != nil {
			return err
		}
		if err := tx.Create(&planRecord).Error; err != nil {
			return err
		}
		if replacement == nil {
			// An interrupted planner call can settle after the plan is rebound to another Run.
			if err := tx.Model(&modelCallModel{}).
				Where("user_id = ? AND run_id = ? AND status = ? AND request_manifest ->> 'plan_usage' = 'true'", userID, plan.RunID, biz.ModelCallRunning).
				UpdateColumn("request_manifest", gorm.Expr("request_manifest || jsonb_build_object('plan_id', CAST(? AS text))", plan.ID.String())).Error; err != nil {
				return err
			}
		}
		if len(steps) > 0 {
			if err := tx.Create(&steps).Error; err != nil {
				return err
			}
		}
		return appendOutbox(tx, event)
	}); err != nil {
		return biz.Plan{}, catalogError("create plan", err)
	}
	return toBizPlan(planRecord, steps)
}

// completedAncestorTools keeps a settled operation reusable even when an intermediate
// revision omitted it. Rebinding to a new Run preserves the parent links.
func completedAncestorTools(tx *gorm.DB, userID uuid.UUID, parent biz.Plan) ([]biz.PlanStep, error) {
	if parent.ParentPlanID == nil {
		return nil, nil
	}
	var records []planStepModel
	if err := tx.Raw(`
WITH RECURSIVE ancestors(id, parent_plan_id, version) AS (
  SELECT id, parent_plan_id, version FROM plans WHERE id = ? AND user_id = ? AND session_id = ?
  UNION
  SELECT p.id, p.parent_plan_id, p.version FROM plans p JOIN ancestors a ON p.id = a.parent_plan_id
  WHERE p.user_id = ? AND p.session_id = ?
)
SELECT s.* FROM plan_steps s JOIN ancestors a ON a.id = s.plan_id
WHERE s.user_id = ? AND s.status = 'completed' AND s.tool_name <> ''
ORDER BY a.version DESC, s.step_no`, *parent.ParentPlanID, userID, parent.SessionID,
		userID, parent.SessionID, userID).Scan(&records).Error; err != nil {
		return nil, err
	}
	steps := make([]biz.PlanStep, 0, len(records))
	for _, record := range records {
		step, err := toBizPlanStep(record)
		if err != nil {
			return nil, err
		}
		steps = append(steps, step)
	}
	return steps, nil
}

func supersedePlan(tx *gorm.DB, userID uuid.UUID, next *planModel, replacement planReplacement) (biz.Plan, error) {
	now := time.Now().UTC()
	manual := replacement.fence.Owner == ""
	var run runModel
	if err := tx.Select("status", "deadline_at").Clauses(clause.Locking{Strength: "UPDATE"}).
		Take(&run, "id = ? AND user_id = ?", next.RunID, userID).Error; err != nil {
		return biz.Plan{}, err
	}
	if manual && run.Status != string(biz.RunQueued) && run.Status != string(biz.RunRunning) {
		return biz.Plan{}, fmt.Errorf("%w: run cannot accept a revised plan in its current state", biz.ErrResourceConflict)
	}
	var parent planModel
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("user_id = ? AND run_id = ?", userID, next.RunID).Order("version DESC").Take(&parent).Error; err != nil {
		return biz.Plan{}, err
	}
	if parent.ID != *next.ParentPlanID || parent.SessionID != next.SessionID || next.Version != parent.Version+1 {
		return biz.Plan{}, fmt.Errorf("%w: parent plan is no longer the latest version", biz.ErrPlanRevisionConflict)
	}
	status, target := biz.PlanExecuting, biz.PlanFailed
	code, message := "plan_replanned", "步骤失败，已生成新版本计划"
	if manual {
		status, target = biz.PlanPaused, biz.PlanCancelled
		code, message = "plan_superseded", "计划已由人工修订版本替代"
	} else {
		switch biz.PlanStatus(parent.Status) {
		case biz.PlanPaused:
			return biz.Plan{}, biz.ErrPlanPaused
		case biz.PlanCancelled:
			return biz.Plan{}, biz.ErrPlanCancelled
		}
	}
	if parent.Status != string(status) || parent.Revision != replacement.revision {
		return biz.Plan{}, fmt.Errorf("%w: parent plan status or revision changed", biz.ErrPlanRevisionConflict)
	}
	// Dispatch and approval claims lock Run then Plan. Lock step rows as well so
	// settlement cannot change the completed results while they are being carried forward.
	var steps []planStepModel
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("user_id = ? AND plan_id = ?", userID, parent.ID).Order("step_no").Find(&steps).Error; err != nil {
		return biz.Plan{}, err
	}
	if err := guardPlanReplacementEffects(tx, userID, parent); err != nil {
		if !manual {
			return biz.Plan{}, planReplacementConflict(err)
		}
		return biz.Plan{}, err
	}
	if manual {
		if err := extendRevisedPlanDeadline(tx, userID, &parent, &run, now); err != nil {
			return biz.Plan{}, err
		}
		next.DeadlineAt = parent.DeadlineAt
	} else if _, err := lockRunningRunLease(tx, userID, next.RunID, biz.RunFence(replacement.fence), now); err != nil {
		return biz.Plan{}, planReplacementConflict(err)
	}
	next.TotalInputTokens = parent.TotalInputTokens
	next.TotalOutputTokens = parent.TotalOutputTokens
	next.EstimatedCost = parent.EstimatedCost
	result := tx.Model(&planModel{}).
		Where("id = ? AND user_id = ? AND status = ? AND revision = ?", parent.ID, userID, status, replacement.revision).Updates(map[string]any{
		"status": target, "revision": gorm.Expr("revision + 1"), "error_code": code, "error_message": message,
		"deadline_at": parent.DeadlineAt, "updated_at": now, "finished_at": now,
	})
	if result.Error != nil {
		return biz.Plan{}, result.Error
	}
	if result.RowsAffected != 1 {
		return biz.Plan{}, biz.ErrPlanRevisionConflict
	}
	return toBizPlan(parent, steps)
}

func planReplacementConflict(err error) error {
	if errors.Is(err, biz.ErrResourceConflict) {
		return fmt.Errorf("%w: %w", biz.ErrPlanRevisionConflict, err)
	}
	return err
}

func guardPlanReplacementEffects(tx *gorm.DB, userID uuid.UUID, parent planModel) error {
	var unsettled bool
	if err := tx.Raw(`
SELECT EXISTS (
  SELECT 1 FROM plan_steps AS step
  WHERE step.user_id = ? AND step.plan_id = ? AND (
    step.status = 'unknown'
    OR (step.status = 'running' AND (step.tool_name <> '' OR EXISTS (
      SELECT 1 FROM execution_steps AS tool
      WHERE tool.user_id = step.user_id AND tool.run_id = ? AND tool.parent_step_id = step.execution_step_id
        AND tool.kind = 'tool' AND tool.status IN ('running', 'completed', 'reconciliation_required')
    )))
    OR (step.status IN ('running', 'waiting_approval') AND EXISTS (
      SELECT 1 FROM tool_approvals AS approval
      WHERE approval.user_id = step.user_id AND approval.run_id = ?
        AND (approval.tool_call_id = 'plan-step-' || step.id::text OR approval.step_id = step.execution_step_id)
        AND approval.status IN ('pending', 'approved', 'executing', 'executed', 'failed')
    ))
  )
)`, userID, parent.ID, parent.RunID, parent.RunID).Scan(&unsettled).Error; err != nil {
		return err
	}
	if unsettled {
		return fmt.Errorf("%w: settle the current plan's tool effects before replacing it", biz.ErrResourceConflict)
	}
	return nil
}

func extendRevisedPlanDeadline(tx *gorm.DB, userID uuid.UUID, parent *planModel, run *runModel, now time.Time) error {
	pausedFor := now.Sub(parent.UpdatedAt)
	if pausedFor <= 0 {
		return nil
	}
	if parent.DeadlineAt != nil {
		deadline := parent.DeadlineAt.Add(pausedFor)
		parent.DeadlineAt = &deadline
	}
	base := now
	if run.DeadlineAt != nil {
		base = *run.DeadlineAt
	}
	return tx.Model(&runModel{}).Where("id = ? AND user_id = ?", parent.RunID, userID).
		Updates(map[string]any{"deadline_at": base.Add(pausedFor), "updated_at": now}).Error
}

// GetPlanByRun 读取 Run 的最新版本计划及步骤。
func (r *PlanRepository) GetPlanByRun(ctx context.Context, userID, runID uuid.UUID) (biz.Plan, error) {
	var planRecord planModel
	var steps []planStepModel
	if err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		if err := tx.Where("user_id = ? AND run_id = ?", userID, runID).Order("version DESC").First(&planRecord).Error; err != nil {
			return err
		}
		return tx.Where("user_id = ? AND plan_id = ?", userID, planRecord.ID).Order("step_no").Find(&steps).Error
	}); err != nil {
		return biz.Plan{}, catalogError("get plan by run", err)
	}
	return toBizPlan(planRecord, steps)
}

// RecoverPlanUsage reconciles the latest Plan with retained model calls, including
// calls written by older workers and by Runs from which the Plan was resumed.
func (r *PlanRepository) RecoverPlanUsage(ctx context.Context, userID, runID uuid.UUID) (biz.ModelCallUsage, error) {
	var usage biz.ModelCallUsage
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		if err := lockPlanUsageRun(tx, userID, runID); err != nil {
			return err
		}
		var plan planModel
		query := tx.Select("id", "total_input_tokens", "total_output_tokens", "estimated_cost").
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("user_id = ? AND run_id = ?", userID, runID).Order("version DESC").Limit(1)
		if err := query.Find(&plan).Error; err != nil {
			return err
		}
		if err := tx.Raw(`
WITH RECURSIVE plan_runs(id) AS (
  SELECT ?::uuid
  UNION
  SELECT previous.id FROM plan_runs current
  JOIN run_events e ON e.run_id = current.id AND e.user_id = ? AND e.event_type = 'plan.resumed'
  JOIN agent_runs previous ON previous.id::text = e.payload ->> 'previous_run_id' AND previous.user_id = ?
)
SELECT COUNT(*) AS count, COALESCE(SUM(c.input_tokens), 0) AS input_tokens,
       COALESCE(SUM(c.output_tokens), 0) AS output_tokens, COALESCE(SUM(c.estimated_cost), 0) AS estimated_cost
FROM model_calls c JOIN plan_runs ON plan_runs.id = c.run_id WHERE c.user_id = ?`,
			runID, userID, userID, userID).Scan(&usage).Error; err != nil {
			return err
		}
		// Some older attempts may have been pruned. Never decrease a stored total.
		usage.InputTokens = max(usage.InputTokens, plan.TotalInputTokens)
		usage.OutputTokens = max(usage.OutputTokens, plan.TotalOutputTokens)
		usage.EstimatedCost = max(usage.EstimatedCost, plan.EstimatedCost)
		if plan.ID == uuid.Nil || (usage.InputTokens == plan.TotalInputTokens &&
			usage.OutputTokens == plan.TotalOutputTokens && usage.EstimatedCost == plan.EstimatedCost) {
			return nil
		}
		return tx.Model(&planModel{}).Where("id = ? AND user_id = ?", plan.ID, userID).UpdateColumns(map[string]any{
			"total_input_tokens": usage.InputTokens, "total_output_tokens": usage.OutputTokens, "estimated_cost": usage.EstimatedCost,
		}).Error
	})
	return usage, catalogError("recover plan usage", err)
}

// ExtendPlanDeadline 延长非终态计划的截止时间，不递增 revision，避免恢复路径产生额外冲突。
func (r *PlanRepository) ExtendPlanDeadline(ctx context.Context, userID, planID uuid.UUID, deadline time.Time, now time.Time) (biz.Plan, error) {
	if userID == uuid.Nil || planID == uuid.Nil || deadline.IsZero() {
		return biz.Plan{}, fmt.Errorf("%w: plan identity and deadline are required", biz.ErrInvalidConfig)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		result := tx.Model(&planModel{}).
			Where("id = ? AND user_id = ? AND status IN ?", planID, userID,
				[]string{string(biz.PlanPlanning), string(biz.PlanExecuting), string(biz.PlanWaitingApproval), string(biz.PlanPaused)}).
			Updates(map[string]any{"deadline_at": deadline, "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("%w: plan deadline cannot be extended in its current state", biz.ErrResourceConflict)
		}
		return nil
	})
	if err != nil {
		return biz.Plan{}, catalogError("extend plan deadline", err)
	}
	return r.GetPlan(ctx, userID, planID)
}

// GetPlan 按 ID 读取计划及步骤。
func (r *PlanRepository) GetPlan(ctx context.Context, userID, planID uuid.UUID) (biz.Plan, error) {
	var planRecord planModel
	var steps []planStepModel
	if err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		if err := tx.Where("user_id = ? AND id = ?", userID, planID).First(&planRecord).Error; err != nil {
			return err
		}
		return tx.Where("user_id = ? AND plan_id = ?", userID, planID).Order("step_no").Find(&steps).Error
	}); err != nil {
		return biz.Plan{}, catalogError("get plan", err)
	}
	return toBizPlan(planRecord, steps)
}

// ListPlanVersions 列出 Run 的全部计划版本，不加载步骤。
func (r *PlanRepository) ListPlanVersions(ctx context.Context, userID, runID uuid.UUID) ([]biz.Plan, error) {
	var records []planModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Where("user_id = ? AND run_id = ?", userID, runID).Order("version DESC").Find(&records).Error
	})
	if err != nil {
		return nil, catalogError("list plan versions", err)
	}
	plans := make([]biz.Plan, 0, len(records))
	for _, record := range records {
		plan, err := toBizPlan(record, nil)
		if err != nil {
			return nil, err
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

// UpdatePlan 以 revision 与 from 状态为前置条件推进计划；fence 非空时校验 Run 租约。
func (r *PlanRepository) UpdatePlan(ctx context.Context, userID, planID uuid.UUID, from, to biz.PlanStatus, expectedRevision int64, fence biz.PlanFence, update biz.PlanUpdate) (biz.Plan, error) {
	// 仓储层重复校验状态机，防止绕过领域层把终态写回。
	if err := biz.TransitionPlanStatus(from, to); err != nil {
		return biz.Plan{}, err
	}
	now := time.Now().UTC()
	// 使用表达式累加 Token 与费用，避免并发更新覆盖累计值。
	fields := map[string]any{
		"status": string(to), "revision": gorm.Expr("revision + 1"), "updated_at": now,
		"error_code": update.ErrorCode, "error_message": update.ErrorMessage,
	}
	if update.InputTokensDelta != 0 {
		fields["total_input_tokens"] = gorm.Expr("total_input_tokens + ?", update.InputTokensDelta)
	}
	if update.OutputTokensDelta != 0 {
		fields["total_output_tokens"] = gorm.Expr("total_output_tokens + ?", update.OutputTokensDelta)
	}
	if update.EstimatedCostDelta != 0 {
		fields["estimated_cost"] = gorm.Expr("estimated_cost + ?", update.EstimatedCostDelta)
	}
	if update.FinishedAt != nil {
		fields["finished_at"] = update.FinishedAt.UTC()
	}
	var result *gorm.DB
	if err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		if fence.Owner != "" {
			if _, err := lockWorkerPlan(tx, userID, planID, fence, now); err != nil {
				return planReplacementConflict(err)
			}
		}
		result = tx.Model(&planModel{}).
			Where("id = ? AND user_id = ? AND status = ? AND revision = ?", planID, userID, string(from), expectedRevision).
			Updates(fields)
		return result.Error
	}); err != nil {
		return biz.Plan{}, catalogError("update plan", err)
	}
	if result.RowsAffected != 1 {
		return biz.Plan{}, fmt.Errorf("%w: plan revision or status changed", biz.ErrPlanRevisionConflict)
	}
	return r.GetPlan(ctx, userID, planID)
}

// lockWorkerPlan serializes a worker mutation with Run takeover, pause and rebinding.
// Keep the lock order consistent with plan claims, replacement and tool dispatch.
func lockWorkerPlan(tx *gorm.DB, userID, planID uuid.UUID, fence biz.PlanFence, now time.Time) (biz.PlanStatus, error) {
	var plan planModel
	if err := tx.Select("run_id").Take(&plan, "id = ? AND user_id = ?", planID, userID).Error; err != nil {
		return "", err
	}
	if _, err := lockRunningRunLease(tx, userID, plan.RunID, biz.RunFence(fence), now); err != nil {
		return "", err
	}
	err := tx.Model(&planModel{}).Select("status").Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND user_id = ? AND run_id = ?", planID, userID, plan.RunID).Take(&plan).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", fmt.Errorf("%w: plan was rebound", biz.ErrResourceConflict)
	}
	return biz.PlanStatus(plan.Status), err
}

// FindPausedPlanBySession 读取会话中最近一个暂停中的计划，用于新消息自动续跑。
func (r *PlanRepository) FindPausedPlanBySession(ctx context.Context, userID, sessionID uuid.UUID) (biz.Plan, error) {
	var planRecord planModel
	if err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Where("user_id = ? AND session_id = ? AND status = ?", userID, sessionID, string(biz.PlanPaused)).
			Order("created_at DESC").First(&planRecord).Error
	}); err != nil {
		return biz.Plan{}, catalogError("find paused plan by session", err)
	}
	return r.GetPlan(ctx, userID, planRecord.ID)
}

// RebindPlanRun 把暂停中的计划迁移到新的 Run 并恢复执行，保留已完成步骤。
// 已领取的工具调用保留对账证据，纯模型与未执行的审批步骤可以重跑。
func (r *PlanRepository) RebindPlanRun(ctx context.Context, userID, planID, runID uuid.UUID, expectedRevision int64, deadlineAt *time.Time, now time.Time) (biz.Plan, error) {
	var result *gorm.DB
	var oldRunID uuid.UUID
	if err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		var planRecord planModel
		if err := tx.Select("run_id").Where("id = ? AND user_id = ?", planID, userID).First(&planRecord).Error; err != nil {
			return err
		}
		oldRunID = planRecord.RunID
		if err := lockPlanUsageRun(tx, userID, oldRunID); err != nil {
			return err
		}
		result = tx.Model(&planModel{}).
			Where("id = ? AND user_id = ? AND status = ? AND revision = ?", planID, userID, string(biz.PlanPaused), expectedRevision).
			Updates(map[string]any{
				"run_id": runID, "status": string(biz.PlanExecuting),
				"deadline_at": deadlineAt, "revision": gorm.Expr("revision + 1"),
				"error_code": "", "error_message": "", "updated_at": now.UTC(),
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return nil
		}
		if err := tx.Exec(`
UPDATE plan_steps AS step
SET status = 'completed', result_json = jsonb_build_object('content', COALESCE(approval.result_json::text, '')),
    lease_owner = '', fence_token = 0, updated_at = ?, finished_at = approval.updated_at,
    error_code = '', error_message = ''
FROM tool_approvals AS approval
WHERE step.plan_id = ? AND step.user_id = ? AND step.status = 'waiting_approval'
  AND approval.user_id = step.user_id AND approval.run_id = ?
  AND approval.tool_call_id = 'plan-step-' || step.id::text AND approval.status = 'executed'`,
			now.UTC(), planID, userID, oldRunID).Error; err != nil {
			return err
		}
		// 保留旧 Run 的执行步骤关联，供新 Run 与人工对账时定位工具记录。
		if err := tx.Exec(`
UPDATE plan_steps AS step
SET status = 'unknown', lease_owner = '', fence_token = 0, updated_at = ?, finished_at = ?,
    error_code = 'tool_execution_unknown', error_message = '旧执行轮次的工具副作用需要人工对账'
WHERE step.plan_id = ? AND step.user_id = ? AND (
  (step.status = 'running' AND step.tool_name = '' AND EXISTS (
    SELECT 1 FROM execution_steps AS tool
    WHERE tool.user_id = step.user_id AND tool.run_id = ? AND tool.parent_step_id = step.execution_step_id
      AND tool.kind = 'tool' AND tool.status IN ('running', 'completed', 'reconciliation_required')
  )) OR (step.status = 'waiting_approval' AND EXISTS (
    SELECT 1 FROM tool_approvals AS approval
    WHERE approval.user_id = step.user_id AND approval.run_id = ?
      AND approval.tool_call_id = 'plan-step-' || step.id::text
      AND approval.status IN ('executing', 'executed', 'failed')
  ))
)`, now.UTC(), now.UTC(), planID, userID, oldRunID, oldRunID).Error; err != nil {
			return err
		}
		// 仅重置没有已领取工具调用的步骤。独立工具步骤的 running 状态由 Worker 对账。
		if err := tx.Model(&planStepModel{}).
			Where("plan_id = ? AND user_id = ?", planID, userID).
			Where("(status = ? AND tool_name = '') OR status = ?", biz.PlanStepRunning, biz.PlanStepWaitingApproval).
			Updates(map[string]any{
				"status": string(biz.PlanStepPending), "lease_owner": "", "fence_token": int64(0),
				"attempts": 0, "execution_step_id": nil, "started_at": nil, "finished_at": nil,
				"updated_at": now.UTC(), "error_code": "", "error_message": "",
			}).Error; err != nil {
			return err
		}
		// 旧 Run 上未决策的审批作废：计划已迁走，投递到旧 Run 的结果不会被任何执行器消费。
		if oldRunID != uuid.Nil {
			if err := tx.Model(&toolApprovalModel{}).
				Where("user_id = ? AND run_id = ? AND status = ?", userID, oldRunID, string(biz.ApprovalPending)).
				Updates(map[string]any{
					"status": string(biz.ApprovalDenied), "decided_at": now.UTC(),
					"error_code": "plan_rebound", "error_message": "计划已迁移到新的执行轮次，旧审批作废",
					"updated_at": now.UTC(),
				}).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return biz.Plan{}, catalogError("rebind plan run", err)
	}
	if result == nil || result.RowsAffected != 1 {
		return biz.Plan{}, fmt.Errorf("%w: plan revision or status changed", biz.ErrPlanRevisionConflict)
	}
	return r.GetPlan(ctx, userID, planID)
}

func lockLatestRunPlan(tx *gorm.DB, userID, runID uuid.UUID) (planModel, error) {
	var plan planModel
	err := tx.Select("id", "status").Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("user_id = ? AND run_id = ?", userID, runID).Order("version DESC").Take(&plan).Error
	return plan, err
}

func lockExecutingPlan(tx *gorm.DB, userID, runID uuid.UUID, fence biz.PlanFence, now time.Time) (uuid.UUID, error) {
	if _, err := lockRunningRunLease(tx, userID, runID, biz.RunFence(fence), now); err != nil {
		return uuid.Nil, err
	}
	plan, err := lockLatestRunPlan(tx, userID, runID)
	if err != nil {
		return uuid.Nil, err
	}
	switch biz.PlanStatus(plan.Status) {
	case biz.PlanExecuting:
		return plan.ID, nil
	case biz.PlanPaused:
		return uuid.Nil, biz.ErrPlanPaused
	case biz.PlanCancelled:
		return uuid.Nil, biz.ErrPlanCancelled
	default:
		return uuid.Nil, fmt.Errorf("%w: plan is not executing", biz.ErrResourceConflict)
	}
}

// AddPlanUsage 累加计划的 Token 与费用，revision 递增用于展示最新事实。
func (r *PlanRepository) AddPlanUsage(ctx context.Context, userID, planID uuid.UUID, inputTokens, outputTokens int64, estimatedCost float64, now time.Time) error {
	if inputTokens == 0 && outputTokens == 0 && estimatedCost == 0 {
		return nil
	}
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		result := tx.Model(&planModel{}).Where("id = ? AND user_id = ?", planID, userID).Updates(map[string]any{
			"total_input_tokens":  gorm.Expr("total_input_tokens + ?", inputTokens),
			"total_output_tokens": gorm.Expr("total_output_tokens + ?", outputTokens),
			"estimated_cost":      gorm.Expr("estimated_cost + ?", estimatedCost),
			"revision":            gorm.Expr("revision + 1"),
			"updated_at":          now.UTC(),
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("%w: plan not found", biz.ErrResourceNotFound)
		}
		return nil
	})
	if err != nil {
		return catalogError("add plan usage", err)
	}
	return nil
}

// ClaimPlanStep 领取待执行或可重试步骤，写入 attempts、fencing、执行步骤与开始时间。
func (r *PlanRepository) ClaimPlanStep(ctx context.Context, userID, planID, stepID uuid.UUID, executionStepID *uuid.UUID, fence biz.PlanFence, now time.Time) (biz.PlanStep, error) {
	var record planStepModel
	if err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		var plan planModel
		if err := tx.Select("run_id").Take(&plan, "id = ? AND user_id = ?", planID, userID).Error; err != nil {
			return err
		}
		activeID, err := lockExecutingPlan(tx, userID, plan.RunID, fence, now)
		if err != nil {
			return err
		}
		if activeID != planID {
			return fmt.Errorf("%w: plan version changed", biz.ErrResourceConflict)
		}
		return tx.Raw(`
UPDATE plan_steps
SET status = 'running', attempts = attempts + 1, fence_token = ?, lease_owner = ?,
    execution_step_id = COALESCE(?, execution_step_id),
    started_at = COALESCE(started_at, ?), finished_at = NULL, updated_at = ?, error_code = '', error_message = ''
WHERE id = ? AND user_id = ? AND plan_id = ?
  AND status IN ('pending', 'failed') AND attempts < max_attempts
RETURNING *`, fence.Token, fence.Owner, executionStepID, now.UTC(), now.UTC(), stepID, userID, planID).Scan(&record).Error
	}); err != nil {
		return biz.PlanStep{}, catalogError("claim plan step", err)
	}
	if record.ID == uuid.Nil {
		return biz.PlanStep{}, fmt.Errorf("%w: plan step is not claimable", biz.ErrResourceConflict)
	}
	return toBizPlanStep(record)
}

// UpdatePlanStep 以 running、fence_token 与 lease_owner 为前置条件推进步骤。
func (r *PlanRepository) UpdatePlanStep(ctx context.Context, userID, planID, stepID uuid.UUID, fence biz.PlanFence, update biz.PlanStepUpdate) (biz.PlanStep, error) {
	// 仓储层校验 running -> 目标状态，阻止终态被迟到事件回写。
	if err := biz.TransitionPlanStepStatus(biz.PlanStepRunning, update.Status); err != nil {
		return biz.PlanStep{}, err
	}
	now := time.Now().UTC()
	resultJSON, err := encodeOptionalJSON(update.Result)
	if err != nil {
		return biz.PlanStep{}, err
	}
	finishedAt := update.FinishedAt
	if finishedAt == nil && biz.PlanStepTerminal(update.Status) {
		finishedAt = &now
	}
	var record planStepModel
	if err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		status, err := lockWorkerPlan(tx, userID, planID, fence, now)
		if err != nil {
			return err
		}
		if biz.PlanTerminal(status) {
			return fmt.Errorf("%w: plan no longer accepts step settlement", biz.ErrResourceConflict)
		}
		return tx.Raw(`
UPDATE plan_steps
SET status = ?, result_json = ?, error_code = ?, error_message = ?,
    execution_step_id = COALESCE(?, execution_step_id),
    finished_at = ?, updated_at = ?, lease_owner = '', fence_token = 0
WHERE id = ? AND user_id = ? AND plan_id = ?
  AND status = 'running' AND lease_owner = ? AND fence_token = ?
RETURNING *`, string(update.Status), resultJSON, update.ErrorCode, update.ErrorMessage,
			update.ExecutionStepID, finishedAt, now, stepID, userID, planID, fence.Owner, fence.Token).Scan(&record).Error
	}); err != nil {
		return biz.PlanStep{}, catalogError("update plan step", err)
	}
	if record.ID == uuid.Nil {
		return biz.PlanStep{}, fmt.Errorf("%w: stale plan step fence", biz.ErrResourceConflict)
	}
	return toBizPlanStep(record)
}

// ResolvePlanStep 以当前状态为前置条件推进等待审批或待执行步骤。
func (r *PlanRepository) ResolvePlanStep(ctx context.Context, userID, planID, stepID uuid.UUID, from biz.PlanStepStatus, fence biz.PlanFence, update biz.PlanStepUpdate) (biz.PlanStep, error) {
	if err := biz.TransitionPlanStepStatus(from, update.Status); err != nil {
		return biz.PlanStep{}, err
	}
	now := time.Now().UTC()
	resultJSON, err := encodeOptionalJSON(update.Result)
	if err != nil {
		return biz.PlanStep{}, err
	}
	finishedAt := update.FinishedAt
	if finishedAt == nil && biz.PlanStepTerminal(update.Status) {
		finishedAt = &now
	}
	var record planStepModel
	if err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		status, err := lockWorkerPlan(tx, userID, planID, fence, now)
		if err != nil {
			return err
		}
		if biz.PlanTerminal(status) && (status != biz.PlanFailed || update.Status != biz.PlanStepSkipped) {
			return fmt.Errorf("%w: plan no longer accepts step resolution", biz.ErrResourceConflict)
		}
		return tx.Raw(`
UPDATE plan_steps
SET status = ?, result_json = ?, error_code = ?, error_message = ?,
    execution_step_id = COALESCE(?, execution_step_id),
    finished_at = ?, updated_at = ?, lease_owner = '', fence_token = 0
WHERE id = ? AND user_id = ? AND plan_id = ? AND status = ?
RETURNING *`, string(update.Status), resultJSON, update.ErrorCode, update.ErrorMessage,
			update.ExecutionStepID, finishedAt, now, stepID, userID, planID, string(from)).Scan(&record).Error
	}); err != nil {
		return biz.PlanStep{}, catalogError("resolve plan step", err)
	}
	if record.ID == uuid.Nil {
		return biz.PlanStep{}, fmt.Errorf("%w: plan step status changed", biz.ErrResourceConflict)
	}
	return toBizPlanStep(record)
}

// fromBizPlan 将领域计划转换为数据库行。
func fromBizPlan(plan biz.Plan) (planModel, error) {
	snapshot, err := json.Marshal(nonNilMap(plan.ConfigSnapshot))
	if err != nil {
		return planModel{}, err
	}
	return planModel{
		ID: plan.ID, UserID: plan.UserID, RunID: plan.RunID, SessionID: plan.SessionID,
		Version: plan.Version, ParentPlanID: plan.ParentPlanID, Status: string(plan.Status), Goal: plan.Goal,
		ConfigSnapshot: datatypes.JSON(snapshot), MaxSteps: plan.MaxSteps,
		TotalInputTokens: plan.TotalInputTokens, TotalOutputTokens: plan.TotalOutputTokens, EstimatedCost: plan.EstimatedCost,
		DeadlineAt: plan.DeadlineAt, ErrorCode: plan.ErrorCode, ErrorMessage: plan.ErrorMessage, Revision: plan.Revision,
		CreatedAt: plan.CreatedAt, UpdatedAt: plan.UpdatedAt, FinishedAt: plan.FinishedAt,
	}, nil
}

// fromBizPlanSteps 将领域步骤转换为数据库行。
func fromBizPlanSteps(planID, userID uuid.UUID, steps []biz.PlanStep) ([]planStepModel, error) {
	records := make([]planStepModel, 0, len(steps))
	for _, step := range steps {
		dependsOn, err := json.Marshal(nonNilInts(step.DependsOn))
		if err != nil {
			return nil, err
		}
		arguments, err := json.Marshal(nonNilMap(step.Arguments))
		if err != nil {
			return nil, err
		}
		resultJSON, err := encodeOptionalJSON(step.Result)
		if err != nil {
			return nil, err
		}
		records = append(records, planStepModel{
			ID: step.ID, PlanID: planID, UserID: userID, StepNo: step.StepNo, Title: step.Title,
			Description: step.Description, DependsOn: datatypes.JSON(dependsOn), ToolName: step.ToolName,
			Arguments: datatypes.JSON(arguments), IdempotencyKey: step.IdempotencyKey, Status: string(step.Status),
			Attempts: step.Attempts, MaxAttempts: step.MaxAttempts, FenceToken: step.FenceToken, LeaseOwner: step.LeaseOwner,
			ExecutionStepID: step.ExecutionStepID, Result: resultJSON, ErrorCode: step.ErrorCode, ErrorMessage: step.ErrorMessage,
			StartedAt: step.StartedAt, FinishedAt: step.FinishedAt, CreatedAt: step.CreatedAt, UpdatedAt: step.UpdatedAt,
		})
	}
	return records, nil
}

// toBizPlan 将计划行与步骤行转换为领域对象。
func toBizPlan(record planModel, steps []planStepModel) (biz.Plan, error) {
	var snapshot map[string]any
	if len(record.ConfigSnapshot) > 0 {
		if err := json.Unmarshal(record.ConfigSnapshot, &snapshot); err != nil {
			return biz.Plan{}, fmt.Errorf("decode plan snapshot: %w", err)
		}
	}
	plan := biz.Plan{
		ID: record.ID, UserID: record.UserID, RunID: record.RunID, SessionID: record.SessionID,
		Version: record.Version, ParentPlanID: record.ParentPlanID, Status: biz.PlanStatus(record.Status), Goal: record.Goal,
		ConfigSnapshot: snapshot, MaxSteps: record.MaxSteps,
		TotalInputTokens: record.TotalInputTokens, TotalOutputTokens: record.TotalOutputTokens, EstimatedCost: record.EstimatedCost,
		DeadlineAt: record.DeadlineAt, ErrorCode: record.ErrorCode, ErrorMessage: record.ErrorMessage, Revision: record.Revision,
		CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt, FinishedAt: record.FinishedAt,
	}
	if len(steps) > 0 {
		plan.Steps = make([]biz.PlanStep, 0, len(steps))
		for _, step := range steps {
			converted, err := toBizPlanStep(step)
			if err != nil {
				return biz.Plan{}, err
			}
			plan.Steps = append(plan.Steps, converted)
		}
	}
	return plan, nil
}

// toBizPlanStep 将步骤行转换为领域对象，解析依赖、参数与结果 JSON。
func toBizPlanStep(record planStepModel) (biz.PlanStep, error) {
	var dependsOn []int
	if len(record.DependsOn) > 0 {
		if err := json.Unmarshal(record.DependsOn, &dependsOn); err != nil {
			return biz.PlanStep{}, fmt.Errorf("decode plan dependencies: %w", err)
		}
	}
	var arguments map[string]any
	if len(record.Arguments) > 0 {
		if err := json.Unmarshal(record.Arguments, &arguments); err != nil {
			return biz.PlanStep{}, fmt.Errorf("decode plan arguments: %w", err)
		}
	}
	var result map[string]any
	if len(record.Result) > 0 {
		if err := json.Unmarshal(record.Result, &result); err != nil {
			return biz.PlanStep{}, fmt.Errorf("decode plan result: %w", err)
		}
	}
	return biz.PlanStep{
		ID: record.ID, PlanID: record.PlanID, UserID: record.UserID, StepNo: record.StepNo,
		Title: record.Title, Description: record.Description, DependsOn: dependsOn, ToolName: record.ToolName,
		Arguments: arguments, IdempotencyKey: record.IdempotencyKey, Status: biz.PlanStepStatus(record.Status),
		Attempts: record.Attempts, MaxAttempts: record.MaxAttempts, FenceToken: record.FenceToken, LeaseOwner: record.LeaseOwner,
		ExecutionStepID: record.ExecutionStepID, Result: result, ErrorCode: record.ErrorCode, ErrorMessage: record.ErrorMessage,
		StartedAt: record.StartedAt, FinishedAt: record.FinishedAt, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
	}, nil
}

// encodeOptionalJSON 编码可选结果 JSON，nil 返回 nil 以便数据库存 NULL。
func encodeOptionalJSON(value map[string]any) (datatypes.JSON, error) {
	if value == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return datatypes.JSON(encoded), nil
}

// nonNilInts 保证依赖数组编码为 JSON 数组而不是 null。
func nonNilInts(values []int) []int {
	if values == nil {
		return []int{}
	}
	return values
}

// 编译期断言接口实现，避免装配阶段才发现契约不一致。
var _ biz.PlanRepository = (*PlanRepository)(nil)
