package biz_test

import (
	"errors"
	"testing"

	"github.com/Mark-Panda/harness-agent/internal/biz"
)

// TestApprovalStatusTransitions 验证审批状态机只允许计划内的迁移。
func TestApprovalStatusTransitions(t *testing.T) {
	valid := [][2]biz.ApprovalStatus{
		{biz.ApprovalPending, biz.ApprovalApproved},
		{biz.ApprovalPending, biz.ApprovalDenied},
		{biz.ApprovalApproved, biz.ApprovalExecuting},
		{biz.ApprovalExecuting, biz.ApprovalExecuted},
		{biz.ApprovalExecuting, biz.ApprovalFailed},
	}
	for _, pair := range valid {
		if err := biz.TransitionApprovalStatus(pair[0], pair[1]); err != nil {
			t.Fatalf("valid transition %s -> %s rejected: %v", pair[0], pair[1], err)
		}
	}
	invalid := [][2]biz.ApprovalStatus{
		{biz.ApprovalPending, biz.ApprovalExecuting},
		{biz.ApprovalApproved, biz.ApprovalDenied},
		{biz.ApprovalExecuting, biz.ApprovalApproved},
		{biz.ApprovalExecuted, biz.ApprovalExecuting},
		{biz.ApprovalDenied, biz.ApprovalApproved},
	}
	for _, pair := range invalid {
		if err := biz.TransitionApprovalStatus(pair[0], pair[1]); !errors.Is(err, biz.ErrResourceConflict) {
			t.Fatalf("invalid transition %s -> %s error = %v", pair[0], pair[1], err)
		}
	}
	for _, status := range []biz.ApprovalStatus{biz.ApprovalExecuted, biz.ApprovalFailed, biz.ApprovalDenied} {
		if !biz.ApprovalTerminal(status) {
			t.Fatalf("%s must be terminal", status)
		}
	}
	for _, status := range []biz.ApprovalStatus{biz.ApprovalPending, biz.ApprovalApproved, biz.ApprovalExecuting} {
		if biz.ApprovalTerminal(status) {
			t.Fatalf("%s must not be terminal", status)
		}
	}
	for _, raw := range []string{"approve", "deny"} {
		if _, err := biz.ParseApprovalDecision(raw); err != nil {
			t.Fatalf("decision %q rejected: %v", raw, err)
		}
	}
}
