package biz_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/service/mocks"
	"github.com/google/uuid"
	"go.uber.org/mock/gomock"
)

// TestRunServiceCreateAndValidation 验证 Run 创建、会话归属与模式校验。
func TestRunServiceCreateAndValidation(t *testing.T) {
	ctrl := gomock.NewController(t)
	runRepo := mocks.NewMockRunRepository(ctrl)
	sessionRepo := mocks.NewMockSessionRepository(ctrl)
	service, err := biz.NewRunService(runRepo, sessionRepo)
	if err != nil {
		t.Fatal(err)
	}
	principal := biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}
	sessionID := uuid.New()
	triggerMessageID := uuid.New()
	triggerAt := time.Now().UTC()
	sessionRepo.EXPECT().GetSession(gomock.Any(), principal.UserID, sessionID).Return(biz.Session{ID: sessionID, UserID: principal.UserID, Status: biz.SessionActive}, nil)
	sessionRepo.EXPECT().ListMessages(gomock.Any(), principal.UserID, sessionID, gomock.Any(), "").Return(
		biz.Page[biz.Message]{Items: []biz.Message{{ID: triggerMessageID, Role: biz.MessageUser, CreatedAt: triggerAt}}}, nil,
	)
	runRepo.EXPECT().CreateRun(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, run biz.Run) (biz.Run, error) {
			if run.Status != biz.RunQueued || run.Mode != biz.AgentModeReact || run.DeadlineAt == nil {
				t.Fatalf("invalid run defaults: %#v", run)
			}
			if run.TriggerMessageID == nil || *run.TriggerMessageID != triggerMessageID || run.TriggerMessageAt == nil {
				t.Fatalf("trigger message boundary was not frozen: %#v", run)
			}
			return run, nil
		},
	)
	created, err := service.Create(context.Background(), principal, biz.CreateRunInput{SessionID: sessionID, Mode: biz.AgentModeReact})
	if err != nil {
		t.Fatal(err)
	}
	if created.UserID != principal.UserID {
		t.Fatalf("run used wrong identity: %#v", created)
	}

	// 已归档会话不得创建 Run。
	sessionRepo.EXPECT().GetSession(gomock.Any(), principal.UserID, sessionID).Return(biz.Session{ID: sessionID, Status: biz.SessionArchived}, nil)
	if _, err := service.Create(context.Background(), principal, biz.CreateRunInput{SessionID: sessionID, Mode: biz.AgentModeReact}); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("archived session error = %v", err)
	}
	// 非法模式与 Agent 版本成对校验。
	sessionRepo.EXPECT().GetSession(gomock.Any(), principal.UserID, sessionID).Return(biz.Session{ID: sessionID, Status: biz.SessionActive}, nil)
	if _, err := service.Create(context.Background(), principal, biz.CreateRunInput{SessionID: sessionID, Mode: "unknown"}); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("invalid mode error = %v", err)
	}
	agentID := uuid.New()
	sessionRepo.EXPECT().GetSession(gomock.Any(), principal.UserID, sessionID).Return(biz.Session{ID: sessionID, Status: biz.SessionActive}, nil)
	if _, err := service.Create(context.Background(), principal, biz.CreateRunInput{SessionID: sessionID, Mode: biz.AgentModeReact, AgentID: &agentID}); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("agent version pair error = %v", err)
	}
	if _, err := service.Create(context.Background(), biz.Principal{}, biz.CreateRunInput{SessionID: sessionID}); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("anonymous create error = %v", err)
	}
	if _, err := biz.NewRunService(nil, sessionRepo); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("nil dependency error = %v", err)
	}
}

