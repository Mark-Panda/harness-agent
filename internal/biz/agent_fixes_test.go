package biz

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestPlanStepApprovalIndexFallsBackToToolCallID 验证未启用执行步骤仓储时，
// 计划步骤仍能通过 tool_call_id 关联到审批，而不是被判为 approval_missing。
func TestPlanStepApprovalIndexFallsBackToToolCallID(t *testing.T) {
	planStepID := uuid.New()
	executionStepID := uuid.New()
	userID, runID := uuid.New(), uuid.New()
	plan := Plan{ID: uuid.New(), Steps: []PlanStep{
		{ID: planStepID, Status: PlanStepWaitingApproval},
		{ID: uuid.New(), Status: PlanStepCompleted, ExecutionStepID: &executionStepID},
	}}

	withoutStep := planStepApprovalIndex(plan, []ToolApproval{{
		ID: uuid.New(), UserID: userID, RunID: runID, ToolCallID: planStepToolCallID(planStepID), Status: ApprovalExecuted,
	}})
	approval, ok := withoutStep[planStepID]
	if !ok || approval.Status != ApprovalExecuted {
		t.Fatalf("approval indexed by tool_call_id = %#v, ok=%v", approval, ok)
	}

	withStep := planStepApprovalIndex(plan, []ToolApproval{{
		ID: uuid.New(), UserID: userID, RunID: runID, StepID: &executionStepID,
		ToolCallID: "legacy-call-id", Status: ApprovalExecuted,
	}})
	if _, ok := withStep[plan.Steps[1].ID]; !ok {
		t.Fatal("approval indexed by execution step id is missing")
	}
}

// TestIsPlanResumeIntent 覆盖礼貌前缀、语气词与否定词，避免续跑意图漏判或误判新任务。
func TestIsPlanResumeIntent(t *testing.T) {
	cases := []struct {
		content string
		want    bool
	}{
		{"继续", true},
		{"请继续吧", true},
		{"那就接着做", true},
		{"继续执行！", true},
		{"麻烦恢复计划", true},
		{"Continue plan", true},
		{"  resume.  ", true},
		{"不要继续", false},
		{"别继续执行", false},
		{"继续开发一个新功能并部署", false},
		{"帮我写一个计划", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isPlanResumeIntent(tc.content); got != tc.want {
			t.Errorf("isPlanResumeIntent(%q) = %v, want %v", tc.content, got, tc.want)
		}
	}
}

// countingRunRepository 记录事件追加次数，并可预置已有事件。
type countingRunRepository struct {
	stubRunRepository
	events   []RunEvent
	appended int
}

func (r *countingRunRepository) ListEvents(context.Context, uuid.UUID, uuid.UUID, int64, int) ([]RunEvent, error) {
	return r.events, nil
}

func (r *countingRunRepository) AppendEventFenced(_ context.Context, event RunEvent, _ RunFence) (RunEvent, error) {
	r.appended++
	r.events = append(r.events, event)
	return event, nil
}

// TestDeliverPlanApprovalResultSkipsSessionMessage 验证计划审批只写事件与投递标记，
// 不向会话写入没有对应 assistant tool_call 的孤立 tool 消息。
func TestDeliverPlanApprovalResultSkipsSessionMessage(t *testing.T) {
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), Mode: AgentModePlan}
	stepID := uuid.New()
	approval := ToolApproval{
		ID: uuid.New(), RunID: run.ID, UserID: run.UserID, ToolCallID: planStepToolCallID(stepID),
		ToolName: "lookup", Status: ApprovalExecuted, ResultJSON: []byte(`{"ok":true}`),
	}
	approvals := &fakeApprovalRepository{approvals: []ToolApproval{approval}}
	runs := &countingRunRepository{}
	worker := &ExecutionWorker{runs: runs, approvals: approvals, clock: func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }}

	if err := worker.deliverPlanApprovalResult(context.Background(), run, RunFence{Owner: "worker", Token: 1}, approval); err != nil {
		t.Fatalf("deliverPlanApprovalResult() error = %v", err)
	}
	if runs.appended != 1 {
		t.Fatalf("appended events = %d, want 1", runs.appended)
	}
	delivered, err := approvals.GetApproval(context.Background(), run.UserID, approval.ID)
	if err != nil || delivered.DeliveredAt == nil {
		t.Fatalf("approval delivered_at = %v, err = %v", delivered.DeliveredAt, err)
	}
	// 重复投递不应重复追加事件。
	if err := worker.deliverPlanApprovalResult(context.Background(), run, RunFence{Owner: "worker", Token: 1}, approval); err != nil {
		t.Fatalf("second deliverPlanApprovalResult() error = %v", err)
	}
	if runs.appended != 1 {
		t.Fatalf("appended events after retry = %d, want 1", runs.appended)
	}
}

