package biz

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
	"github.com/Mark-Panda/harness-agent/pkg/mcp"
	"github.com/google/uuid"
)

type approvalExecutionFaults struct {
	*fakeApprovalRepository
	claimErr, readErr, finishErr error
}

type toolWithErrorOutput struct{ *fakeRuntimeTool }

func (t toolWithErrorOutput) Invoke(context.Context, json.RawMessage) (json.RawMessage, error) {
	t.calls++
	return t.output, t.err
}

func (r approvalExecutionFaults) ClaimApprovalExecution(ctx context.Context, userID, runID, approvalID uuid.UUID, fence RunFence, at time.Time) (ToolApproval, error) {
	if r.claimErr != nil {
		return ToolApproval{}, r.claimErr
	}
	return r.fakeApprovalRepository.ClaimApprovalExecution(ctx, userID, runID, approvalID, fence, at)
}

func (r approvalExecutionFaults) GetApproval(ctx context.Context, userID, approvalID uuid.UUID) (ToolApproval, error) {
	if r.readErr != nil {
		return ToolApproval{}, r.readErr
	}
	return r.fakeApprovalRepository.GetApproval(ctx, userID, approvalID)
}

func (r approvalExecutionFaults) FinishApprovalExecution(ctx context.Context, userID, approvalID uuid.UUID, status ApprovalStatus,
	result json.RawMessage, code, message string, at time.Time,
) (ToolApproval, error) {
	if r.finishErr != nil {
		return ToolApproval{}, r.finishErr
	}
	return r.fakeApprovalRepository.FinishApprovalExecution(ctx, userID, approvalID, status, result, code, message, at)
}

func TestApprovedToolRecoveryNeverRepeatsAnUncertainInvocation(t *testing.T) {
	databaseErr := errors.New("database unavailable")
	now := time.Now()
	for _, tc := range []struct {
		name                                           string
		status                                         ApprovalStatus
		claimErr, readErr, finishErr, toolErr, wantErr error
		missing, delivered                             bool
		wantCalls                                      int
		wantCode                                       string
	}{
		{name: "claim failed", status: ApprovalApproved, claimErr: databaseErr, wantErr: databaseErr},
		{name: "claim lost and approval read failed", status: ApprovalApproved, claimErr: ErrResourceConflict, readErr: databaseErr, wantErr: databaseErr},
		{name: "crash after claim", status: ApprovalExecuting, wantErr: ErrApprovalExecutionUnknown},
		{name: "completed needs delivery", status: ApprovalExecuted},
		{name: "already delivered", status: ApprovalExecuted, delivered: true},
		{name: "tool removed before execution", status: ApprovalApproved, missing: true, wantCode: "tool_unavailable"},
		{name: "tool timeout", status: ApprovalApproved, toolErr: context.DeadlineExceeded, wantCalls: 1, wantCode: "tool_timeout"},
		{name: "tool business error", status: ApprovalApproved, toolErr: mcp.ErrToolBusiness, wantCalls: 1, wantCode: "tool_business_error"},
		{name: "tool completed but settlement failed", status: ApprovalApproved, finishErr: databaseErr, wantCalls: 1, wantErr: ErrApprovalExecutionUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New()}
			stepID := uuid.New()
			target := mcp.ToolIdentity{ServerID: uuid.New(), OriginalName: "write", EndpointFingerprint: "endpoint"}
			approval := ToolApproval{ID: uuid.New(), UserID: run.UserID, RunID: run.ID, StepID: &stepID,
				ToolName: "write", ToolCallID: "call_write", Status: tc.status, Arguments: json.RawMessage(`{}`), MCPBinding: target}
			if tc.delivered {
				approval.DeliveredAt = &now
			}
			base := &fakeApprovalRepository{approvals: []ToolApproval{approval}}
			worker := newObservedWorker(&fakeStepRepository{})
			worker.sessions = fakeSessionRepository{}
			worker.runs = stubRunRepository{}
			worker.approvals = approvalExecutionFaults{base, tc.claimErr, tc.readErr, tc.finishErr}
			worker.approvalDelivery = testAtomicApprovalDelivery{
				sessions: worker.sessions, runs: worker.runs, steps: worker.steps, approvals: worker.approvals,
			}
			tool := &fakeRuntimeTool{name: "write", err: tc.toolErr, output: json.RawMessage(`{"detail":"remote"}`)}
			var registry react.Registry = singleToolRegistry{tool: approvalBoundTestTool{Tool: toolWithErrorOutput{tool}, target: target}}
			if tc.missing {
				registry = emptyToolRegistry{}
			}
			err := worker.executeApprovedTool(context.Background(), run, RunFence{}, registry, approval)
			if !errors.Is(err, tc.wantErr) || tool.calls != tc.wantCalls {
				t.Fatalf("ExecuteApprovedTool error=%v calls=%d; want %v and %d", err, tool.calls, tc.wantErr, tc.wantCalls)
			}
			if got := base.approvals[0]; tc.wantCode != "" && (got.ErrorCode != tc.wantCode || got.DeliveredAt == nil) {
				t.Fatalf("tool observation was not delivered: %+v", got)
			}
		})
	}
}

