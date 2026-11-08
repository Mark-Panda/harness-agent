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

type replayRunQueue struct {
	*mocks.MockRunEnqueuer
	requeue func(uuid.UUID) error
}

func (q replayRunQueue) RequeueRun(_ context.Context, runID uuid.UUID, _ time.Time) error {
	return q.requeue(runID)
}

func TestRunServiceReplayKeepsTerminalAndReconciliationRunsSettled(t *testing.T) {
	ctx := context.Background()
	principal := biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}
	runID := uuid.New()
	temporary := errors.New("queue unavailable")
	for _, tc := range []struct {
		name     string
		status   biz.RunStatus
		getErr   error
		retryErr error
		wantErr  error
		fallback bool
	}{
		{name: "reset dead job", status: biz.RunRunning},
		{name: "recover legacy queued run without job", status: biz.RunQueued, retryErr: biz.ErrResourceConflict, fallback: true},
		{name: "do not mask queue error", status: biz.RunQueued, retryErr: temporary, wantErr: temporary},
		{name: "reconciliation needs human settlement", status: biz.RunReconciliationRequired, wantErr: biz.ErrResourceConflict},
		{name: "completed", status: biz.RunCompleted, wantErr: biz.ErrResourceConflict},
		{name: "ownership", getErr: biz.ErrResourceNotFound, wantErr: biz.ErrResourceNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			runs := mocks.NewMockRunRepository(ctrl)
			queue := mocks.NewMockRunEnqueuer(ctrl)
			requeued := 0
			svc, err := biz.NewRunService(runs, mocks.NewMockSessionRepository(ctrl), biz.WithRunEnqueuer(replayRunQueue{
				MockRunEnqueuer: queue, requeue: func(id uuid.UUID) error {
					requeued++
					if id != runID {
						t.Fatalf("replayed incorrect run %s", id)
					}
					return tc.retryErr
				},
			}))
			if err != nil {
				t.Fatal(err)
			}
			run := biz.Run{ID: runID, UserID: principal.UserID, Status: tc.status}
			read := runs.EXPECT().GetRun(ctx, principal.UserID, runID).Return(run, tc.getErr)
			if tc.fallback {
				read.Times(2)
				queue.EXPECT().EnqueueRun(ctx, principal.UserID, runID, gomock.Any()).Return(nil)
			}
			if err := svc.Replay(ctx, principal, runID); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Replay error = %v, want %v", err, tc.wantErr)
			}
			wantRequeued := 0
			if tc.getErr == nil && !biz.RunSettled(tc.status) {
				wantRequeued = 1
			}
			if requeued != wantRequeued {
				t.Fatalf("attempted %d requeues, want %d", requeued, wantRequeued)
			}
			if err := svc.Replay(ctx, biz.Principal{}, runID); !errors.Is(err, biz.ErrForbidden) {
				t.Fatalf("anonymous Replay error = %v", err)
			}
		})
	}
}

