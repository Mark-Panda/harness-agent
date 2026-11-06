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

// CreatePlan 在同一事务内创建计划、全部步骤与审计事件。
func (r *PlanRepository) CreatePlan(ctx context.Context, userID uuid.UUID, plan biz.Plan, event biz.OutboxEvent) (biz.Plan, error) {
	planRecord, err := fromBizPlan(plan)
	if err != nil {
		return biz.Plan{}, err
	}
	steps, err := fromBizPlanSteps(plan.ID, userID, plan.Steps)
	if err != nil {
		return biz.Plan{}, err
	}
	if err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		if err := tx.Create(&planRecord).Error; err != nil {
			return err
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
		query := tx.Model(&planModel{}).
			Where("id = ? AND user_id = ? AND status = ? AND revision = ?", planID, userID, string(from), expectedRevision)
		if fence.Owner != "" {
			query = query.Where("EXISTS (SELECT 1 FROM agent_runs r WHERE r.id = plans.run_id AND r.user_id = ? AND r.lease_owner = ? AND r.fencing_token = ? AND r.status IN ('running', 'waiting_approval'))",
				userID, fence.Owner, fence.Token)
		}
		result = query.Updates(fields)
		return result.Error
	}); err != nil {
		return biz.Plan{}, catalogError("update plan", err)
	}
	if result.RowsAffected != 1 {
		return biz.Plan{}, fmt.Errorf("%w: plan revision or status changed", biz.ErrPlanRevisionConflict)
	}
	return r.GetPlan(ctx, userID, planID)
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
// 迁移时：无副作用的模型步骤与尚未执行的审批步骤退回 pending；仍在 running 的
// 工具步骤保留原状，交给 recoverPlanSteps 按“副作用未知”处理；旧 Run 上未决策的
// 审批作废，避免审批列表残留无法投递的请求。
func (r *PlanRepository) RebindPlanRun(ctx context.Context, userID, planID, runID uuid.UUID, expectedRevision int64, deadlineAt *time.Time, now time.Time) (biz.Plan, error) {
	var result *gorm.DB
	var oldRunID uuid.UUID
	if err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		var planRecord planModel
		if err := tx.Select("run_id").Where("id = ? AND user_id = ?", planID, userID).First(&planRecord).Error; err != nil {
			return err
		}
		oldRunID = planRecord.RunID
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
		// 模型步骤没有外部副作用，可以安全重跑。
		if err := tx.Model(&planStepModel{}).
			Where("plan_id = ? AND user_id = ? AND status = ? AND tool_name = ''", planID, userID, string(biz.PlanStepRunning)).
			Updates(map[string]any{
				"status": string(biz.PlanStepPending), "lease_owner": "", "fence_token": int64(0),
				// 中断不是业务失败，重置尝试次数，避免达到 MaxAttempts 后新 Run 永远领不到该步骤。
				"attempts": 0, "started_at": nil, "finished_at": nil, "updated_at": now.UTC(),
				"error_code": "", "error_message": "",
			}).Error; err != nil {
			return err
		}
		// 等待审批的步骤尚未执行，退回 pending 让新 Run 重新发起审批。
		if err := tx.Model(&planStepModel{}).
			Where("plan_id = ? AND user_id = ? AND status = ?", planID, userID, string(biz.PlanStepWaitingApproval)).
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
		return tx.Raw(`
UPDATE plan_steps
SET status = 'running', attempts = attempts + 1, fence_token = ?, lease_owner = ?,
    execution_step_id = COALESCE(?, execution_step_id),
    started_at = COALESCE(started_at, ?), finished_at = NULL, updated_at = ?, error_code = '', error_message = ''
WHERE id = ? AND user_id = ? AND plan_id = ?
  AND status IN ('pending', 'failed') AND attempts < max_attempts
  AND EXISTS (SELECT 1 FROM plans p JOIN agent_runs r ON r.id = p.run_id
              WHERE p.id = plan_steps.plan_id AND p.user_id = ? AND r.user_id = ?
                AND r.lease_owner = ? AND r.fencing_token = ? AND r.status IN ('running', 'waiting_approval'))
RETURNING *`, fence.Token, fence.Owner, executionStepID, now.UTC(), now.UTC(), stepID, userID, planID,
			userID, userID, fence.Owner, fence.Token).Scan(&record).Error
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
		return tx.Raw(`
UPDATE plan_steps
SET status = ?, result_json = ?, error_code = ?, error_message = ?,
    execution_step_id = COALESCE(?, execution_step_id),
    finished_at = ?, updated_at = ?, lease_owner = '', fence_token = 0
WHERE id = ? AND user_id = ? AND plan_id = ?
  AND status = 'running' AND lease_owner = ? AND fence_token = ?
  AND EXISTS (SELECT 1 FROM plans p JOIN agent_runs r ON r.id = p.run_id
              WHERE p.id = plan_steps.plan_id AND p.user_id = ? AND r.user_id = ?
                AND r.lease_owner = ? AND r.fencing_token = ? AND r.status IN ('running', 'waiting_approval'))
RETURNING *`, string(update.Status), resultJSON, update.ErrorCode, update.ErrorMessage,
			update.ExecutionStepID, finishedAt, now, stepID, userID, planID, fence.Owner, fence.Token,
			userID, userID, fence.Owner, fence.Token).Scan(&record).Error
	}); err != nil {
		return biz.PlanStep{}, catalogError("update plan step", err)
	}
	if record.ID == uuid.Nil {
		return biz.PlanStep{}, fmt.Errorf("%w: stale plan step fence", biz.ErrResourceConflict)
	}
	return toBizPlanStep(record)
}

// ResolvePlanStep 以当前状态为前置条件推进等待审批或待执行步骤。
func (r *PlanRepository) ResolvePlanStep(ctx context.Context, userID, planID, stepID uuid.UUID, from biz.PlanStepStatus, update biz.PlanStepUpdate) (biz.PlanStep, error) {
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
