package biz_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/service/mocks"
	"github.com/google/uuid"
	"go.uber.org/mock/gomock"
)

// newApprovalRunService 构造带审批仓储与入队能力的 Run 服务。
func newApprovalRunService(t *testing.T, ctrl *gomock.Controller) (*biz.RunService, *mocks.MockRunRepository, *mocks.MockSessionRepository, *mocks.MockToolApprovalRepository, *mocks.MockJobQueue) {
	t.Helper()
	runs := mocks.NewMockRunRepository(ctrl)
	sessions := mocks.NewMockSessionRepository(ctrl)
	approvals := mocks.NewMockToolApprovalRepository(ctrl)
	queue := mocks.NewMockJobQueue(ctrl)
	service, err := biz.NewRunService(runs, sessions,
		biz.WithToolApprovals(approvals), biz.WithRunEnqueuer(queue))
	if err != nil {
		t.Fatal(err)
	}
	return service, runs, sessions, approvals, queue
}

func newApprovalRunServiceWithPlans(t *testing.T, ctrl *gomock.Controller) (*biz.RunService, *mocks.MockRunRepository, *mocks.MockToolApprovalRepository, *mocks.MockJobQueue, *mocks.MockPlanRepository) {
	t.Helper()
	runs := mocks.NewMockRunRepository(ctrl)
	approvals := mocks.NewMockToolApprovalRepository(ctrl)
	queue := mocks.NewMockJobQueue(ctrl)
	plans := mocks.NewMockPlanRepository(ctrl)
	service, err := biz.NewRunService(runs, mocks.NewMockSessionRepository(ctrl),
		biz.WithToolApprovals(approvals), biz.WithRunEnqueuer(queue), biz.WithPlanResume(plans))
	if err != nil {
		t.Fatal(err)
	}
	return service, runs, approvals, queue, plans
}

// TestDecideApprovalResumesRunWhenAllDecided 验证最后一条审批决策会恢复 Run 并唤醒任务。
func TestDecideApprovalResumesRunWhenAllDecided(t *testing.T) {
	ctrl := gomock.NewController(t)
	userID, runID, approvalID := uuid.New(), uuid.New(), uuid.New()
	service, runs, _, approvals, queue := newApprovalRunService(t, ctrl)
	run := biz.Run{ID: runID, UserID: userID, Status: biz.RunWaitingApproval}
	pending := biz.ToolApproval{
		ID: approvalID, RunID: runID, UserID: userID, ToolCallID: "call-1", ToolName: "mcp_admin_delete",
		Status: biz.ApprovalPending, Arguments: json.RawMessage(`{"id":"x"}`), IdempotencyKey: "idem-1",
	}
	approved := pending
	approved.Status = biz.ApprovalApproved
	runs.EXPECT().GetRun(gomock.Any(), userID, runID).Return(run, nil)
	approvals.EXPECT().GetApproval(gomock.Any(), userID, approvalID).Return(pending, nil)
	approvals.EXPECT().DecideApproval(gomock.Any(), userID, runID, approvalID, biz.ApprovalDecisionApprove, userID, gomock.Any()).Return(approved, nil)
	runs.EXPECT().AppendEvent(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, event biz.RunEvent) (biz.RunEvent, error) {
			if event.EventType != "tool.approval.decided" || event.Payload["approval_id"] != approvalID.String() {
				t.Fatalf("unexpected event: %#v", event)
			}
			return event, nil
		},
	)
	approvals.EXPECT().CountPendingApprovals(gomock.Any(), userID, runID).Return(0, nil)
	runs.EXPECT().ResumeRun(gomock.Any(), userID, runID, gomock.Any()).Return(biz.Run{ID: runID, Status: biz.RunRunning}, nil)
	queue.EXPECT().EnqueueRun(gomock.Any(), userID, runID, gomock.Any()).Return(nil)

	result, err := service.DecideApproval(context.Background(), biz.Principal{UserID: userID}, runID, approvalID, "approve")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != biz.ApprovalApproved {
		t.Fatalf("approval status = %s", result.Status)
	}
}