func TestRunServiceResumeAfterPauseHonorsApprovalGate(t *testing.T) {
	ctx := context.Background()
	principal := biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}
	runID := uuid.New()
	databaseErr := errors.New("database unavailable")
	for _, tc := range []struct {
		name      string
		status    biz.RunStatus
		pending   int
		countErr  error
		resumeErr error
		queueErr  error
		wantErr   error
	}{
		{name: "already settled", status: biz.RunReconciliationRequired, wantErr: biz.ErrResourceConflict},
		{name: "pending approval blocks resume", status: biz.RunWaitingApproval, pending: 1},
		{name: "cannot check approvals", status: biz.RunWaitingApproval, countErr: databaseErr, wantErr: databaseErr},
		{name: "decisions complete", status: biz.RunWaitingApproval},
		{name: "another worker resumes concurrently", status: biz.RunWaitingApproval, resumeErr: biz.ErrResourceConflict},
		{name: "resume write failed", status: biz.RunWaitingApproval, resumeErr: databaseErr, wantErr: databaseErr},
		{name: "enqueue failed", status: biz.RunQueued, queueErr: databaseErr, wantErr: databaseErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			runs := mocks.NewMockRunRepository(ctrl)
			queue := mocks.NewMockRunEnqueuer(ctrl)
			approvals := mocks.NewMockToolApprovalRepository(ctrl)
			svc, err := biz.NewRunService(runs, mocks.NewMockSessionRepository(ctrl),
				biz.WithRunEnqueuer(queue), biz.WithToolApprovals(approvals))
			if err != nil {
				t.Fatal(err)
			}
			run := biz.Run{ID: runID, UserID: principal.UserID, Status: tc.status}
			wantStatus := tc.status
			runs.EXPECT().GetRun(ctx, principal.UserID, runID).Return(run, nil)
			if tc.status == biz.RunWaitingApproval {
				approvals.EXPECT().CountPendingApprovals(ctx, principal.UserID, runID).Return(tc.pending, tc.countErr)
				if tc.countErr == nil && tc.pending == 0 {
					resumed := run
					resumed.Status = biz.RunRunning
					runs.EXPECT().ResumeRun(ctx, principal.UserID, runID, gomock.Any()).Return(resumed, tc.resumeErr)
					if tc.resumeErr == nil {
						wantStatus = biz.RunRunning
					}
				}
			}
			if tc.status == biz.RunQueued || (tc.status == biz.RunWaitingApproval && tc.pending == 0 &&
				tc.countErr == nil && (tc.resumeErr == nil || errors.Is(tc.resumeErr, biz.ErrResourceConflict))) {
				queue.EXPECT().EnqueueRun(ctx, principal.UserID, runID, gomock.Any()).Return(tc.queueErr)
			}
			resumed, err := svc.ResumeAfterPause(ctx, principal, runID)
			if !errors.Is(err, tc.wantErr) || (err == nil && resumed.Status != wantStatus) {
				t.Fatalf("ResumeAfterPause = %s, %v, want %s, %v", resumed.Status, err, wantStatus, tc.wantErr)
			}
		})
	}
}