// TestRunServiceCancelAndEvents 验证取消状态机与事件读取上限。
func TestRunServiceCancelAndEvents(t *testing.T) {
	ctrl := gomock.NewController(t)
	runRepo := mocks.NewMockRunRepository(ctrl)
	sessionRepo := mocks.NewMockSessionRepository(ctrl)
	service, _ := biz.NewRunService(runRepo, sessionRepo)
	principal := biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}
	runID := uuid.New()
	runRepo.EXPECT().GetRun(gomock.Any(), principal.UserID, runID).Return(biz.Run{ID: runID, UserID: principal.UserID, Status: biz.RunQueued}, nil)
	runRepo.EXPECT().TransitionRun(gomock.Any(), principal.UserID, runID, biz.RunQueued, biz.RunCancelled, gomock.Any()).DoAndReturn(
		func(_ context.Context, _ uuid.UUID, _ uuid.UUID, _, _ biz.RunStatus, fields map[string]any) (biz.Run, error) {
			if _, exists := fields["finished_at"]; !exists {
				t.Fatal("cancellation must set finished_at")
			}
			return biz.Run{ID: runID, Status: biz.RunCancelled}, nil
		},
	)
	cancelled, err := service.Cancel(context.Background(), principal, runID)
	if err != nil || cancelled.Status != biz.RunCancelled {
		t.Fatalf("Cancel() = %#v, %v", cancelled, err)
	}
	runRepo.EXPECT().GetRun(gomock.Any(), principal.UserID, runID).Return(biz.Run{ID: runID, Status: biz.RunCompleted}, nil)
	if _, err := service.Cancel(context.Background(), principal, runID); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("terminal cancel error = %v", err)
	}

	events := []biz.RunEvent{{RunID: runID, Seq: 1, EventType: "meta"}}
	runRepo.EXPECT().GetRun(gomock.Any(), principal.UserID, runID).Return(biz.Run{ID: runID}, nil)
	runRepo.EXPECT().ListEvents(gomock.Any(), principal.UserID, runID, int64(0), 200).Return(events, nil)
	if _, err := service.ListEvents(context.Background(), principal, runID, -1, 0); err != nil {
		t.Fatal(err)
	}
	runRepo.EXPECT().GetRun(gomock.Any(), principal.UserID, runID).Return(biz.Run{ID: runID}, nil)
	runRepo.EXPECT().ListEvents(gomock.Any(), principal.UserID, runID, int64(5), 10).Return(events, nil)
	if _, err := service.ListEvents(context.Background(), principal, runID, 5, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ListEvents(context.Background(), biz.Principal{}, runID, 0, 10); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("anonymous list events error = %v", err)
	}

	runRepo.EXPECT().GetRun(gomock.Any(), principal.UserID, runID).Return(biz.Run{ID: runID}, nil)
	runRepo.EXPECT().ListRuns(gomock.Any(), principal.UserID, runID, 20, "").Return(biz.Page[biz.Run]{Items: []biz.Run{{ID: runID}}}, nil)
	sessionRepo.EXPECT().GetSession(gomock.Any(), principal.UserID, runID).Return(biz.Session{ID: runID, Status: biz.SessionActive}, nil)
	if _, err := service.List(context.Background(), principal, runID, 20, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Get(context.Background(), principal, runID); err != nil {
		t.Fatal(err)
	}
}

// TestRunServiceResolveReconciliation 验证人工对账只能从对账状态结案，并强制写入审计事件。
func TestRunServiceResolveReconciliation(t *testing.T) {
	ctrl := gomock.NewController(t)
	runRepo := mocks.NewMockRunRepository(ctrl)
	sessionRepo := mocks.NewMockSessionRepository(ctrl)
	service, _ := biz.NewRunService(runRepo, sessionRepo)
	principal := biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}
	runID := uuid.New()
	runRepo.EXPECT().GetRun(gomock.Any(), principal.UserID, runID).
		Return(biz.Run{ID: runID, UserID: principal.UserID, Status: biz.RunReconciliationRequired}, nil)
	runRepo.EXPECT().TransitionRun(gomock.Any(), principal.UserID, runID,
		biz.RunReconciliationRequired, biz.RunCompleted, gomock.Any()).DoAndReturn(
		func(_ context.Context, _ uuid.UUID, _ uuid.UUID, _, to biz.RunStatus, fields map[string]any) (biz.Run, error) {
			if to != biz.RunCompleted || fields["error_code"] != "reconciliation_completed" {
				t.Fatalf("unexpected reconciliation fields: %#v", fields)
			}
			return biz.Run{ID: runID, Status: biz.RunCompleted}, nil
		},
	)
	runRepo.EXPECT().AppendEvent(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, event biz.RunEvent) (biz.RunEvent, error) {
			if event.EventType != "run.reconciliation.resolved" {
				t.Fatalf("event type = %q", event.EventType)
			}
			return event, nil
		},
	)
	resolved, err := service.ResolveReconciliation(context.Background(), principal, runID, "completed", "已确认下游执行成功")
	if err != nil || resolved.Status != biz.RunCompleted {
		t.Fatalf("ResolveReconciliation() = %#v, %v", resolved, err)
	}

	runRepo.EXPECT().GetRun(gomock.Any(), principal.UserID, runID).
		Return(biz.Run{ID: runID, UserID: principal.UserID, Status: biz.RunRunning}, nil)
	if _, err := service.ResolveReconciliation(context.Background(), principal, runID, "completed", "已确认下游执行成功"); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("non-reconciliation error = %v", err)
	}
	if _, err := service.ResolveReconciliation(context.Background(), principal, runID, "unknown", "已确认下游执行成功"); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("invalid outcome error = %v", err)
	}
}

