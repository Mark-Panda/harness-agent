package biz

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/Mark-Panda/harness-agent/pkg/agent/model"
	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
	"github.com/cloudwego/eino/compose"
)

// Supervisor 模式 Eino 图的节点名。
const (
	supervisorNodeLoad      = "supervisor_load"
	supervisorNodeWorkers   = "supervisor_workers"
	supervisorNodeAggregate = "supervisor_aggregate"
)

// supervisorGraphState 是单次 Supervisor 执行的图内状态。
// 子任务事实源仍是 PostgreSQL；图状态只保存本轮编排的任务快照、共享用量与结果。
type supervisorGraphState struct {
	Tasks          []SupervisorTask
	Usage          supervisorUsage
	Result         react.Result
	HasPending     bool
	BudgetExceeded bool
}

// executeSupervisorEino 使用 Eino Graph 编排 Supervisor：加载委派 → 并发 Worker → 汇总。
func (w *ExecutionWorker) executeSupervisorEino(ctx context.Context, run Run, fence RunFence, candidates []model.Candidate, policyVersion *int, messages []react.Message, tools react.Registry) (react.Result, error) {
	var attempts atomic.Int64
	attempts.Store(int64(run.AttemptNo))
	graph, err := w.buildSupervisorGraph(run, fence, candidates, policyVersion, &attempts, messages, tools)
	if err != nil {
		return react.Result{}, err
	}
	runnable, err := graph.Compile(ctx,
		compose.WithGraphName("supervisor_execution"),
		compose.WithMaxRunSteps(8),
	)
	if err != nil {
		return react.Result{}, fmt.Errorf("compile supervisor graph: %w", err)
	}
	state, err := runnable.Invoke(ctx, &supervisorGraphState{})
	if err != nil {
		// 父上下文取消时把仍未领取的子任务同步为取消，避免 Run 结束后留下永久 pending。
		if ctx.Err() != nil {
			_ = w.supervisor.CancelPendingTasks(context.WithoutCancel(ctx), run.UserID, run.ID, "任务已取消", w.clock())
		}
		return react.Result{}, err
	}
	if state == nil {
		return react.Result{}, fmt.Errorf("%w: supervisor graph produced no state", ErrSupervisorInvalid)
	}
	return state.Result, nil
}

// buildSupervisorGraph 构造 Supervisor 执行图：加载/生成任务 → 有 pending 时并发执行 → 汇总。
func (w *ExecutionWorker) buildSupervisorGraph(
	run Run, fence RunFence, candidates []model.Candidate, policyVersion *int,
	attempts *atomic.Int64, messages []react.Message, tools react.Registry,
) (*compose.Graph[*supervisorGraphState, *supervisorGraphState], error) {
	graph := compose.NewGraph[*supervisorGraphState, *supervisorGraphState]()

	load := compose.InvokableLambda(func(ctx context.Context, state *supervisorGraphState) (*supervisorGraphState, error) {
		tasks, usage, err := w.prepareSupervisorTasks(ctx, run, fence, candidates, policyVersion, attempts, messages, tools)
		if err != nil {
			return nil, err
		}
		state.Tasks = tasks
		state.Usage = usage
		state.HasPending = len(pendingSupervisorTasks(tasks)) > 0
		return state, nil
	})

	workers := compose.InvokableLambda(func(ctx context.Context, state *supervisorGraphState) (*supervisorGraphState, error) {
		outcome, err := w.runPendingSupervisorTasks(ctx, run, fence, candidates, policyVersion, attempts, messages, tools,
			state.Usage, pendingSupervisorTasks(state.Tasks))
		if err != nil {
			return nil, err
		}
		// 审批门禁拦截必须冒泡给主 Run，由 Worker 暂停等待人工决策。
		if outcome.ApprovalError != nil {
			return nil, outcome.ApprovalError
		}
		state.Tasks = outcome.Tasks
		state.BudgetExceeded = outcome.BudgetExceeded
		return state, nil
	})

	aggregate := compose.InvokableLambda(func(ctx context.Context, state *supervisorGraphState) (*supervisorGraphState, error) {
		tasks, err := w.supervisor.ListTasks(ctx, run.UserID, run.ID)
		if err != nil {
			return nil, err
		}
		// 预算耗尽属于部分失败：已有 Worker 产出时降级为部分结果汇总，只有全部无产出才整体失败。
		if err := w.ensureSupervisorPartialAllowed(run, tasks, state.BudgetExceeded); err != nil {
			return nil, err
		}
		result, err := w.aggregateSupervisor(ctx, run, fence, candidates, policyVersion, attempts, messages, state.Usage, tasks)
		if err != nil {
			return nil, err
		}
		state.Tasks = tasks
		state.Result = result
		return state, nil
	})

	nodes := []struct {
		name string
		node *compose.Lambda
	}{
		{supervisorNodeLoad, load},
		{supervisorNodeWorkers, workers},
		{supervisorNodeAggregate, aggregate},
	}
	for _, item := range nodes {
		if err := graph.AddLambdaNode(item.name, item.node); err != nil {
			return nil, fmt.Errorf("add supervisor graph node %s: %w", item.name, err)
		}
	}

	edges := [][2]string{
		{compose.START, supervisorNodeLoad},
		{supervisorNodeWorkers, supervisorNodeAggregate},
		{supervisorNodeAggregate, compose.END},
	}
	for _, edge := range edges {
		if err := graph.AddEdge(edge[0], edge[1]); err != nil {
			return nil, fmt.Errorf("add supervisor graph edge %s->%s: %w", edge[0], edge[1], err)
		}
	}

	// 没有待执行子任务时直接汇总，避免空跑 Worker 节点。
	if err := graph.AddBranch(supervisorNodeLoad, compose.NewGraphBranch(
		func(_ context.Context, state *supervisorGraphState) (string, error) {
			if state.HasPending {
				return supervisorNodeWorkers, nil
			}
			return supervisorNodeAggregate, nil
		},
		map[string]bool{supervisorNodeWorkers: true, supervisorNodeAggregate: true},
	)); err != nil {
		return nil, fmt.Errorf("add supervisor load branch: %w", err)
	}

	return graph, nil
}