func TestRunServiceSharedGroupUsesOnlyPublishedModelPolicy(t *testing.T) {
	ctx := context.Background()
	transient := errors.New("repository unavailable")
	for _, tc := range []struct {
		name         string
		source       biz.SessionSource
		projection   bool
		policyStatus biz.PolicyStatus
		policyErr    error
		triggerRole  biz.MessageRole
		rejectInput  bool
		wantErr      error
	}{
		{name: "valid projection", source: biz.SessionSourceFeishu, projection: true, policyStatus: biz.PolicyPublished, triggerRole: biz.MessageUser},
		{name: "web session", source: biz.SessionSourceWeb, wantErr: biz.ErrForbidden},
		{name: "unmarked feishu session", source: biz.SessionSourceFeishu, wantErr: biz.ErrForbidden},
		{name: "wrong trigger role", source: biz.SessionSourceFeishu, projection: true, triggerRole: biz.MessageAssistant, wantErr: biz.ErrInvalidConfig},
		{name: "missing trigger", source: biz.SessionSourceFeishu, projection: true, wantErr: biz.ErrInvalidConfig},
		{name: "draft policy", source: biz.SessionSourceFeishu, projection: true, triggerRole: biz.MessageUser, policyStatus: biz.PolicyDraft, wantErr: biz.ErrResourceConflict},
		{name: "policy lookup", source: biz.SessionSourceFeishu, projection: true, triggerRole: biz.MessageUser, policyErr: transient, wantErr: transient},
		{name: "explicit agent", rejectInput: true, wantErr: biz.ErrInvalidConfig},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			runs := mocks.NewMockRunRepository(ctrl)
			sessions := mocks.NewMockSessionRepository(ctrl)
			queue := mocks.NewMockRunEnqueuer(ctrl)
			snapshots, principal, agentID, _ := publishedSnapshotFixture()
			snapshots.policy.Status, snapshots.policyErr = tc.policyStatus, tc.policyErr
			inTransaction := false
			service, err := biz.NewRunService(runs, sessions, biz.WithRunSnapshots(snapshots), biz.WithRunEnqueuer(queue),
				biz.WithRunDeadline(time.Minute), biz.WithRunCreationTransaction(func(txCtx context.Context, fn func(context.Context) error) error {
					inTransaction = true
					defer func() { inTransaction = false }()
					return fn(txCtx)
				}))
			if err != nil {
				t.Fatal(err)
			}
			triggerID := uuid.New()
			input := biz.CreateRunInput{SessionID: uuid.New(), TriggerMessageID: &triggerID, Mode: biz.AgentModeReact}
			if tc.rejectInput {
				input.AgentID = &agentID
			} else {
				sessions.EXPECT().GetSession(ctx, principal.UserID, input.SessionID).Return(biz.Session{
					ID: input.SessionID, UserID: principal.UserID, Source: tc.source, Status: biz.SessionActive, MemoryPolicy: biz.SessionMemoryGroup,
					FeishuGroupProjection: tc.projection,
				}, nil)
			}
			if tc.projection {
				var page biz.Page[biz.Message]
				if tc.triggerRole != "" {
					page.Items = []biz.Message{{ID: triggerID, Role: tc.triggerRole, CreatedAt: time.Now()}}
				}
				sessions.EXPECT().ListMessages(ctx, principal.UserID, input.SessionID, gomock.Any(), "").Return(page, nil)
			}
			if tc.wantErr == nil {
				runs.EXPECT().CreateRun(ctx, gomock.Any()).DoAndReturn(func(_ context.Context, run biz.Run) (biz.Run, error) {
					if !inTransaction || run.AgentID != nil || run.ConfigSnapshot["feishu_shared_group"] != true ||
						run.ModelPolicySnapshot["policy_id"] != snapshots.policy.ID.String() || run.TriggerMessageID == nil ||
						*run.TriggerMessageID != triggerID || run.UserID != principal.UserID {
						t.Fatalf("unsafe shared run %+v", run)
					}
					return run, nil
				})
				queue.EXPECT().EnqueueRun(ctx, principal.UserID, gomock.Any(), gomock.Any()).
					DoAndReturn(func(context.Context, uuid.UUID, uuid.UUID, time.Time) error {
						if !inTransaction {
							t.Fatal("enqueue outside creation transaction")
						}
						return nil
					})
			}
			if _, err := service.CreateFeishuGroup(ctx, principal, snapshots.policy.ID, input); !errors.Is(err, tc.wantErr) {
				t.Fatalf("CreateFeishuGroup error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestRunServiceTraceViewsEnforceTenantAndAdminAccess(t *testing.T) {
	ctx := context.Background()
	ctrl := gomock.NewController(t)
	runs := mocks.NewMockRunRepository(ctrl)
	sessions := mocks.NewMockSessionRepository(ctrl)
	tasks := mocks.NewMockSupervisorTaskRepository(ctrl)
	service, err := biz.NewRunService(runs, sessions, biz.WithSupervisorTasks(tasks))
	if err != nil {
		t.Fatal(err)
	}
	user := biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}
	admin := biz.Principal{UserID: uuid.New(), Role: biz.RoleAdmin}
	runID := uuid.New()
	filter := biz.AdminRunFilter{UserID: &user.UserID, Source: biz.SessionSourceFeishu}
	if _, err := service.ListAll(ctx, user, filter, 10, ""); !errors.Is(err, biz.ErrForbidden) {
		t.Fatal(err)
	}
	if _, err := service.ListAll(ctx, admin, biz.AdminRunFilter{Source: "unknown"}, 10, ""); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatal(err)
	}
	want := biz.Run{ID: runID, UserID: user.UserID}
	runs.EXPECT().ListAllRuns(ctx, filter, 10, "cursor").Return(biz.Page[biz.RunSummary]{Items: []biz.RunSummary{{Run: want}}}, nil)
	page, err := service.ListAll(ctx, admin, filter, 10, "cursor")
	if err != nil || len(page.Items) != 1 || page.Items[0].Run.ID != runID {
		t.Fatalf("ListAll = %+v, %v", page, err)
	}
	if _, err := service.ListSupervisorTasks(ctx, biz.Principal{}, runID); !errors.Is(err, biz.ErrForbidden) {
		t.Fatal(err)
	}
	runs.EXPECT().GetRun(ctx, admin.UserID, runID).Return(biz.Run{}, biz.ErrResourceNotFound)
	if _, err := service.ListSupervisorTasks(ctx, admin, runID); !errors.Is(err, biz.ErrResourceNotFound) {
		t.Fatal(err)
	}
	runs.EXPECT().GetRun(ctx, user.UserID, runID).Return(want, nil).Times(2)
	tasks.EXPECT().ListTasks(ctx, user.UserID, runID).Return([]biz.SupervisorTask{{ParentRunID: runID}}, nil)
	if list, err := service.ListSupervisorTasks(ctx, user, runID); err != nil || len(list) != 1 {
		t.Fatalf("ListSupervisorTasks = %+v, %v", list, err)
	}
	plain, _ := biz.NewRunService(runs, sessions)
	if list, err := plain.ListSupervisorTasks(ctx, user, runID); err != nil || len(list) != 0 {
		t.Fatalf("unconfigured ListSupervisorTasks = %+v, %v", list, err)
	}
}

func TestRunServiceCancelRevokesOnlyPendingApprovals(t *testing.T) {
	ctx := context.Background()
	ctrl := gomock.NewController(t)
	runs := mocks.NewMockRunRepository(ctrl)
	approvals := mocks.NewMockToolApprovalRepository(ctrl)
	service, err := biz.NewRunService(runs, mocks.NewMockSessionRepository(ctrl), biz.WithToolApprovals(approvals))
	if err != nil {
		t.Fatal(err)
	}
	user := biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}
	run := biz.Run{ID: uuid.New(), UserID: user.UserID, Status: biz.RunWaitingApproval}
	cancelled := run
	cancelled.Status = biz.RunCancelled
	pending := uuid.New()
	runs.EXPECT().GetRun(ctx, user.UserID, run.ID).Return(run, nil)
	runs.EXPECT().TransitionRun(ctx, user.UserID, run.ID, biz.RunWaitingApproval, biz.RunCancelled, gomock.Any()).Return(cancelled, nil)
	approvals.EXPECT().ListApprovals(ctx, user.UserID, run.ID).Return([]biz.ToolApproval{
		{ID: pending, Status: biz.ApprovalPending},
		{ID: uuid.New(), Status: biz.ApprovalExecuting},
		{ID: uuid.New(), Status: biz.ApprovalExecuted},
	}, nil)
	approvals.EXPECT().DecideApproval(ctx, user.UserID, pending, biz.ApprovalDecisionDeny, user.UserID, gomock.Any()).
		Return(biz.ToolApproval{ID: pending, Status: biz.ApprovalDenied}, nil)
	got, err := service.Cancel(ctx, user, run.ID)
	if err != nil || got.Status != biz.RunCancelled {
		t.Fatalf("Cancel = %+v, %v", got, err)
	}
}

