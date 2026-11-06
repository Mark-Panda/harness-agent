package biz

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/pkg/agent/model"
	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
	"github.com/google/uuid"
)

// einoPlanFixture 构造可直接调用 executePlan 的计划 Worker。
func einoPlanFixture(t *testing.T, handler func(body string, w http.ResponseWriter)) (*ExecutionWorker, *memoryPlanRepo, *model.Router) {
	t.Helper()
	worker, plans, router, _ := planWorkerFixture(t, handler)
	return worker, plans, router
}

// TestExecutePlanEinoSuccess 验证 Eino 图能按依赖顺序推进多个模型步骤并汇总。
func TestExecutePlanEinoSuccess(t *testing.T) {
	planner := `{"goal":"整理数据","steps":[{"step_no":1,"title":"收集","description":"收集数据","depends_on":[],"tool_name":"","arguments":{}},{"step_no":2,"title":"总结","description":"输出总结","depends_on":[1],"tool_name":"","arguments":{}}]}`
	worker, plans, router := einoPlanFixture(t, func(body string, w http.ResponseWriter) {
		switch {
		case strings.Contains(body, "最终汇总器"):
			writeCompletion(t, w, "最终汇总", http.StatusOK)
		case strings.Contains(body, "生成可执行计划"):
			writeCompletion(t, w, planner, http.StatusOK)
		default:
			writeCompletion(t, w, "步骤结果", http.StatusOK)
		}
	})
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), Mode: AgentModePlan}
	messages := []react.Message{{Role: react.RoleSystem, Content: "system"}, {Role: react.RoleUser, Content: "整理数据"}}
	result, err := worker.executePlan(context.Background(), run, RunFence{Owner: "plan-worker", Token: 1}, router, messages, emptyToolRegistry{})
	if err != nil {
		plan, _ := plans.GetPlanByRun(context.Background(), run.UserID, run.ID)
		t.Logf("plan after failure: %#v", plan)
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "最终汇总") {
		t.Fatalf("final content = %q", result.Content)
	}
	plan, err := plans.GetPlanByRun(context.Background(), run.UserID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != PlanCompleted || len(plan.Steps) != 2 {
		t.Fatalf("plan = %#v", plan)
	}
	for _, step := range plan.Steps {
		if step.Status != PlanStepCompleted {
			t.Fatalf("step %d status = %s", step.StepNo, step.Status)
		}
	}
}

// TestExecutePlanEinoToolStep 验证 Eino 图的工具分支会执行 low 风险工具并进入汇总。
func TestExecutePlanEinoToolStep(t *testing.T) {
	worker, plans, router := einoPlanFixture(t, func(body string, w http.ResponseWriter) {
		if strings.Contains(body, "最终汇总器") {
			writeCompletion(t, w, "工具汇总", http.StatusOK)
			return
		}
		writeCompletion(t, w, "unused", http.StatusOK)
	})
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), Mode: AgentModePlan}
	step := PlanStep{
		ID: uuid.New(), UserID: run.UserID, StepNo: 1, Title: "调用工具",
		ToolName: "tool", Arguments: map[string]any{"x": 1}, Status: PlanStepPending,
		MaxAttempts: 1, IdempotencyKey: "plan:test:tool:1",
	}
	plan := Plan{
		ID: uuid.New(), UserID: run.UserID, RunID: run.ID, SessionID: run.SessionID, Version: 1,
		Status: PlanExecuting, Goal: "g", MaxSteps: 50, Steps: []PlanStep{step},
	}
	plan.Steps[0].PlanID = plan.ID
	plans.seedPlan(plan)
	result, err := worker.executePlan(context.Background(), run, RunFence{Owner: "plan-worker", Token: 1}, router,
		[]react.Message{{Role: react.RoleUser, Content: "任务"}},
		fakePlanRegistry{tool: fakePlanTool{risk: "low", output: `{"ok":true}`}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "工具汇总" {
		t.Fatalf("final content = %q", result.Content)
	}
	stored, err := plans.GetPlanByRun(context.Background(), run.UserID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != PlanCompleted || stored.Steps[0].Status != PlanStepCompleted ||
		stored.Steps[0].Result["content"] != `{"ok":true}` {
		t.Fatalf("tool plan = %#v", stored)
	}
}

// TestExecutePlanEinoCompletedPlanRecovery 验证已完成计划只恢复既有最终答复。
func TestExecutePlanEinoCompletedPlanRecovery(t *testing.T) {
	worker, plans, router := einoPlanFixture(t, func(_ string, w http.ResponseWriter) {
		writeCompletion(t, w, "unused", http.StatusOK)
	})
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), Mode: AgentModePlan}
	plans.seedPlan(Plan{
		ID: uuid.New(), UserID: run.UserID, RunID: run.ID, SessionID: run.SessionID, Version: 1,
		Status: PlanCompleted, Goal: "g", MaxSteps: 50,
		Steps: []PlanStep{{ID: uuid.New(), StepNo: 1, Title: "s", Status: PlanStepCompleted, Result: map[string]any{"content": "结果"}}},
	})
	result, err := worker.executePlan(context.Background(), run, RunFence{Owner: "plan-worker", Token: 1}, router,
		[]react.Message{{Role: react.RoleUser, Content: "任务"}}, emptyToolRegistry{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "结果") {
		t.Fatalf("recovered content = %q", result.Content)
	}
}

// TestExecutePlanEinoPaused 验证暂停计划在 Eino 路径下仍返回暂停语义。
func TestExecutePlanEinoPaused(t *testing.T) {
	worker, plans, router := einoPlanFixture(t, func(_ string, w http.ResponseWriter) {
		writeCompletion(t, w, "unused", http.StatusOK)
	})
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), Mode: AgentModePlan}
	plans.seedPlan(Plan{
		ID: uuid.New(), UserID: run.UserID, RunID: run.ID, SessionID: run.SessionID, Version: 1,
		Status: PlanPaused, Goal: "g", MaxSteps: 50,
		Steps: []PlanStep{{ID: uuid.New(), StepNo: 1, Title: "s", Status: PlanStepPending, MaxAttempts: 2}},
	})
	if _, err := worker.executePlan(context.Background(), run, RunFence{Owner: "plan-worker", Token: 1}, router,
		[]react.Message{{Role: react.RoleUser, Content: "任务"}}, emptyToolRegistry{}); !errors.Is(err, ErrPlanPaused) {
		t.Fatalf("paused plan error = %v", err)
	}
}