type failedMessageWriter struct{ SessionRepository }

func (failedMessageWriter) AppendMessageIdempotent(context.Context, Message, OutboxEvent, RunFence) (Message, error) {
	return Message{}, errors.New("message write failed")
}

func TestToolTranscriptWriteFailureRequiresReconciliation(t *testing.T) {
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New()}
	worker := &ExecutionWorker{sessions: failedMessageWriter{}, runs: stubRunRepository{}}
	for _, role := range []MessageRole{MessageAssistant, MessageTool} {
		ctx, stop := context.WithCancelCause(context.Background())
		state := &runEventSink{cancel: stop}
		worker.runSink(ctx, run, RunFence{}, state)(react.StepEvent{Type: "message.created", Payload: map[string]any{
			"index": 0, "role": string(role), "content": "result",
		}})
		if err := state.Err(); err == nil || errors.Is(err, ErrToolResultPersistence) != (role == MessageTool) || ctx.Err() == nil {
			t.Fatalf("role %s sink error=%v context=%v", role, err, ctx.Err())
		}
	}
}

// TestPromptBudgetReservesModelOutputAndToolDefinitions 验证 Prompt 预算为输出与工具定义预留空间。
func TestPromptBudgetReservesModelOutputAndToolDefinitions(t *testing.T) {
	worker := &ExecutionWorker{config: WorkerConfig{PromptTokenBudget: 1000, ReserveOutputTokens: 100, SafetyMarginTokens: 50}, logger: slog.Default()}
	for _, tc := range []struct {
		window      int64
		tools, want int
	}{
		{0, 100, 900}, {500, 100, 350}, {100, 100, 900}, {5000, 100, 900},
	} {
		if got := worker.promptBudgetWithWindow(tc.window, tc.tools); got != tc.want {
			t.Fatalf("window %d tools %d = %d, want %d", tc.window, tc.tools, got, tc.want)
		}
	}
}

type approvalPauseRun struct {
	stubRunRepository
	current             RunStatus
	pauseErr, resumeErr error
	resumes             int
}

func (r *approvalPauseRun) PauseRun(context.Context, uuid.UUID, uuid.UUID, RunFence, time.Time) (Run, error) {
	return Run{Status: RunWaitingApproval}, r.pauseErr
}

func (r *approvalPauseRun) GetRun(context.Context, uuid.UUID, uuid.UUID) (Run, error) {
	return Run{Status: r.current}, nil
}

func (r *approvalPauseRun) ResumeRun(context.Context, uuid.UUID, uuid.UUID, time.Time) (Run, error) {
	r.resumes++
	return Run{Status: RunRunning}, r.resumeErr
}

type pendingApprovals struct {
	ToolApprovalRepository
	count int
	err   error
}

func (r pendingApprovals) CountPendingApprovals(context.Context, uuid.UUID, uuid.UUID) (int, error) {
	return r.count, r.err
}

func TestApprovalPauseAndDecisionRace(t *testing.T) {
	fault := errors.New("database unavailable")
	for _, tc := range []struct {
		name                          string
		current                       RunStatus
		pauseErr, resumeErr, countErr error
		count, wantResumes            int
	}{
		{name: "decision arrived before pause", wantResumes: 1},
		{name: "another worker already paused", current: RunWaitingApproval, pauseErr: ErrResourceConflict, wantResumes: 1},
		{name: "pause write failed", current: RunRunning, pauseErr: fault},
		{name: "approval count unavailable", countErr: fault},
		{name: "pending approvals keep run paused", count: 1},
		{name: "resume write failed", resumeErr: fault, wantResumes: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runs := &approvalPauseRun{current: tc.current, pauseErr: tc.pauseErr, resumeErr: tc.resumeErr}
			worker := &ExecutionWorker{runs: runs, approvals: pendingApprovals{count: tc.count, err: tc.countErr}, clock: time.Now, logger: slog.Default()}
			run := Run{ID: uuid.New(), UserID: uuid.New(), Mode: AgentModeReact}
			worker.pauseForApproval(context.Background(), run, RunFence{})
			if runs.resumes != tc.wantResumes {
				t.Fatalf("resume attempts = %d, want %d", runs.resumes, tc.wantResumes)
			}
		})
	}
}

type projectedHistory struct {
	SessionRepository
	messages []Message
	err      error
}

func (p projectedHistory) ListFeishuGroupRunHistory(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) ([]Message, error) {
	return p.messages, p.err
}