// TestRunServiceAdoptsPausedPlanOnNewRun 验证同一会话新建 Plan Run 时会续跑暂停中的计划，
// 而不是丢弃既有进度重新规划。
func TestRunServiceAdoptsPausedPlanOnNewRun(t *testing.T) {
	ctrl := gomock.NewController(t)
	runRepo := mocks.NewMockRunRepository(ctrl)
	sessionRepo := mocks.NewMockSessionRepository(ctrl)
	planRepo := mocks.NewMockPlanRepository(ctrl)
	service, err := biz.NewRunService(runRepo, sessionRepo, biz.WithPlanResume(planRepo))
	if err != nil {
		t.Fatal(err)
	}
	principal := biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}
	sessionID := uuid.New()
	oldRunID := uuid.New()
	triggerMessageID := uuid.New()
	sessionRepo.EXPECT().GetSession(gomock.Any(), principal.UserID, sessionID).
		Return(biz.Session{ID: sessionID, UserID: principal.UserID, Status: biz.SessionActive}, nil)
	sessionRepo.EXPECT().ListMessages(gomock.Any(), principal.UserID, sessionID, gomock.Any(), "").
		Return(biz.Page[biz.Message]{Items: []biz.Message{{ID: triggerMessageID, Role: biz.MessageUser, Content: "继续", CreatedAt: time.Now().UTC()}}}, nil).
		Times(2)
	var createdRun biz.Run
	runRepo.EXPECT().CreateRun(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, run biz.Run) (biz.Run, error) {
			createdRun = run
			return run, nil
		},
	)
	plan := biz.Plan{ID: uuid.New(), UserID: principal.UserID, SessionID: sessionID, RunID: oldRunID, Revision: 3, Status: biz.PlanPaused}
	planRepo.EXPECT().FindPausedPlanBySession(gomock.Any(), principal.UserID, sessionID).Return(plan, nil)
	runRepo.EXPECT().ListRuns(gomock.Any(), principal.UserID, sessionID, 20, "").DoAndReturn(
		func(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ int, _ string) (biz.Page[biz.Run], error) {
			return biz.Page[biz.Run]{Items: []biz.Run{{ID: createdRun.ID, Mode: biz.AgentModePlan}, {ID: oldRunID, Mode: biz.AgentModePlan}}}, nil
		},
	)
	runRepo.EXPECT().GetRun(gomock.Any(), principal.UserID, oldRunID).
		Return(biz.Run{ID: oldRunID, UserID: principal.UserID, Status: biz.RunRunning}, nil)
	runRepo.EXPECT().TransitionRun(gomock.Any(), principal.UserID, oldRunID, biz.RunRunning, biz.RunCancelled, gomock.Any()).
		Return(biz.Run{ID: oldRunID, UserID: principal.UserID, Status: biz.RunCancelled}, nil)
	planRepo.EXPECT().RebindPlanRun(gomock.Any(), principal.UserID, plan.ID, gomock.Any(), int64(3), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ uuid.UUID, _ uuid.UUID, runID uuid.UUID, _ int64, _ *time.Time, _ time.Time) (biz.Plan, error) {
			if runID != createdRun.ID {
				t.Fatalf("plan rebound to %s, want new run %s", runID, createdRun.ID)
			}
			return plan, nil
		})
	runRepo.EXPECT().AppendEvent(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, event biz.RunEvent) (biz.RunEvent, error) {
			if event.EventType != "plan.resumed" {
				t.Fatalf("resume event = %q", event.EventType)
			}
			return event, nil
		},
	)
	if _, err := service.Create(context.Background(), principal, biz.CreateRunInput{SessionID: sessionID, Mode: biz.AgentModePlan}); err != nil {
		t.Fatal(err)
	}
}