// TestExecutePlanEinoApprovalPauseAndResume 验证高风险工具步骤暂停后可由审批结果恢复并完成。
func TestExecutePlanEinoApprovalPauseAndResume(t *testing.T) {
	approvals := &fakePlanApprovals{}
	worker, plans, router := einoPlanFixture(t, func(body string, w http.ResponseWriter) {
		if strings.Contains(body, "最终汇总器") {
			writeCompletion(t, w, "审批后汇总", http.StatusOK)
			return
		}
		writeCompletion(t, w, "unused", http.StatusOK)
	})
	worker.approvals = approvals
	worker.steps = &fakeStepRepository{}
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), Mode: AgentModePlan}
	step := PlanStep{
		ID: uuid.New(), UserID: run.UserID, StepNo: 1, Title: "危险工具",
		ToolName: "tool", Arguments: map[string]any{"x": 1}, Status: PlanStepPending,
		MaxAttempts: 1, IdempotencyKey: "plan:test:approval:1",
	}
	plan := Plan{
		ID: uuid.New(), UserID: run.UserID, RunID: run.ID, SessionID: run.SessionID, Version: 1,
		Status: PlanExecuting, Goal: "g", MaxSteps: 50, Steps: []PlanStep{step},
	}
	plan.Steps[0].PlanID = plan.ID
	plans.seedPlan(plan)
	registry := fakePlanRegistry{tool: fakePlanTool{risk: "high", output: `{"ok":true}`}}

	_, err := worker.executePlan(context.Background(), run, RunFence{Owner: "plan-worker", Token: 1}, router,
		[]react.Message{{Role: react.RoleUser, Content: "任务"}}, registry)
	if !errors.Is(err, ErrApprovalPending) {
		t.Fatalf("approval pause error = %v", err)
	}
	stored, err := plans.GetPlanByRun(context.Background(), run.UserID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != PlanWaitingApproval || stored.Steps[0].Status != PlanStepWaitingApproval {
		t.Fatalf("waiting plan = %#v", stored)
	}
	if len(approvals.items) != 1 || approvals.items[0].Status != ApprovalPending {
		t.Fatalf("approval = %#v", approvals.items)
	}

	approvals.items[0].Status = ApprovalExecuted
	approvals.items[0].ResultJSON = json.RawMessage(`{"approved":true}`)
	result, err := worker.executePlan(context.Background(), run, RunFence{Owner: "plan-worker", Token: 1}, router,
		[]react.Message{{Role: react.RoleUser, Content: "任务"}}, registry)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "审批后汇总" {
		t.Fatalf("resumed content = %q", result.Content)
	}
	stored, err = plans.GetPlanByRun(context.Background(), run.UserID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != PlanCompleted || stored.Steps[0].Status != PlanStepCompleted {
		t.Fatalf("resumed plan = %#v", stored)
	}
}

// TestExecutePlanEinoReplan 验证步骤失败后 Eino 图会重规划并执行新版本计划。
func TestExecutePlanEinoReplan(t *testing.T) {
	firstPlan := `{"goal":"g","steps":[{"step_no":1,"title":"s1","description":"d1"}]}`
	secondPlan := `{"goal":"g","steps":[{"step_no":1,"title":"s2","description":"d2"}]}`
	worker, plans, router := einoPlanFixture(t, func(body string, w http.ResponseWriter) {
		switch {
		case strings.Contains(body, "最终汇总器"):
			writeCompletion(t, w, "重规划汇总", http.StatusOK)
		case strings.Contains(body, "修正后的完整可执行计划"):
			writeCompletion(t, w, secondPlan, http.StatusOK)
		case strings.Contains(body, "生成可执行计划"):
			writeCompletion(t, w, firstPlan, http.StatusOK)
		case strings.Contains(body, "步骤执行器"):
			if strings.Contains(body, "「s2」") {
				writeCompletion(t, w, "新步骤结果", http.StatusOK)
				return
			}
			writeCompletion(t, w, "失败", http.StatusInternalServerError)
		default:
			writeCompletion(t, w, "unused", http.StatusOK)
		}
	})
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), Mode: AgentModePlan}
	result, err := worker.executePlan(context.Background(), run, RunFence{Owner: "plan-worker", Token: 1}, router,
		[]react.Message{{Role: react.RoleUser, Content: "任务"}}, emptyToolRegistry{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "重规划汇总" {
		t.Fatalf("final content = %q", result.Content)
	}
	stored, err := plans.GetPlanByRun(context.Background(), run.UserID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Version != 2 || stored.Status != PlanCompleted || stored.Steps[0].Title != "s2" {
		t.Fatalf("replanned plan = %#v", stored)
	}
}

// TestExecutePlanEinoBudgetGuard 验证 Eino 图的守卫节点会阻断超预算计划。
func TestExecutePlanEinoBudgetGuard(t *testing.T) {
	worker, plans, router := einoPlanFixture(t, func(_ string, w http.ResponseWriter) {
		writeCompletion(t, w, "unused", http.StatusOK)
	})
	worker.config.PlanMaxTokens = 10
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), Mode: AgentModePlan}
	plans.seedPlan(Plan{
		ID: uuid.New(), UserID: run.UserID, RunID: run.ID, SessionID: run.SessionID, Version: 1,
		Status: PlanExecuting, Goal: "g", MaxSteps: 50, TotalInputTokens: 100,
		Steps: []PlanStep{{ID: uuid.New(), StepNo: 1, Title: "s", Status: PlanStepPending, MaxAttempts: 2}},
	})
	if _, err := worker.executePlan(context.Background(), run, RunFence{Owner: "plan-worker", Token: 1}, router,
		[]react.Message{{Role: react.RoleUser, Content: "任务"}}, emptyToolRegistry{}); !errors.Is(err, ErrPlanFailed) {
		t.Fatalf("budget error = %v", err)
	}
	stored, err := plans.GetPlanByRun(context.Background(), run.UserID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != PlanFailed || stored.ErrorCode != "plan_budget_exceeded" {
		t.Fatalf("budget plan = %#v", stored)
	}
}

// TestExecutePlanEinoDeadlineGuard 验证 Eino 图的守卫节点在截止时间后终止计划。
func TestExecutePlanEinoDeadlineGuard(t *testing.T) {
	worker, plans, router := einoPlanFixture(t, func(_ string, w http.ResponseWriter) {
		writeCompletion(t, w, "unused", http.StatusOK)
	})
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), Mode: AgentModePlan}
	deadline := time.Now().Add(-time.Minute)
	plans.seedPlan(Plan{
		ID: uuid.New(), UserID: run.UserID, RunID: run.ID, SessionID: run.SessionID, Version: 1,
		Status: PlanExecuting, Goal: "g", MaxSteps: 50, DeadlineAt: &deadline,
		Steps: []PlanStep{{ID: uuid.New(), StepNo: 1, Title: "s", Status: PlanStepPending, MaxAttempts: 2}},
	})
	if _, err := worker.executePlan(context.Background(), run, RunFence{Owner: "plan-worker", Token: 1}, router,
		[]react.Message{{Role: react.RoleUser, Content: "任务"}}, emptyToolRegistry{}); !errors.Is(err, ErrPlanFailed) {
		t.Fatalf("deadline error = %v", err)
	}
	stored, err := plans.GetPlanByRun(context.Background(), run.UserID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != PlanFailed || stored.ErrorCode != "plan_deadline_exceeded" {
		t.Fatalf("deadline plan = %#v", stored)
	}
}
