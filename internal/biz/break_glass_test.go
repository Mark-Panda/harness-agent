package biz_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/service/mocks"
	"github.com/google/uuid"
	"go.uber.org/mock/gomock"
)

// TestBreakGlassReadAuthorizationAndAudit 验证 Break-Glass 仅管理员可用、理由必填且强制审计。
func TestBreakGlassReadAuthorizationAndAudit(t *testing.T) {
	ctrl := gomock.NewController(t)
	adminID, targetUserID, runID := uuid.New(), uuid.New(), uuid.New()
	runs := mocks.NewMockRunRepository(ctrl)
	steps := mocks.NewMockExecutionStepRepository(ctrl)
	calls := mocks.NewMockModelCallRepository(ctrl)
	sessions := mocks.NewMockSessionRepository(ctrl)
	service, err := biz.NewRunService(runs, sessions, biz.WithExecutionSteps(steps), biz.WithModelCalls(calls))
	if err != nil {
		t.Fatalf("new run service: %v", err)
	}

	run := biz.Run{ID: runID, UserID: targetUserID, Mode: biz.AgentModeReact, Status: biz.RunCompleted}
	step := biz.ExecutionStep{ID: uuid.New(), RunID: runID, UserID: targetUserID, StepSeq: 1}
	payload := biz.ExecutionPayload{ID: uuid.New(), RunID: runID, UserID: targetUserID, Type: biz.PayloadToolOutput}
	call := biz.ModelCall{ID: uuid.New(), RunID: runID, UserID: targetUserID, AttemptNo: 1}

	// 非管理员必须被拒绝，不能读取他人执行记录。
	if _, err := service.BreakGlassRead(context.Background(), biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}, targetUserID, runID, "incident-123"); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("non-admin error = %v", err)
	}
	// 理由缺失或过短必须拒绝，避免无理由特权读取。
	if _, err := service.BreakGlassRead(context.Background(), biz.Principal{UserID: adminID, Role: biz.RoleAdmin}, targetUserID, runID, "  "); err == nil {
		t.Fatal("empty reason must fail")
	}

	runs.EXPECT().GetRun(gomock.Any(), targetUserID, runID).Return(run, nil)
	steps.EXPECT().ListSteps(gomock.Any(), targetUserID, runID, 500).Return([]biz.ExecutionStep{step}, nil)
	steps.EXPECT().ListPayloads(gomock.Any(), targetUserID, runID, nil, 500).Return([]biz.ExecutionPayload{payload}, nil)
	calls.EXPECT().ListModelCalls(gomock.Any(), targetUserID, runID, 200).Return([]biz.ModelCall{call}, nil)
	runs.EXPECT().AppendEvent(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, event biz.RunEvent) (biz.RunEvent, error) {
			if event.RunID != runID || event.UserID != targetUserID || event.EventType != "audit.break_glass.read" {
				t.Errorf("audit event = %+v", event)
			}
			if event.Payload["actor_user_id"] != adminID.String() || event.Payload["reason"] != "incident-123" ||
				event.Payload["target_user_id"] != targetUserID.String() {
				t.Errorf("audit payload = %+v", event.Payload)
			}
			return event, nil
		},
	)
	result, err := service.BreakGlassRead(context.Background(), biz.Principal{UserID: adminID, Role: biz.RoleAdmin}, targetUserID, runID, "incident-123")
	if err != nil {
		t.Fatalf("break-glass read: %v", err)
	}
	if result.Run.ID != runID || len(result.Steps) != 1 || len(result.Payloads) != 1 || len(result.ModelCalls) != 1 {
		t.Fatalf("result = %+v", result)
	}
}

// TestBreakGlassReadFailsClosedWhenAuditFails 验证审计写入失败时不返回执行记录。
func TestBreakGlassReadFailsClosedWhenAuditFails(t *testing.T) {
	ctrl := gomock.NewController(t)
	adminID, targetUserID, runID := uuid.New(), uuid.New(), uuid.New()
	runs := mocks.NewMockRunRepository(ctrl)
	sessions := mocks.NewMockSessionRepository(ctrl)
	service, err := biz.NewRunService(runs, sessions)
	if err != nil {
		t.Fatalf("new run service: %v", err)
	}
	runs.EXPECT().GetRun(gomock.Any(), targetUserID, runID).Return(biz.Run{ID: runID, UserID: targetUserID}, nil)
	runs.EXPECT().AppendEvent(gomock.Any(), gomock.Any()).Return(biz.RunEvent{}, errors.New("audit store down"))
	result, err := service.BreakGlassRead(context.Background(), biz.Principal{UserID: adminID, Role: biz.RoleAdmin}, targetUserID, runID, "incident-456")
	if err == nil {
		t.Fatal("audit failure must surface")
	}
	if result.Run.ID != uuid.Nil || len(result.Steps) != 0 || len(result.Payloads) != 0 || len(result.ModelCalls) != 0 {
		t.Fatalf("result must be empty on audit failure: %+v", result)
	}
}
