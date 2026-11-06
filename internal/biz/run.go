package biz

import "fmt"

// RunStatus Agent Run 生命周期状态。
type RunStatus string

const (
	// RunQueued 已入队，等待 Worker 认领。
	RunQueued RunStatus = "queued"
	// RunRunning 正在执行。
	RunRunning RunStatus = "running"
	// RunWaitingApproval 等待人工审批高风险工具。
	RunWaitingApproval RunStatus = "waiting_approval"
	// RunCompleted 正常完成（终态）。
	RunCompleted RunStatus = "completed"
	// RunFailed 失败结束（终态）。
	RunFailed RunStatus = "failed"
	// RunCancelled 被取消（终态）。
	RunCancelled RunStatus = "cancelled"
	// RunReconciliationRequired 副作用已发生但结果未知，需人工对账，禁止盲目重试。
	RunReconciliationRequired RunStatus = "reconciliation_required"
)

// runTransitions 定义全部合法状态迁移，终态不再允许任何出边。
var runTransitions = map[RunStatus]map[RunStatus]struct{}{
	RunQueued: {
		RunRunning: {}, RunFailed: {}, RunCancelled: {},
	},
	RunRunning: {
		// RunQueued 用于计划暂停等“主动让出会话”的场景：停止续租后退回队列，
		// 避免同会话新消息被一个长期暂停的 Run 永久阻塞。
		RunQueued: {}, RunWaitingApproval: {}, RunCompleted: {}, RunFailed: {}, RunCancelled: {}, RunReconciliationRequired: {},
	},
	RunWaitingApproval: {
		RunRunning: {}, RunFailed: {}, RunCancelled: {},
	},
	RunReconciliationRequired: {
		RunCompleted: {}, RunFailed: {},
	},
	RunCompleted: {},
	RunFailed:    {},
	RunCancelled: {},
}

// TransitionRunStatus 校验状态迁移；非法迁移返回 ErrResourceConflict，未知状态返回 ErrInvalidConfig。
func TransitionRunStatus(current, next RunStatus) error {
	transitions, known := runTransitions[current]
	if !known {
		return fmt.Errorf("%w: unknown run status %q", ErrInvalidConfig, current)
	}
	if _, allowed := transitions[next]; !allowed {
		return fmt.Errorf("%w: run cannot transition from %s to %s", ErrResourceConflict, current, next)
	}
	return nil
}

// RunTerminal 判断是否处于终态，终态 Run 不得再被 Worker 修改。
func RunTerminal(status RunStatus) bool {
	return status == RunCompleted || status == RunFailed || status == RunCancelled
}

// RunSettled 判断 Run 是否已停止自动执行。
// 人工对账状态虽不是终态，但同样不会再被 Worker 自动推进，SSE 与前端应按“已停止”
// 关闭连接并允许用户发起新的对话，避免任务永久占据当前会话。
func RunSettled(status RunStatus) bool {
	return RunTerminal(status) || status == RunReconciliationRequired
}