// supervisorSessionRecorder 记录子会话创建与消息写入，用于验证重试复用。
type supervisorSessionRecorder struct {
	SessionRepository
	sessions map[uuid.UUID]Session
	created  int
	appended int
}

func (r *supervisorSessionRecorder) GetSession(_ context.Context, userID, sessionID uuid.UUID) (Session, error) {
	if session, ok := r.sessions[sessionID]; ok && session.UserID == userID {
		return session, nil
	}
	return Session{}, ErrResourceNotFound
}

func (r *supervisorSessionRecorder) CreateSession(_ context.Context, session Session, _ OutboxEvent) (Session, error) {
	if r.sessions == nil {
		r.sessions = map[uuid.UUID]Session{}
	}
	if _, ok := r.sessions[session.ID]; ok {
		return Session{}, ErrResourceConflict
	}
	r.sessions[session.ID] = session
	r.created++
	return session, nil
}

func (r *supervisorSessionRecorder) ListMessages(context.Context, uuid.UUID, uuid.UUID, int, string) (Page[Message], error) {
	return Page[Message]{}, nil
}

func (r *supervisorSessionRecorder) AppendMessage(_ context.Context, message Message, _ OutboxEvent) (Message, error) {
	r.appended++
	return message, nil
}

// TestEnsureSupervisorChildSessionReusesDeterministicSession 验证同一 Run 的重试复用同一子会话。
func TestEnsureSupervisorChildSessionReusesDeterministicSession(t *testing.T) {
	agentID := uuid.New()
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), AgentID: &agentID}
	recorder := &supervisorSessionRecorder{}
	worker := &ExecutionWorker{sessions: recorder}

	sessionID := supervisorChildSessionID(run.ID, 1)
	if sessionID != supervisorChildSessionID(run.ID, 1) {
		t.Fatal("supervisor child session id is not deterministic")
	}
	if sessionID == supervisorChildSessionID(run.ID, 2) {
		t.Fatal("different workers must use different child session ids")
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	if _, reused, err := worker.ensureSupervisorChildSession(context.Background(), run, sessionID, "worker", now); err != nil || reused {
		t.Fatalf("first ensure = reused %v, err %v", reused, err)
	}
	if _, reused, err := worker.ensureSupervisorChildSession(context.Background(), run, sessionID, "worker", now); err != nil || !reused {
		t.Fatalf("second ensure = reused %v, err %v", reused, err)
	}
	if recorder.created != 1 {
		t.Fatalf("created child sessions = %d, want 1", recorder.created)
	}
}

// TestEnsureSupervisorChildSessionRejectsArchivedSession 验证归档子会话不会被复用于新的委派。
func TestEnsureSupervisorChildSessionRejectsArchivedSession(t *testing.T) {
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New()}
	sessionID := supervisorChildSessionID(run.ID, 1)
	recorder := &supervisorSessionRecorder{sessions: map[uuid.UUID]Session{
		sessionID: {ID: sessionID, UserID: run.UserID, Status: SessionArchived},
	}}
	worker := &ExecutionWorker{sessions: recorder}
	_, _, err := worker.ensureSupervisorChildSession(context.Background(), run, sessionID, "worker", time.Now().UTC())
	if !errors.Is(err, ErrResourceConflict) {
		t.Fatalf("archived session error = %v, want resource conflict", err)
	}
}
