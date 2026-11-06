package biz

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// PlanService 提供计划查询、暂停、恢复与人工终止用例。
type PlanService struct {
	plans PlanRepository
	runs  *RunService
	clock func() time.Time
}

// NewPlanService 构造计划应用服务。
func NewPlanService(plans PlanRepository, runs *RunService) (*PlanService, error) {
	if plans == nil || runs == nil {
		return nil, invalid("plan service requires plan repository and run service")
	}
	return &PlanService{plans: plans, runs: runs, clock: func() time.Time { return time.Now().UTC() }}, nil
}

// GetByRun 读取当前用户 Run 的最新计划与步骤。
func (s *PlanService) GetByRun(ctx context.Context, principal Principal, runID uuid.UUID) (Plan, error) {
	if principal.UserID == uuid.Nil {
		return Plan{}, ErrForbidden
	}
	if _, err := s.runs.Get(ctx, principal, runID); err != nil {
		return Plan{}, err
	}
	return s.plans.GetPlanByRun(ctx, principal.UserID, runID)
}

// ListVersions 列出 Run 的计划版本，仅包含元数据。
func (s *PlanService) ListVersions(ctx context.Context, principal Principal, runID uuid.UUID) ([]Plan, error) {
	if principal.UserID == uuid.Nil {
		return nil, ErrForbidden
	}
	if _, err := s.runs.Get(ctx, principal, runID); err != nil {
		return nil, err
	}
	return s.plans.ListPlanVersions(ctx, principal.UserID, runID)
}

// Cancel 人工终止计划并取消关联 Run；终态计划返回冲突。
func (s *PlanService) Cancel(ctx context.Context, principal Principal, runID uuid.UUID) (Plan, error) {
	plan, err := s.GetByRun(ctx, principal, runID)
	if err != nil {
		return Plan{}, err
	}
	if PlanTerminal(plan.Status) {
		return Plan{}, fmt.Errorf("%w: plan is already terminal", ErrResourceConflict)
	}
	if err := TransitionPlanStatus(plan.Status, PlanCancelled); err != nil {
		return Plan{}, err
	}
	now := s.clock()
	cancelled, err := s.plans.UpdatePlan(ctx, principal.UserID, plan.ID, plan.Status, PlanCancelled, plan.Revision, PlanFence{}, PlanUpdate{
		ErrorCode: "plan_cancelled", ErrorMessage: "计划已被人工终止", FinishedAt: &now,
	})
	if err != nil {
		return Plan{}, err
	}
	// Run 可能已经终态：取消冲突不覆盖已完成的人工终止事实。
	if _, err := s.runs.Cancel(ctx, principal, runID); err != nil && !errors.Is(err, ErrResourceConflict) {
		return Plan{}, err
	}
	return cancelled, nil
}

// Pause 暂停执行中的计划；Worker 在下一个检查点释放任务。
func (s *PlanService) Pause(ctx context.Context, principal Principal, runID uuid.UUID) (Plan, error) {
	plan, err := s.GetByRun(ctx, principal, runID)
	if err != nil {
		return Plan{}, err
	}
	if plan.Status != PlanExecuting && plan.Status != PlanWaitingApproval {
		return Plan{}, fmt.Errorf("%w: only executing plans can be paused", ErrResourceConflict)
	}
	if err := TransitionPlanStatus(plan.Status, PlanPaused); err != nil {
		return Plan{}, err
	}
	return s.plans.UpdatePlan(ctx, principal.UserID, plan.ID, plan.Status, PlanPaused, plan.Revision, PlanFence{}, PlanUpdate{
		ErrorCode: "plan_paused", ErrorMessage: "计划已暂停",
	})
}