// TestDecideApprovalKeepsRunWaitingWhenOthersPending 验证仍有待决策审批时不恢复 Run。
func TestDecideApprovalKeepsRunWaitingWhenOthersPending(t *testing.T) {
	ctrl := gomock.NewController(t)
	userID, runID, approvalID := uuid.New(), uuid.New(), uuid.New()
	service, runs, _, approvals, _ := newApprovalRunService(t, ctrl)
	run := biz.Run{ID: runID, UserID: userID, Status: biz.RunRunning}
	pending := biz.ToolApproval{
		ID: approvalID, RunID: runID, UserID: userID, ToolCallID: "call-2", ToolName: "mcp_admin_delete",
		Status: biz.ApprovalPending, Arguments: json.RawMessage(`{}`), IdempotencyKey: "idem-2",
	}
	denied := pending
	denied.Status = biz.ApprovalDenied
	runs.EXPECT().GetRun(gomock.Any(), userID, runID).Return(run, nil)
	approvals.EXPECT().GetApproval(gomock.Any(), userID, approvalID).Return(pending, nil)
	approvals.EXPECT().DecideApproval(gomock.Any(), userID, runID, approvalID, biz.ApprovalDecisionDeny, userID, gomock.Any()).Return(denied, nil)
	runs.EXPECT().AppendEvent(gomock.Any(), gomock.Any()).Return(biz.RunEvent{}, nil)
	approvals.EXPECT().CountPendingApprovals(gomock.Any(), userID, runID).Return(1, nil)

	result, err := service.DecideApproval(context.Background(), biz.Principal{UserID: userID}, runID, approvalID, "deny")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != biz.ApprovalDenied {
		t.Fatalf("approval status = %s", result.Status)
	}
}

func TestDecideApprovalNeverAutoResumesPausedPlan(t *testing.T) {
	for _, errorCode := range []string{"plan_paused", "plan_resume_failed", "run_enqueue_failed", ""} {
		t.Run("error_code="+errorCode, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			userID, runID, approvalID := uuid.New(), uuid.New(), uuid.New()
			service, runs, approvals, _, plans := newApprovalRunServiceWithPlans(t, ctrl)
			pending := biz.ToolApproval{ID: approvalID, RunID: runID, UserID: userID, ToolCallID: "call-1", Status: biz.ApprovalPending}
			approved := pending
			approved.Status = biz.ApprovalApproved
			runs.EXPECT().GetRun(gomock.Any(), userID, runID).Return(biz.Run{ID: runID, UserID: userID, Status: biz.RunWaitingApproval}, nil)
			approvals.EXPECT().GetApproval(gomock.Any(), userID, approvalID).Return(pending, nil)
			approvals.EXPECT().DecideApproval(gomock.Any(), userID, runID, approvalID, biz.ApprovalDecisionApprove, userID, gomock.Any()).Return(approved, nil)
			runs.EXPECT().AppendEvent(gomock.Any(), gomock.Any()).Return(biz.RunEvent{}, nil)
			approvals.EXPECT().CountPendingApprovals(gomock.Any(), userID, runID).Return(0, nil)
			plans.EXPECT().GetPlanByRun(gomock.Any(), userID, runID).Return(biz.Plan{Status: biz.PlanPaused, ErrorCode: errorCode}, nil)

			result, err := service.DecideApproval(context.Background(), biz.Principal{UserID: userID}, runID, approvalID, "approve")
			if err != nil || result.Status != biz.ApprovalApproved {
				t.Fatalf("DecideApproval() = %#v, %v", result, err)
			}
		})
	}
}

