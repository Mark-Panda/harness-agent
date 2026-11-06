package biz

import (
	"context"
	"fmt"

	"github.com/Mark-Panda/harness-agent/pkg/agent/model"
	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
	"github.com/cloudwego/eino/compose"
)

// Plan 模式 Eino 图的节点名。
const (
	planNodeLoad      = "plan_load"
	planNodeRecover   = "plan_recover"
	planNodeGuard     = "plan_guard"
	planNodeApprovals = "plan_approvals"
	planNodeSelect    = "plan_select"
	planNodeModelStep = "plan_model_step"
	planNodeToolStep  = "plan_tool_step"
	planNodeReplan    = "plan_replan"
	planNodeFinish    = "plan_finish"
	planNodeFinal     = "plan_final"
)

// planGraphState 是单次 Plan 执行的图内状态。
// 计划与步骤的事实源仍是 PostgreSQL；图状态只保存本轮推进的快照与分支标记。
type planGraphState struct {
	Plan        Plan
	Step        PlanStep
	Result      react.Result
	Iterations  int
	Finalized   bool
	Finished    bool
	NeedsReplan bool
	Replanned   bool
}

// executePlanEino 使用 Eino Graph 编排 Plan 模式的推进循环。
// 计划持久化、审批恢复、预算与事件语义与原 executePlan 完全一致，图只负责节点流转。
func (w *ExecutionWorker) executePlanEino(ctx context.Context, run Run, fence RunFence, router *model.Router, messages []react.Message, tools react.Registry) (react.Result, error) {
	if w.plans == nil {
		return react.Result{}, fmt.Errorf("%w: plan repository is not configured", ErrPlanInvalid)
	}
	planFence := PlanFence{Owner: w.config.WorkerID, Token: fence.Token}
	graph, err := w.buildPlanGraph(run, fence, planFence, router, messages, tools)
	if err != nil {
		return react.Result{}, err
	}
	// 单次计划最多执行 MaxSteps+9 轮，每轮固定经过 guard/approvals/select/step 四个节点，
	// 额外预留重规划与汇总节点；轮次超限由图内 guard 返回业务错误，而不是依赖 Eino 的通用步数错误。
	runnable, err := graph.Compile(ctx,
		compose.WithGraphName("plan_execution"),
		compose.WithMaxRunSteps((DefaultPlanLimits().MaxSteps+10)*6+16),
	)
	if err != nil {
		return react.Result{}, fmt.Errorf("compile plan graph: %w", err)
	}
	state, err := runnable.Invoke(ctx, &planGraphState{})
	if err != nil {
		return react.Result{}, err
	}
	if state == nil {
		return react.Result{}, fmt.Errorf("%w: plan graph produced no state", ErrPlanInvalid)
	}
	return state.Result, nil
}