// extendForPause 把暂停期间从 Run 与 Plan 的截止时间中扣除。
// 暂停不应消耗执行窗口，否则长时间暂停后的恢复会立即触发 deadline_exceeded。
func (s *PlanService) extendForPause(ctx context.Context, principal Principal, plan Plan) (Plan, error) {
	pausedFor := s.clock().Sub(plan.UpdatedAt)
	if pausedFor <= 0 {
		return plan, nil
	}
	if plan.DeadlineAt != nil {
		deadline := plan.DeadlineAt.Add(pausedFor)
		if extender, ok := s.plans.(PlanDeadlineExtender); ok {
			updated, err := extender.ExtendPlanDeadline(ctx, principal.UserID, plan.ID, deadline, s.clock())
			if err != nil {
				return Plan{}, err
			}
			plan = updated
		} else {
			plan.DeadlineAt = &deadline
		}
	}
	if _, err := s.runs.ExtendDeadline(ctx, principal, plan.RunID, pausedFor); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

// Resume 恢复暂停的计划并重新入队 Run。
func (s *PlanService) Resume(ctx context.Context, principal Principal, runID uuid.UUID) (Plan, error) {
	plan, err := s.GetByRun(ctx, principal, runID)
	if err != nil {
		return Plan{}, err
	}
	if plan.Status != PlanPaused {
		return Plan{}, fmt.Errorf("%w: only paused plans can be resumed", ErrResourceConflict)
	}
	if err := TransitionPlanStatus(plan.Status, PlanExecuting); err != nil {
		return Plan{}, err
	}
	plan, err = s.extendForPause(ctx, principal, plan)
	if err != nil {
		return Plan{}, err
	}
	resumed, err := s.plans.UpdatePlan(ctx, principal.UserID, plan.ID, PlanPaused, PlanExecuting, plan.Revision, PlanFence{}, PlanUpdate{
		ErrorCode: "", ErrorMessage: "",
	})
	if err != nil {
		return Plan{}, err
	}
	run, err := s.runs.ResumeAfterPause(ctx, principal, runID)
	if err != nil {
		// 入队或审批状态不满足时回滚计划状态，避免出现“计划执行中但没有 Run”的悬挂状态。
		_, _ = s.plans.UpdatePlan(ctx, principal.UserID, plan.ID, PlanExecuting, PlanPaused, resumed.Revision, PlanFence{}, PlanUpdate{
			ErrorCode: "plan_paused", ErrorMessage: "计划恢复失败，已回到暂停状态",
		})
		return Plan{}, err
	}
	if run.Status == RunWaitingApproval {
		// 仍有待决策审批：计划保持暂停，等待审批完成后再由用户恢复。
		_, _ = s.plans.UpdatePlan(ctx, principal.UserID, plan.ID, PlanExecuting, PlanPaused, resumed.Revision, PlanFence{}, PlanUpdate{
			ErrorCode: "plan_paused", ErrorMessage: "仍有工具审批待处理，计划保持暂停",
		})
		return Plan{}, fmt.Errorf("%w: plan has pending tool approvals", ErrResourceConflict)
	}
	return resumed, nil
}

// Revise 人工修改暂停中的计划：创建新版本并沿用原配置快照，随后恢复执行。
// 原版本保留为审计历史，新版本通过 parent_plan_id 关联。
func (s *PlanService) Revise(ctx context.Context, principal Principal, runID uuid.UUID, draft PlanDraft) (Plan, error) {
	if principal.UserID == uuid.Nil {
		return Plan{}, ErrForbidden
	}
	latest, err := s.GetByRun(ctx, principal, runID)
	if err != nil {
		return Plan{}, err
	}
	if latest.Status != PlanPaused {
		return Plan{}, fmt.Errorf("%w: only paused plans can be revised", ErrResourceConflict)
	}
	// 审批待决策时修订会让旧审批与新版本步骤失配；要求先处理审批再修订。
	runState, err := s.runs.Get(ctx, principal, runID)
	if err != nil {
		return Plan{}, err
	}
	if runState.Status == RunWaitingApproval {
		return Plan{}, fmt.Errorf("%w: resolve pending tool approvals before revising the plan", ErrResourceConflict)
	}
	limits := DefaultPlanLimits()
	draft, err = validatePlanDraft(draft, limits)
	if err != nil {
		return Plan{}, err
	}
	now := s.clock()
	planID := uuid.New()
	steps := make([]PlanStep, 0, len(draft.Steps))
	for _, draftStep := range draft.Steps {
		maxAttempts := 2
		if draftStep.ToolName != "" {
			maxAttempts = 1
		}
		steps = append(steps, PlanStep{
			ID: uuid.New(), PlanID: planID, UserID: principal.UserID, StepNo: draftStep.StepNo,
			Title: draftStep.Title, Description: draftStep.Description, DependsOn: draftStep.DependsOn,
			ToolName: draftStep.ToolName, Arguments: draftStep.Arguments,
			IdempotencyKey: fmt.Sprintf("plan:%s:step:%d", planID, draftStep.StepNo),
			Status:         PlanStepPending, MaxAttempts: maxAttempts, CreatedAt: now, UpdatedAt: now,
		})
	}
	parentID := latest.ID
	steps = carryCompletedPlanSteps(steps, latest, true)
	latest, err = s.extendForPause(ctx, principal, latest)
	if err != nil {
		return Plan{}, err
	}
	created, err := s.plans.CreatePlan(ctx, principal.UserID, Plan{
		ID: planID, UserID: principal.UserID, RunID: latest.RunID, SessionID: latest.SessionID,
		Version: latest.Version + 1, ParentPlanID: &parentID, Status: PlanExecuting, Goal: draft.Goal,
		ConfigSnapshot: latest.ConfigSnapshot, MaxSteps: limits.MaxSteps, DeadlineAt: latest.DeadlineAt,
		CreatedAt: now, UpdatedAt: now, Steps: steps,
	}, OutboxEvent{
		UserID: principal.UserID, EventType: "plan.revised", AggregateType: "plan", AggregateID: planID.String(),
		DedupeKey: "plan.revised:" + planID.String(),
		Payload:   map[string]any{"run_id": latest.RunID.String(), "version": latest.Version + 1, "parent_plan_id": parentID.String()},
	})
	if err != nil {
		return Plan{}, err
	}
	// 旧版本必须退出 paused 生命周期，否则后续“继续”可能重新绑定已修订的旧计划。
	if _, err := s.plans.UpdatePlan(ctx, principal.UserID, latest.ID, PlanPaused, PlanCancelled, latest.Revision, PlanFence{}, PlanUpdate{
		ErrorCode: "plan_superseded", ErrorMessage: "计划已由人工修订版本替代", FinishedAt: ptrTime(now),
	}); err != nil {
		_, _ = s.plans.UpdatePlan(ctx, principal.UserID, created.ID, PlanExecuting, PlanFailed, created.Revision, PlanFence{}, PlanUpdate{
			ErrorCode: "plan_supersede_failed", ErrorMessage: "旧计划版本无法关闭，已终止新版本", FinishedAt: ptrTime(now),
		})
		return Plan{}, err
	}
	run, err := s.runs.ResumeAfterPause(ctx, principal, runID)
	if err != nil {
		_, _ = s.plans.UpdatePlan(ctx, principal.UserID, created.ID, PlanExecuting, PlanFailed, created.Revision, PlanFence{}, PlanUpdate{
			ErrorCode: "plan_resume_failed", ErrorMessage: "修订后的计划无法恢复执行", FinishedAt: ptrTime(now),
		})
		return Plan{}, err
	}
	if run.Status == RunWaitingApproval {
		paused, pauseErr := s.plans.UpdatePlan(ctx, principal.UserID, created.ID, PlanExecuting, PlanPaused, created.Revision, PlanFence{}, PlanUpdate{
			ErrorCode: "plan_paused", ErrorMessage: "仍有工具审批待处理，修订计划保持暂停",
		})
		if pauseErr != nil {
			return Plan{}, pauseErr
		}
		return paused, nil
	}
	return created, nil
}