func TestDecideApprovalResumesWaitingApprovalPlan(t *testing.T) {
	ctrl := gomock.NewController(t)
	userID, runID, approvalID := uuid.New(), uuid.New(), uuid.New()
	service, runs, approvals, queue, plans := newApprovalRunServiceWithPlans(t, ctrl)
	pending := biz.ToolApproval{ID: approvalID, RunID: runID, UserID: userID, Status: biz.ApprovalPending}
	approved := pending
	approved.Status = biz.ApprovalApproved
	runs.EXPECT().GetRun(gomock.Any(), userID, runID).Return(biz.Run{ID: runID, UserID: userID, Status: biz.RunWaitingApproval}, nil)
	approvals.EXPECT().GetApproval(gomock.Any(), userID, approvalID).Return(pending, nil)
	approvals.EXPECT().DecideApproval(gomock.Any(), userID, runID, approvalID, biz.ApprovalDecisionApprove, userID, gomock.Any()).Return(approved, nil)
	runs.EXPECT().AppendEvent(gomock.Any(), gomock.Any()).Return(biz.RunEvent{}, nil)
	approvals.EXPECT().CountPendingApprovals(gomock.Any(), userID, runID).Return(0, nil)
	plans.EXPECT().GetPlanByRun(gomock.Any(), userID, runID).Return(biz.Plan{Status: biz.PlanWaitingApproval}, nil)
	runs.EXPECT().ResumeRun(gomock.Any(), userID, runID, gomock.Any()).Return(biz.Run{Status: biz.RunRunning}, nil)
	queue.EXPECT().EnqueueRun(gomock.Any(), userID, runID, gomock.Any()).Return(nil)
	if _, err := service.DecideApproval(context.Background(), biz.Principal{UserID: userID}, runID, approvalID, "approve"); err != nil {
		t.Fatal(err)
	}
}

func TestDecideApprovalReturnsEnqueueFailure(t *testing.T) {
	ctrl := gomock.NewController(t)
	userID, runID, approvalID := uuid.New(), uuid.New(), uuid.New()
	service, runs, _, approvals, queue := newApprovalRunService(t, ctrl)
	pending := biz.ToolApproval{ID: approvalID, RunID: runID, UserID: userID, Status: biz.ApprovalPending}
	approved := pending
	approved.Status = biz.ApprovalApproved
	enqueueErr := errors.New("queue unavailable")
	runs.EXPECT().GetRun(gomock.Any(), userID, runID).Return(biz.Run{ID: runID, UserID: userID, Status: biz.RunWaitingApproval}, nil)
	approvals.EXPECT().GetApproval(gomock.Any(), userID, approvalID).Return(pending, nil)
	approvals.EXPECT().DecideApproval(gomock.Any(), userID, runID, approvalID, biz.ApprovalDecisionApprove, userID, gomock.Any()).Return(approved, nil)
	runs.EXPECT().AppendEvent(gomock.Any(), gomock.Any()).Return(biz.RunEvent{}, nil)
	approvals.EXPECT().CountPendingApprovals(gomock.Any(), userID, runID).Return(0, nil)
	runs.EXPECT().ResumeRun(gomock.Any(), userID, runID, gomock.Any()).Return(biz.Run{Status: biz.RunRunning}, nil)
	queue.EXPECT().EnqueueRun(gomock.Any(), userID, runID, gomock.Any()).Return(enqueueErr)
	if _, err := service.DecideApproval(context.Background(), biz.Principal{UserID: userID}, runID, approvalID, "approve"); !errors.Is(err, enqueueErr) {
		t.Fatalf("DecideApproval() error = %v, want %v", err, enqueueErr)
	}
}