func TestRunServiceRefusesResumeWithoutQueueOrApprovalStore(t *testing.T) {
	ctx := context.Background()
	ctrl := gomock.NewController(t)
	runs := mocks.NewMockRunRepository(ctrl)
	service, err := biz.NewRunService(runs, mocks.NewMockSessionRepository(ctrl))
	if err != nil {
		t.Fatal(err)
	}
	user := biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}
	runID := uuid.New()
	if _, err := service.ResumeAfterPause(ctx, biz.Principal{}, runID); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("anonymous resume = %v", err)
	}
	runs.EXPECT().GetRun(ctx, user.UserID, runID).Return(biz.Run{}, biz.ErrResourceNotFound)
	if _, err := service.ResumeAfterPause(ctx, user, runID); !errors.Is(err, biz.ErrResourceNotFound) {
		t.Fatalf("unknown run resume = %v", err)
	}
	runs.EXPECT().GetRun(ctx, user.UserID, runID).Return(biz.Run{Status: biz.RunWaitingApproval}, nil)
	if _, err := service.ResumeAfterPause(ctx, user, runID); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("resume without approval store = %v", err)
	}
	runs.EXPECT().GetRun(ctx, user.UserID, runID).Return(biz.Run{Status: biz.RunQueued}, nil).Times(2)
	if _, err := service.ResumeAfterPause(ctx, user, runID); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("resume without queue = %v", err)
	}
	if err := service.Enqueue(ctx, user, runID); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("enqueue without queue = %v", err)
	}
}

func TestRunServiceDoesNotListRunsForForeignSession(t *testing.T) {
	ctx := context.Background()
	ctrl := gomock.NewController(t)
	runs := mocks.NewMockRunRepository(ctrl)
	sessions := mocks.NewMockSessionRepository(ctrl)
	service, err := biz.NewRunService(runs, sessions)
	if err != nil {
		t.Fatal(err)
	}
	principal := biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}
	sessionID := uuid.New()
	if _, err := service.List(ctx, biz.Principal{}, sessionID, 10, ""); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("anonymous List = %v", err)
	}
	sessions.EXPECT().GetSession(ctx, principal.UserID, sessionID).Return(biz.Session{}, biz.ErrResourceNotFound)
	if _, err := service.List(ctx, principal, sessionID, 10, ""); !errors.Is(err, biz.ErrResourceNotFound) {
		t.Fatalf("foreign session List = %v", err)
	}
}
