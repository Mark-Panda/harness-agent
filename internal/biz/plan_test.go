package biz_test

import (
	"errors"
	"testing"

	"github.com/Mark-Panda/harness-agent/internal/biz"
)

// TestPlanStateMachines 验证计划与步骤状态迁移以及终态保护。
func TestPlanStateMachines(t *testing.T) {
	valid := [][2]biz.PlanStatus{
		{biz.PlanPlanning, biz.PlanExecuting},
		{biz.PlanExecuting, biz.PlanWaitingApproval},
		{biz.PlanWaitingApproval, biz.PlanExecuting},
		{biz.PlanExecuting, biz.PlanPaused},
		{biz.PlanPaused, biz.PlanExecuting},
		{biz.PlanExecuting, biz.PlanCompleted},
		{biz.PlanPlanning, biz.PlanFailed},
		{biz.PlanExecuting, biz.PlanCancelled},
	}
	for _, pair := range valid {
		if err := biz.TransitionPlanStatus(pair[0], pair[1]); err != nil {
			t.Fatalf("valid plan transition %s -> %s rejected: %v", pair[0], pair[1], err)
		}
	}
	for _, terminal := range []biz.PlanStatus{biz.PlanCompleted, biz.PlanFailed, biz.PlanCancelled} {
		if err := biz.TransitionPlanStatus(terminal, biz.PlanExecuting); !errors.Is(err, biz.ErrPlanTerminal) {
			t.Fatalf("terminal plan %s allowed to move: %v", terminal, err)
		}
	}
	if err := biz.TransitionPlanStatus(biz.PlanPlanning, biz.PlanCompleted); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("illegal plan transition error = %v", err)
	}

	stepValid := [][2]biz.PlanStepStatus{
		{biz.PlanStepPending, biz.PlanStepRunning},
		{biz.PlanStepRunning, biz.PlanStepWaitingApproval},
		{biz.PlanStepWaitingApproval, biz.PlanStepCompleted},
		{biz.PlanStepRunning, biz.PlanStepFailed},
		{biz.PlanStepFailed, biz.PlanStepRunning},
		{biz.PlanStepRunning, biz.PlanStepUnknown},
		{biz.PlanStepUnknown, biz.PlanStepFailed},
		{biz.PlanStepPending, biz.PlanStepSkipped},
	}
	for _, pair := range stepValid {
		if err := biz.TransitionPlanStepStatus(pair[0], pair[1]); err != nil {
			t.Fatalf("valid step transition %s -> %s rejected: %v", pair[0], pair[1], err)
		}
	}
	for _, terminal := range []biz.PlanStepStatus{biz.PlanStepCompleted, biz.PlanStepSkipped, biz.PlanStepCancelled} {
		if err := biz.TransitionPlanStepStatus(terminal, biz.PlanStepRunning); !errors.Is(err, biz.ErrPlanStepTerminal) {
			t.Fatalf("terminal step %s allowed to move: %v", terminal, err)
		}
	}
}