func TestDecideApprovalReturnsPlanReadFailure(t *testing.T) {
	ctrl := gomock.NewController(t)
	userID, runID, approvalID := uuid.New(), uuid.New(), uuid.New()
	service, runs, approvals, queue, plans := newApprovalRunServiceWithPlans(t, ctrl)
	pending := biz.ToolApproval{ID: approvalID, RunID: runID, UserID: userID, Status: biz.ApprovalPending}
	approved := pending
	approved.Status = biz.ApprovalApproved
	planErr := errors.New("plan store unavailable")
	runs.EXPECT().GetRun(gomock.Any(), userID, runID).Return(biz.Run{ID: runID, UserID: userID, Status: biz.RunWaitingApproval}, nil)
	approvals.EXPECT().GetApproval(gomock.Any(), userID, approvalID).Return(pending, nil)
	approvals.EXPECT().DecideApproval(gomock.Any(), userID, runID, approvalID, biz.ApprovalDecisionApprove, userID, gomock.Any()).Return(approved, nil)
	runs.EXPECT().AppendEvent(gomock.Any(), gomock.Any()).Return(biz.RunEvent{}, nil)
	approvals.EXPECT().CountPendingApprovals(gomock.Any(), userID, runID).Return(0, nil)
	plans.EXPECT().GetPlanByRun(gomock.Any(), userID, runID).Return(biz.Plan{}, planErr)
	runs.EXPECT().ResumeRun(gomock.Any(), userID, runID, gomock.Any()).Return(biz.Run{Status: biz.RunRunning}, nil).AnyTimes()
	queue.EXPECT().EnqueueRun(gomock.Any(), userID, runID, gomock.Any()).Return(nil).AnyTimes()
	if _, err := service.DecideApproval(context.Background(), biz.Principal{UserID: userID}, runID, approvalID, "approve"); !errors.Is(err, planErr) {
		t.Fatalf("DecideApproval() error = %v, want %v", err, planErr)
	}
}

// TestDecideApprovalRejectsInvalidInput 验证非法决策与终态 Run 被拒绝。
func TestDecideApprovalRejectsInvalidInput(t *testing.T) {
	ctrl := gomock.NewController(t)
	userID, runID, approvalID := uuid.New(), uuid.New(), uuid.New()
	service, runs, _, _, _ := newApprovalRunService(t, ctrl)
	if _, err := service.DecideApproval(context.Background(), biz.Principal{UserID: userID}, runID, approvalID, "maybe"); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("invalid decision error = %v", err)
	}
	runs.EXPECT().GetRun(gomock.Any(), userID, runID).Return(biz.Run{ID: runID, UserID: userID, Status: biz.RunCompleted}, nil)
	if _, err := service.DecideApproval(context.Background(), biz.Principal{UserID: userID}, runID, approvalID, "approve"); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("terminal run decision error = %v", err)
	}
}

// TestListApprovalsScopesToRunOwner 验证审批查询先校验 Run 归属。
func TestListApprovalsScopesToRunOwner(t *testing.T) {
	ctrl := gomock.NewController(t)
	userID, runID := uuid.New(), uuid.New()
	service, runs, _, approvals, _ := newApprovalRunService(t, ctrl)
	runs.EXPECT().GetRun(gomock.Any(), userID, runID).Return(biz.Run{ID: runID, UserID: userID, Status: biz.RunWaitingApproval}, nil).Times(2)
	approvals.EXPECT().ListApprovals(gomock.Any(), userID, runID).Return([]biz.ToolApproval{{ID: uuid.New(), RunID: runID, UserID: userID}}, nil)
	items, err := service.ListApprovals(context.Background(), biz.Principal{UserID: userID}, runID)
	if err != nil || len(items) != 1 {
		t.Fatalf("ListApprovals() = %#v, %v", items, err)
	}
	// 未注入审批仓储时返回空列表而不是错误。
	plain, err := biz.NewRunService(runs, mocks.NewMockSessionRepository(ctrl))
	if err != nil {
		t.Fatal(err)
	}
	if items, err := plain.ListApprovals(context.Background(), biz.Principal{UserID: userID}, runID); err != nil || len(items) != 0 {
		t.Fatalf("plain ListApprovals() = %#v, %v", items, err)
	}
}