// buildPlanGraph 构造 Plan 执行图：加载 → 恢复 → 守卫 → 审批 → 选步 → 执行/重规划 → 汇总。
func (w *ExecutionWorker) buildPlanGraph(
	run Run, fence RunFence, planFence PlanFence,
	router *model.Router, messages []react.Message, tools react.Registry,
) (*compose.Graph[*planGraphState, *planGraphState], error) {
	graph := compose.NewGraph[*planGraphState, *planGraphState]()

	load := compose.InvokableLambda(func(ctx context.Context, state *planGraphState) (*planGraphState, error) {
		plan, err := w.loadOrCreatePlan(ctx, run, fence, router, messages, tools)
		if err != nil {
			return nil, err
		}
		state.Plan = plan
		switch plan.Status {
		case PlanCancelled:
			return nil, ErrPlanCancelled
		case PlanPaused:
			return nil, ErrPlanPaused
		case PlanCompleted:
			// 已完成计划只恢复既有最终答复，禁止再次汇总与追加消息。
			state.Finalized = true
			return state, nil
		case PlanFailed:
			return nil, fmt.Errorf("%w: %s", ErrPlanFailed, plan.ErrorMessage)
		}
		if plan.Status == PlanPlanning {
			updated, err := w.transitionPlan(ctx, plan, PlanExecuting, planFence, "", "")
			if err != nil {
				return nil, err
			}
			state.Plan = updated
		}
		return state, nil
	})

	recoverSteps := compose.InvokableLambda(func(ctx context.Context, state *planGraphState) (*planGraphState, error) {
		recovered, err := w.recoverPlanSteps(ctx, state.Plan, fence)
		if err != nil {
			return nil, err
		}
		state.Plan = recovered
		return state, nil
	})

	guard := compose.InvokableLambda(func(ctx context.Context, state *planGraphState) (*planGraphState, error) {
		state.Iterations++
		plan, err := w.plans.GetPlanByRun(ctx, run.UserID, run.ID)
		if err != nil {
			return nil, err
		}
		state.Plan = plan
		if plan.Status == PlanPaused {
			return nil, ErrPlanPaused
		}
		if plan.Status == PlanCancelled {
			return nil, ErrPlanCancelled
		}
		if state.Iterations > plan.MaxSteps+9 {
			return nil, w.failPlan(ctx, plan, planFence, "plan_step_limit", "计划执行轮次超过上限")
		}
		// 计划级 Token 与费用上限在执行过程中持续校验，超限立即终止而不是继续放大成本。
		if w.config.PlanMaxTokens > 0 && plan.TotalInputTokens+plan.TotalOutputTokens > w.config.PlanMaxTokens {
			return nil, w.failPlan(ctx, plan, planFence, "plan_budget_exceeded", "计划 Token 用量超过上限")
		}
		if w.config.PlanMaxCost > 0 && plan.EstimatedCost > w.config.PlanMaxCost {
			return nil, w.failPlan(ctx, plan, planFence, "plan_budget_exceeded", "计划费用超过上限")
		}
		if plan.DeadlineAt != nil && !w.clock().Before(*plan.DeadlineAt) {
			return nil, w.failPlan(ctx, plan, planFence, "plan_deadline_exceeded", "计划执行超过截止时间")
		}
		return state, nil
	})

	approvals := compose.InvokableLambda(func(ctx context.Context, state *planGraphState) (*planGraphState, error) {
		plan, pending, err := w.resolvePlanApprovals(ctx, run, state.Plan, planFence)
		if err != nil {
			return nil, err
		}
		if pending {
			return nil, ErrApprovalPending
		}
		if plan.Status == PlanWaitingApproval {
			plan, err = w.transitionPlan(ctx, plan, PlanExecuting, planFence, "", "")
			if err != nil {
				return nil, err
			}
		}
		state.Plan = plan
		return state, nil
	})

	selectStep := compose.InvokableLambda(func(ctx context.Context, state *planGraphState) (*planGraphState, error) {
		state.NeedsReplan = false
		next, completed, failed := selectPlanStep(state.Plan)
		if completed {
			state.Finished = true
			return state, nil
		}
		if failed || next == nil {
			// 步骤失败不再立即终止整份计划：允许一次基于失败原因的重规划，
			// 让 Planner 保留已完成成果并生成修正步骤；再次失败才终止。
			if !state.Replanned && state.Plan.Version < 2 {
				state.NeedsReplan = true
				return state, nil
			}
			return nil, w.failPlan(ctx, state.Plan, planFence, "plan_step_failed", "计划步骤失败或依赖无法满足")
		}
		state.Step = *next
		return state, nil
	})

	modelStep := compose.InvokableLambda(func(ctx context.Context, state *planGraphState) (*planGraphState, error) {
		if err := w.executePlanModelStep(ctx, run, planFence, router, messages, tools, state.Plan, state.Step); err != nil {
			return nil, err
		}
		return state, nil
	})

	toolStep := compose.InvokableLambda(func(ctx context.Context, state *planGraphState) (*planGraphState, error) {
		if err := w.executePlanToolStep(ctx, run, fence, planFence, tools, state.Plan, state.Step); err != nil {
			return nil, err
		}
		return state, nil
	})

	replan := compose.InvokableLambda(func(ctx context.Context, state *planGraphState) (*planGraphState, error) {
		state.Replanned = true
		state.NeedsReplan = false
		updated, err := w.replanPlan(ctx, run, fence, router, messages, tools, state.Plan)
		if err != nil {
			w.logger.Warn("replan failed; terminating plan", "run_id", run.ID, "plan_id", state.Plan.ID, "error", err)
			return nil, w.failPlan(ctx, state.Plan, planFence, "plan_step_failed", "计划步骤失败或依赖无法满足")
		}
		state.Plan = updated
		return state, nil
	})

	finish := compose.InvokableLambda(func(ctx context.Context, state *planGraphState) (*planGraphState, error) {
		result, err := w.finishPlan(ctx, run, fence, router, state.Plan, messages)
		if err != nil {
			return nil, err
		}
		state.Result = result
		return state, nil
	})

	final := compose.InvokableLambda(func(ctx context.Context, state *planGraphState) (*planGraphState, error) {
		result, err := w.planFinalResult(ctx, run, fence, state.Plan, messages)
		if err != nil {
			return nil, err
		}
		state.Result = result
		return state, nil
	})

	nodes := []struct {
		name string
		node *compose.Lambda
	}{
		{planNodeLoad, load},
		{planNodeRecover, recoverSteps},
		{planNodeGuard, guard},
		{planNodeApprovals, approvals},
		{planNodeSelect, selectStep},
		{planNodeModelStep, modelStep},
		{planNodeToolStep, toolStep},
		{planNodeReplan, replan},
		{planNodeFinish, finish},
		{planNodeFinal, final},
	}
	for _, item := range nodes {
		if err := graph.AddLambdaNode(item.name, item.node); err != nil {
			return nil, fmt.Errorf("add plan graph node %s: %w", item.name, err)
		}
	}

	edges := [][2]string{
		{compose.START, planNodeLoad},
		{planNodeRecover, planNodeGuard},
		{planNodeGuard, planNodeApprovals},
		{planNodeApprovals, planNodeSelect},
		{planNodeModelStep, planNodeGuard},
		{planNodeToolStep, planNodeGuard},
		{planNodeReplan, planNodeGuard},
		{planNodeFinish, compose.END},
		{planNodeFinal, compose.END},
	}
	for _, edge := range edges {
		if err := graph.AddEdge(edge[0], edge[1]); err != nil {
			return nil, fmt.Errorf("add plan graph edge %s->%s: %w", edge[0], edge[1], err)
		}
	}

	if err := graph.AddBranch(planNodeLoad, compose.NewGraphBranch(
		func(_ context.Context, state *planGraphState) (string, error) {
			if state.Finalized {
				return planNodeFinal, nil
			}
			return planNodeRecover, nil
		},
		map[string]bool{planNodeRecover: true, planNodeFinal: true},
	)); err != nil {
		return nil, fmt.Errorf("add plan load branch: %w", err)
	}

	if err := graph.AddBranch(planNodeSelect, compose.NewGraphBranch(
		func(_ context.Context, state *planGraphState) (string, error) {
			switch {
			case state.Finished:
				return planNodeFinish, nil
			case state.NeedsReplan:
				return planNodeReplan, nil
			case state.Step.ToolName == "":
				return planNodeModelStep, nil
			default:
				return planNodeToolStep, nil
			}
		},
		map[string]bool{
			planNodeFinish: true, planNodeReplan: true,
			planNodeModelStep: true, planNodeToolStep: true,
		},
	)); err != nil {
		return nil, fmt.Errorf("add plan select branch: %w", err)
	}

	return graph, nil
}