// TestRecordPayloadAccessAuditsReason 验证载荷查阅理由校验与审计事件写入。
func TestRecordPayloadAccessAuditsReason(t *testing.T) {
	ctrl := gomock.NewController(t)
	runRepo := mocks.NewMockRunRepository(ctrl)
	sessionRepo := mocks.NewMockSessionRepository(ctrl)
	service, err := biz.NewRunService(runRepo, sessionRepo)
	if err != nil {
		t.Fatal(err)
	}
	runID := uuid.New()
	if err := service.RecordPayloadAccess(context.Background(), biz.Principal{}, runID, nil, "review failure"); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("anonymous access error = %v", err)
	}
	principal := biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}
	if err := service.RecordPayloadAccess(context.Background(), principal, runID, nil, "短"); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("short reason error = %v", err)
	}
	stepID := uuid.New()
	runRepo.EXPECT().GetRun(gomock.Any(), principal.UserID, runID).Return(biz.Run{ID: runID, UserID: principal.UserID}, nil)
	runRepo.EXPECT().AppendEvent(gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, event biz.RunEvent) (biz.RunEvent, error) {
		if event.EventType != "audit.payload.viewed" || event.Payload["reason"] != "review failure" || event.Payload["step_id"] != stepID.String() {
			t.Fatalf("unexpected audit event: %#v", event)
		}
		return event, nil
	})
	if err := service.RecordPayloadAccess(context.Background(), principal, runID, &stepID, "  review failure  "); err != nil {
		t.Fatalf("RecordPayloadAccess() error = %v", err)
	}
}

// TestRunServiceListModelCalls 验证模型尝试查询的鉴权、归属校验与空仓储行为。
func TestRunServiceListModelCalls(t *testing.T) {
	ctrl := gomock.NewController(t)
	runRepo := mocks.NewMockRunRepository(ctrl)
	sessionRepo := mocks.NewMockSessionRepository(ctrl)
	calls := mocks.NewMockModelCallRepository(ctrl)
	service, err := biz.NewRunService(runRepo, sessionRepo, biz.WithModelCalls(calls))
	if err != nil {
		t.Fatal(err)
	}
	runID := uuid.New()
	if _, err := service.ListModelCalls(context.Background(), biz.Principal{}, runID); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("anonymous list error = %v", err)
	}
	principal := biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}
	runRepo.EXPECT().GetRun(gomock.Any(), principal.UserID, runID).Return(biz.Run{ID: runID, UserID: principal.UserID}, nil)
	calls.EXPECT().ListModelCalls(gomock.Any(), principal.UserID, runID, 200).Return([]biz.ModelCall{{AttemptNo: 1}}, nil)
	items, err := service.ListModelCalls(context.Background(), principal, runID)
	if err != nil || len(items) != 1 || items[0].AttemptNo != 1 {
		t.Fatalf("ListModelCalls() = %#v, %v", items, err)
	}
	// 未装配 Attempt 仓储时返回空列表而不是错误。
	runRepo.EXPECT().GetRun(gomock.Any(), principal.UserID, runID).Return(biz.Run{ID: runID, UserID: principal.UserID}, nil)
	noCalls, err := biz.NewRunService(runRepo, sessionRepo)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := noCalls.ListModelCalls(context.Background(), principal, runID)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty ListModelCalls() = %#v, %v", empty, err)
	}
}

// TestRunServiceStatus 验证轻量状态查询的匿名拒绝与用户隔离。
func TestRunServiceStatus(t *testing.T) {
	ctrl := gomock.NewController(t)
	runRepo := mocks.NewMockRunRepository(ctrl)
	sessionRepo := mocks.NewMockSessionRepository(ctrl)
	service, err := biz.NewRunService(runRepo, sessionRepo)
	if err != nil {
		t.Fatal(err)
	}
	runID := uuid.New()
	if _, err := service.Status(context.Background(), biz.Principal{}, runID); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("anonymous status error = %v", err)
	}
	principal := biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}
	runRepo.EXPECT().GetRunStatus(gomock.Any(), principal.UserID, runID).Return(biz.RunCompleted, nil)
	status, err := service.Status(context.Background(), principal, runID)
	if err != nil || status != biz.RunCompleted {
		t.Fatalf("Status() = %q, %v", status, err)
	}
}