func TestFeishuWorkerReadsOnlyProjectedHistoryAndDisablesTools(t *testing.T) {
	trigger := uuid.New()
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), TriggerMessageID: &trigger,
		ConfigSnapshot: map[string]any{"feishu_shared_group": true}}
	ctx := context.Background()
	worker := &ExecutionWorker{
		sessions:    projectedHistory{messages: []Message{{ID: trigger, Role: MessageUser, Content: "follow-up", SpeakerOpenID: "ou_b"}}},
		hotMessages: &fakeHotStore{hit: true, messages: []Message{{ID: trigger, Role: MessageUser, Content: "untrusted Redis message"}}},
		mcps:        faultMCPRepository{}, logger: slog.Default(), clock: time.Now,
	}
	messages, err := worker.buildMessages(ctx, run)
	if err != nil || len(messages) != 2 || !strings.Contains(messages[1].Content, "ou_b") || strings.Contains(messages[1].Content, "untrusted") {
		t.Fatalf("group input = %+v, %v", messages, err)
	}
	registry, err := worker.prepareMCPTools(ctx, run, RunFence{})
	if err != nil || len(registry.List()) != 0 || worker.resolveMemoryPolicy(ctx, run) != SessionMemoryDisabled {
		t.Fatalf("unsafe group execution setup: registry=%v error=%v", registry, err)
	}
	worker.sessions = projectedHistory{}
	if _, _, err := worker.loadHistory(ctx, run); !errors.Is(err, ErrTriggerMessageMissing) {
		t.Fatalf("missing group trigger = %v", err)
	}
	worker.sessions = fakeSessionRepository{}
	if _, _, err := worker.loadHistory(ctx, run); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("missing projection reader = %v", err)
	}
}

type terminalRunWriter struct {
	stubRunRepository
	finishErr, getErr error
	current           RunStatus
	failure           RunEvent
}

func (r *terminalRunWriter) FinishRunWithEvent(_ context.Context, _, _ uuid.UUID, _, _ RunStatus, _ map[string]any,
	event RunEvent, _ RunFence, _ time.Time,
) (Run, error) {
	r.failure = event
	return Run{}, r.finishErr
}

func (r *terminalRunWriter) GetRun(context.Context, uuid.UUID, uuid.UUID) (Run, error) {
	return Run{Status: r.current}, r.getErr
}

func TestFailRunOnlyIgnoresFenceConflictForSettledRun(t *testing.T) {
	fault := errors.New("database unavailable")
	for _, tc := range []struct {
		name                       string
		current                    RunStatus
		finishErr, getErr, wantErr error
	}{
		{name: "already cancelled", current: RunCancelled, finishErr: ErrResourceConflict},
		{name: "still running", current: RunRunning, finishErr: ErrResourceConflict, wantErr: ErrResourceConflict},
		{name: "cannot verify status", finishErr: ErrResourceConflict, getErr: fault, wantErr: ErrResourceConflict},
		{name: "terminal write failed", finishErr: fault, wantErr: fault},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runs := &terminalRunWriter{finishErr: tc.finishErr, getErr: tc.getErr, current: tc.current}
			worker := &ExecutionWorker{runs: runs, clock: time.Now, logger: slog.Default()}
			run := Run{ID: uuid.New(), UserID: uuid.New()}
			if err := worker.failRun(context.Background(), run, RunFence{}, "execution_failed", strings.Repeat("错", 250)); !errors.Is(err, tc.wantErr) {
				t.Fatalf("failure settlement error = %v, want %v", err, tc.wantErr)
			}
			message, _ := runs.failure.Payload["message"].(string)
			if runs.failure.RunID != run.ID || runs.failure.EventType != "run.failed" || len(message) == 0 || len(message) > 512 {
				t.Fatalf("invalid failure event %+v", runs.failure)
			}
		})
	}
}

func TestResumeDecidedApprovalHandlesConcurrentAndFailedWrites(t *testing.T) {
	fault := errors.New("approval database unavailable")
	for _, tc := range []struct {
		name                string
		count               int
		countErr, resumeErr error
		wantResumed         bool
		wantErr             error
	}{
		{name: "approval status unavailable", countErr: fault, wantErr: fault},
		{name: "approval still pending", count: 1},
		{name: "already resumed by another worker", resumeErr: ErrResourceConflict, wantResumed: true},
		{name: "resume write failed", resumeErr: fault, wantErr: fault},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runs := &approvalPauseRun{resumeErr: tc.resumeErr}
			worker := &ExecutionWorker{runs: runs, approvals: pendingApprovals{count: tc.count, err: tc.countErr}, clock: time.Now}
			resumed, err := worker.resumeDecidedApprovalRun(context.Background(), Run{ID: uuid.New(), UserID: uuid.New()})
			if resumed != tc.wantResumed || !errors.Is(err, tc.wantErr) {
				t.Fatalf("resumeDecidedApprovalRun = %t, %v, want %t, %v", resumed, err, tc.wantResumed, tc.wantErr)
			}
		})
	}
}
