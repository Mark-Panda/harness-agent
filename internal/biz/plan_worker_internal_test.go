package biz

import (
	"strings"
	"testing"
)

// TestCarryCompletedPlanSteps 验证新版本计划会继承匹配的已完成步骤结果。
func TestCarryCompletedPlanSteps(t *testing.T) {
	previous := Plan{Steps: []PlanStep{
		{StepNo: 1, Title: "查询", Description: "查询数据", ToolName: "mcp_lookup", Status: PlanStepCompleted,
			Attempts: 1, Result: map[string]any{"content": "ok"}},
		{StepNo: 2, Title: "写入", Description: "写入数据", ToolName: "mcp_write", Status: PlanStepCompleted,
			Result: map[string]any{"content": "written"}},
	}}
	steps := []PlanStep{
		{StepNo: 1, Title: "查询", Description: "查询数据", ToolName: "mcp_lookup", Status: PlanStepPending},
		{StepNo: 2, Title: "写入", Description: "用户修改了说明", ToolName: "mcp_write", Status: PlanStepPending},
	}
	carried := carryCompletedPlanSteps(steps, previous, true)
	if carried[0].Status != PlanStepCompleted || carried[0].Result["content"] != "ok" {
		t.Fatalf("matching completed step was not carried: %#v", carried[0])
	}
	if carried[1].Status != PlanStepPending {
		t.Fatalf("modified step must re-run: %#v", carried[1])
	}
	// 重规划只按标题与工具匹配，描述变化不应阻断结果继承。
	replanned := carryCompletedPlanSteps([]PlanStep{
		{StepNo: 2, Title: "写入", Description: "重新生成的说明", ToolName: "mcp_write", Status: PlanStepPending},
	}, previous, false)
	if replanned[0].Status != PlanStepCompleted {
		t.Fatalf("replanned completed step was not carried: %#v", replanned[0])
	}
}

// TestSelectPlanStep 验证依赖满足、失败重试与终态失败判定。
func TestSelectPlanStep(t *testing.T) {
	plan := Plan{Steps: []PlanStep{
		{StepNo: 1, Status: PlanStepPending},
		{StepNo: 2, Status: PlanStepPending, DependsOn: []int{1}},
	}}
	step, completed, failed := selectPlanStep(plan)
	if completed || failed || step == nil || step.StepNo != 1 {
		t.Fatalf("first pending step selection = %#v completed=%t failed=%t", step, completed, failed)
	}

	plan.Steps[0].Status = PlanStepCompleted
	step, completed, failed = selectPlanStep(plan)
	if completed || failed || step == nil || step.StepNo != 2 {
		t.Fatalf("dependent step selection = %#v completed=%t failed=%t", step, completed, failed)
	}

	plan.Steps[1].Status = PlanStepFailed
	plan.Steps[1].Attempts = 2
	plan.Steps[1].MaxAttempts = 2
	step, completed, failed = selectPlanStep(plan)
	if step != nil || completed || !failed {
		t.Fatalf("exhausted failure detection = %#v completed=%t failed=%t", step, completed, failed)
	}

	plan.Steps[1].Status = PlanStepFailed
	plan.Steps[1].Attempts = 1
	step, _, _ = selectPlanStep(plan)
	if step == nil || step.StepNo != 2 {
		t.Fatalf("retryable failed step was not selected: %#v", step)
	}

	plan.Steps[0].Status = PlanStepCompleted
	plan.Steps[1].Status = PlanStepCompleted
	_, completed, failed = selectPlanStep(plan)
	if !completed || failed {
		t.Fatalf("all completed = %t failed=%t", completed, failed)
	}

	plan.Steps[1].Status = PlanStepSkipped
	_, _, failed = selectPlanStep(plan)
	if !failed {
		t.Fatal("skipped step must mark the plan as failed")
	}
}

// TestPlanFinalContent 验证最终回答包含步骤结果与失败原因。
func TestPlanFinalContent(t *testing.T) {
	plan := Plan{Goal: "整理报告", Steps: []PlanStep{
		{StepNo: 1, Title: "收集数据", Status: PlanStepCompleted, Result: map[string]any{"content": "数据已汇总"}},
		{StepNo: 2, Title: "生成摘要", Status: PlanStepFailed, ErrorMessage: "模型不可用"},
	}}
	content := planFinalContent(plan)
	for _, want := range []string{"整理报告", "收集数据", "数据已汇总", "生成摘要", "模型不可用"} {
		if !strings.Contains(content, want) {
			t.Fatalf("final content missing %q: %s", want, content)
		}
	}
}

// TestPlanStepMessages 验证步骤提示包含目标、前置结果与当前步骤。
func TestPlanStepMessages(t *testing.T) {
	plan := Plan{Goal: "生成季度总结", Steps: []PlanStep{
		{StepNo: 1, Title: "收集", Status: PlanStepCompleted, Result: map[string]any{"content": "销售数据"}},
		{StepNo: 2, Title: "总结", Description: "输出摘要", Status: PlanStepPending, DependsOn: []int{1}},
	}}
	messages := planStepMessages(plan, plan.Steps[1], nil)
	if len(messages) != 2 {
		t.Fatalf("message count = %d", len(messages))
	}
	user := messages[1].Content
	for _, want := range []string{"生成季度总结", "销售数据", "当前步骤 2", "输出摘要"} {
		if !strings.Contains(user, want) {
			t.Fatalf("step prompt missing %q: %s", want, user)
		}
	}
}
