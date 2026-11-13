package biz

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/pkg/agent/model"
	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
	"github.com/Mark-Panda/harness-agent/pkg/mcp"
	"github.com/google/uuid"
)

func TestObservedMCPToolRequiresReconciliationWhenResponseIsLost(t *testing.T) {
	steps := &fakeStepRepository{}
	tool := &fakeRuntimeTool{name: "write", err: errors.Join(mcp.ErrToolOutcomeUnknown, mcp.ErrTransport)}
	wrapper := observedTool{inner: tool, worker: newObservedWorker(steps), run: Run{ID: uuid.New(), UserID: uuid.New()}}
	output, err := wrapper.Invoke(context.Background(), json.RawMessage(`{}`))
	if output != nil || !errors.Is(err, ErrToolResultPersistence) || !errors.Is(err, react.ErrToolInfrastructure) {
		t.Fatalf("tool output=%s error=%v", output, err)
	}
	if len(steps.finished) != 1 || steps.finished[0].Status != StepReconciliationRequired || tool.calls != 1 {
		t.Fatalf("settled steps=%+v tool calls=%d", steps.finished, tool.calls)
	}
}

func TestApprovedMCPToolWithLostResponseStaysUndeliveredAndExecutesOnce(t *testing.T) {
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New()}
	stepID := uuid.New()
	target := mcp.ToolIdentity{ServerID: uuid.New(), OriginalName: "write", EndpointFingerprint: "endpoint"}
	approval := ToolApproval{ID: uuid.New(), UserID: run.UserID, RunID: run.ID, StepID: &stepID,
		ToolName: "write", ToolCallID: "call_write", Status: ApprovalApproved, Arguments: json.RawMessage(`{}`), MCPBinding: target}
	repo := &fakeApprovalRepository{approvals: []ToolApproval{approval}}
	steps := &fakeStepRepository{}
	worker := newObservedWorker(steps)
	worker.sessions = fakeSessionRepository{}
	worker.runs = stubRunRepository{}
	worker.approvals = repo
	tool := &fakeRuntimeTool{name: "write", risk: "high", err: errors.Join(mcp.ErrToolOutcomeUnknown, mcp.ErrTransport)}
	registry := singleToolRegistry{tool: approvalBoundTestTool{Tool: tool, target: target}}

	for range 2 {
		if err := worker.resumeApprovals(context.Background(), run, RunFence{}, registry); !errors.Is(err, ErrApprovalExecutionUnknown) {
			t.Fatalf("resume error = %v", err)
		}
	}
	stored := repo.approvals[0]
	if tool.calls != 1 || stored.Status != ApprovalExecuting || stored.DeliveredAt != nil || steps.finished[0].Status != StepReconciliationRequired {
		t.Fatalf("tool calls=%d approval=%+v steps=%+v", tool.calls, stored, steps.finished)
	}
}

func TestPlanToolResponseLossPreventsRetriesAndReplanning(t *testing.T) {
	for _, nested := range []bool{false, true} {
		name := "direct"
		if nested {
			name = "model-step"
		}
		t.Run(name, func(t *testing.T) {
			var modelCalls atomic.Int32
			worker, plans, _, server := planWorkerFixture(t, func(_ string, w http.ResponseWriter) {
				modelCalls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"function\":{\"name\":\"tool\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
			})
			router, err := model.NewRouter([]model.Candidate{{
				Priority: 1, BaseURL: server.URL, APIKey: "test-key",
				Ref: model.ModelRef{ProviderID: "provider", ModelID: "model", RemoteModelName: "test",
					Tools: true, ContextWindowTokens: 1_000_000, MaxOutputTokens: 64},
			}}, nil, model.RouterConfig{MinContextTokens: 1, MaxOutputTokens: 64}, nil)
			if err != nil {
				t.Fatal(err)
			}
			steps := &fakeStepRepository{failOutputPayload: nested}
			worker.steps = steps
			run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), Mode: AgentModePlan}
			tool := &fakeRuntimeTool{name: "tool", output: json.RawMessage(`{"ok":true}`)}
			plan := Plan{ID: uuid.New(), UserID: run.UserID, RunID: run.ID, Status: PlanExecuting, Version: 1, MaxSteps: 5}
			step := PlanStep{ID: uuid.New(), PlanID: plan.ID, StepNo: 1, Status: PlanStepPending, MaxAttempts: 2}
			if !nested {
				tool.err = errors.Join(mcp.ErrToolOutcomeUnknown, mcp.ErrTransport)
				step.ToolName = "tool"
			}
			plan.Steps = []PlanStep{step}
			plans.seedPlan(plan)
			fence := RunFence{Owner: "plan-worker", Token: 1}
			registry := singleToolRegistry{tool: tool}
			messages := []react.Message{{Role: react.RoleUser, Content: "run the tool"}}
			for range 2 {
				if _, _, err := worker.executePlan(context.Background(), run, fence, router, messages, registry); !errors.Is(err, ErrToolResultPersistence) {
					t.Fatalf("plan error = %v (tool calls = %d)", err, tool.calls)
				}
			}
			stored, _ := plans.GetPlanByRun(context.Background(), run.UserID, run.ID)
			wantModelCalls := int32(0)
			if nested {
				wantModelCalls = 1
			}
			if stored.Status != PlanFailed || stored.Steps[0].Status != PlanStepUnknown || tool.calls != 1 || modelCalls.Load() != wantModelCalls {
				t.Fatalf("plan status=%s step=%s tool calls=%d model calls=%d", stored.Status, stored.Steps[0].Status, tool.calls, modelCalls.Load())
			}
		})
	}
}

type interruptedToolSteps struct {
	*fakeStepRepository
	steps []ExecutionStep
}

func (r interruptedToolSteps) ListSteps(context.Context, uuid.UUID, uuid.UUID, int) ([]ExecutionStep, error) {
	return r.steps, nil
}

func TestExpiredRunPreservesUncertainToolOutcome(t *testing.T) {
	parentID := uuid.New()
	run := Run{ID: uuid.New(), UserID: uuid.New(), AttemptNo: 1}
	worker := newObservedWorker(interruptedToolSteps{steps: []ExecutionStep{
		{ID: uuid.New(), ParentStepID: &parentID, Kind: StepKindTool, Status: StepRunning, RiskLevel: "low"},
	}})
	settlement, err := worker.expiredRunSettlement(context.Background(), run)
	if err != nil || settlement.Status != RunReconciliationRequired {
		t.Fatalf("expired run settlement = %+v, %v", settlement, err)
	}

	run.Mode = AgentModeReact
	worker.steps = interruptedToolSteps{steps: []ExecutionStep{
		{ID: uuid.New(), Kind: StepKindTool, Status: StepCompleted, ToolName: "write",
			IdempotencyKey: approvalIdempotencyKey(run.ID, "call-1")},
	}}
	request := Message{ID: uuid.New(), Role: MessageAssistant, RunID: &run.ID,
		ToolCalls: []map[string]any{{"id": "call-1", "name": "write", "arguments": "{}"}}}
	worker.sessions = fakeSessionRepository{messages: []Message{request}}
	settlement, err = worker.expiredRunSettlement(context.Background(), run)
	if err != nil || settlement.Status != RunReconciliationRequired {
		t.Fatalf("expired run with a missing observation = %+v, %v", settlement, err)
	}
	result := Message{ID: uuid.New(), Role: MessageTool, RunID: &run.ID, ToolCallID: "call-1"}
	worker.sessions = fakeSessionRepository{messages: []Message{result, request}}
	settlement, err = worker.expiredRunSettlement(context.Background(), run)
	if err != nil || settlement.Status != RunFailed {
		t.Fatalf("expired run with a complete transcript = %+v, %v", settlement, err)
	}

	run.Mode = AgentModePlan
	worker.steps = interruptedToolSteps{steps: []ExecutionStep{
		{ID: uuid.New(), ParentStepID: &parentID, Kind: StepKindTool, Status: StepCompleted},
	}}
	plans := newMemoryPlanRepo()
	plans.seedPlan(Plan{ID: uuid.New(), RunID: run.ID, UserID: run.UserID, Status: PlanExecuting,
		Steps: []PlanStep{{ID: uuid.New(), Status: PlanStepRunning, ExecutionStepID: &parentID}}})
	worker.plans = plans
	settlement, err = worker.expiredRunSettlement(context.Background(), run)
	if err != nil || settlement.Status != RunReconciliationRequired {
		t.Fatalf("expired plan with an interrupted model step = %+v, %v", settlement, err)
	}
}

func TestInterruptedPlanModelStepWithExecutedToolRequiresReconciliation(t *testing.T) {
	worker, plans, _, _ := planWorkerFixture(t, func(_ string, _ http.ResponseWriter) {
		t.Error("recovery called the model")
	})
	plan := Plan{ID: uuid.New(), RunID: uuid.New(), UserID: uuid.New(), Status: PlanExecuting, MaxSteps: 5}
	parentID := uuid.New()
	plan.Steps = []PlanStep{{ID: uuid.New(), PlanID: plan.ID, StepNo: 1, Status: PlanStepRunning, ExecutionStepID: &parentID}}
	plans.seedPlan(plan)
	worker.steps = interruptedToolSteps{fakeStepRepository: &fakeStepRepository{}, steps: []ExecutionStep{
		{ID: uuid.New(), ParentStepID: &parentID, Kind: StepKindTool, Status: StepCompleted, RiskLevel: "low"},
	}}
	_, err := worker.recoverPlanSteps(context.Background(), plan, RunFence{Owner: "plan-worker", Token: 1})
	if !errors.Is(err, ErrToolResultPersistence) {
		t.Fatalf("recovery error = %v", err)
	}
	stored, _ := plans.GetPlanByRun(context.Background(), plan.UserID, plan.RunID)
	if stored.Status != PlanFailed || stored.Steps[0].Status != PlanStepUnknown {
		t.Fatalf("recovered plan status=%s step=%s", stored.Status, stored.Steps[0].Status)
	}
}

type reactRecoverySteps struct {
	*fakeStepRepository
	existing []ExecutionStep
}

func (r *reactRecoverySteps) ListSteps(context.Context, uuid.UUID, uuid.UUID, int) ([]ExecutionStep, error) {
	result := append([]ExecutionStep(nil), r.existing...)
	return append(result, r.started...), nil
}

type reactRecoverySessions struct {
	fakeSessionRepository
	messages []Message
}

func (r *reactRecoverySessions) AppendMessageIdempotent(_ context.Context, message Message, _ OutboxEvent, _ RunFence) (Message, error) {
	for _, existing := range r.messages {
		if existing.ID == message.ID {
			return existing, nil
		}
	}
	r.messages = append([]Message{message}, r.messages...)
	return message, nil
}

func (r *reactRecoverySessions) ListMessages(context.Context, uuid.UUID, uuid.UUID, int, string) (Page[Message], error) {
	return Page[Message]{Items: append([]Message(nil), r.messages...)}, nil
}

type reactRecoveryRegistry struct {
	tools []react.Tool
}

func (r reactRecoveryRegistry) Lookup(name string) (react.Tool, bool) {
	for _, tool := range r.tools {
		if tool.Spec().Name == name {
			return tool, true
		}
	}
	return nil, false
}

func (r reactRecoveryRegistry) List() []react.ToolSpec {
	specs := make([]react.ToolSpec, 0, len(r.tools))
	for _, tool := range r.tools {
		specs = append(specs, tool.Spec())
	}
	return specs
}

type reactRecoveryRuns struct {
	stubRunRepository
	dispatchCalls int
	events        []RunEvent
}

func (r *reactRecoveryRuns) AuthorizeToolDispatch(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID, RunFence, time.Time) error {
	r.dispatchCalls++
	return nil
}

func (r *reactRecoveryRuns) AppendEventFenced(_ context.Context, event RunEvent, _ RunFence) (RunEvent, error) {
	r.events = append(r.events, event)
	return event, nil
}

func reactRecoveryFixture(callName, arguments string) (Run, *reactRecoverySessions) {
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), Mode: AgentModeReact, AttemptNo: 1}
	assistant := Message{ID: uuid.New(), RunID: &run.ID, Role: MessageAssistant, ToolCalls: []map[string]any{{
		"id": "call-1", "name": callName, "arguments": arguments,
	}}}
	return run, &reactRecoverySessions{messages: []Message{assistant, {ID: uuid.New(), Role: MessageUser, Content: "run the tool"}}}
}

func TestReactRecoveryExecutesUndispatchedCallOnceAndPersistsObservation(t *testing.T) {
	arguments := `{ "query": "preserve raw arguments" }`
	run, sessions := reactRecoveryFixture("write", arguments)
	steps := &reactRecoverySteps{fakeStepRepository: &fakeStepRepository{}}
	runs := &reactRecoveryRuns{}
	tool := &fakeRuntimeTool{name: "write", output: json.RawMessage(`{"ok":true}`)}
	worker := newObservedWorker(steps)
	worker.sessions = sessions
	worker.runs = runs
	fence := RunFence{Owner: "worker", Token: 1}

	for range 2 {
		if err := worker.recoverUndispatchedReactTools(context.Background(), run, fence, singleToolRegistry{tool: tool}); err != nil {
			t.Fatalf("recovery error = %v", err)
		}
	}
	if tool.calls != 1 || runs.dispatchCalls != 1 || len(steps.started) != 1 {
		t.Fatalf("tool calls=%d dispatch=%d starts=%d", tool.calls, runs.dispatchCalls, len(steps.started))
	}
	if string(tool.arguments[0]) != arguments || tool.idempotencyKeys[0] != approvalIdempotencyKey(run.ID, "call-1") {
		t.Fatalf("arguments=%s idempotency=%s", tool.arguments[0], tool.idempotencyKeys[0])
	}
	messages, err := worker.buildMessages(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, message := range messages {
		if message.Role == react.RoleTool && message.ToolCallID == "call-1" {
			found = message.Content == `{"ok":true}` && message.Content != `{"error":"tool_result_missing"}`
		}
	}
	if !found {
		t.Fatalf("recovered observation is missing: %#v", messages)
	}
}

func TestReactRecoveryDoesNotRerunCompletedCallWithoutObservation(t *testing.T) {
	run, sessions := reactRecoveryFixture("write", `{}`)
	steps := &reactRecoverySteps{fakeStepRepository: &fakeStepRepository{}, existing: []ExecutionStep{{
		ID: uuid.New(), RunID: run.ID, UserID: run.UserID, Kind: StepKindTool, Status: StepCompleted,
		ToolName: "write", IdempotencyKey: approvalIdempotencyKey(run.ID, "call-1"),
	}}}
	tool := &fakeRuntimeTool{name: "write", output: json.RawMessage(`{"ok":true}`)}
	worker := newObservedWorker(steps)
	worker.sessions = sessions
	worker.runs = &reactRecoveryRuns{}
	err := worker.recoverUndispatchedReactTools(context.Background(), run, RunFence{Owner: "worker", Token: 1}, singleToolRegistry{tool: tool})
	if !errors.Is(err, ErrToolResultPersistence) || tool.calls != 0 || len(steps.started) != 0 {
		t.Fatalf("error=%v tool calls=%d starts=%d", err, tool.calls, len(steps.started))
	}
	if _, err := worker.buildMessages(context.Background(), run); !errors.Is(err, ErrToolResultPersistence) {
		t.Fatalf("buildMessages concealed the completed call: %v", err)
	}
}

func TestReactRecoveryPreflightsWholeAssistantBatchBeforeDispatch(t *testing.T) {
	run, sessions := reactRecoveryFixture("safe", `{}`)
	assistant := sessions.messages[0]
	assistant.ToolCalls = append(assistant.ToolCalls, map[string]any{"id": "call-2", "name": "write", "arguments": `{}`})
	sessions.messages[0] = assistant
	steps := &reactRecoverySteps{fakeStepRepository: &fakeStepRepository{}, existing: []ExecutionStep{{
		ID: uuid.New(), RunID: run.ID, UserID: run.UserID, Kind: StepKindTool, Status: StepCompleted,
		ToolName: "write", IdempotencyKey: approvalIdempotencyKey(run.ID, "call-2"),
	}}}
	safe := &fakeRuntimeTool{name: "safe", output: json.RawMessage(`{"ok":true}`)}
	write := &fakeRuntimeTool{name: "write", output: json.RawMessage(`{"ok":true}`)}
	worker := newObservedWorker(steps)
	worker.sessions = sessions
	worker.runs = &reactRecoveryRuns{}
	registry := reactRecoveryRegistry{tools: []react.Tool{safe, write}}
	err := worker.recoverUndispatchedReactTools(context.Background(), run, RunFence{Owner: "worker", Token: 1}, registry)
	if !errors.Is(err, ErrToolResultPersistence) || safe.calls != 0 || write.calls != 0 {
		t.Fatalf("error=%v safe calls=%d write calls=%d", err, safe.calls, write.calls)
	}
}

func TestReactRecoveryPreservesApprovalGate(t *testing.T) {
	run, sessions := reactRecoveryFixture("delete", `{"id":"x"}`)
	steps := &reactRecoverySteps{fakeStepRepository: &fakeStepRepository{}}
	tool := &fakeRuntimeTool{name: "delete", risk: "high", output: json.RawMessage(`{"ok":true}`)}
	approvals := &fakeApprovalRepository{}
	worker := newObservedWorker(steps)
	worker.sessions = sessions
	worker.runs = &reactRecoveryRuns{}
	worker.approvals = approvals
	err := worker.recoverUndispatchedReactTools(context.Background(), run, RunFence{Owner: "worker", Token: 1}, singleToolRegistry{tool: tool})
	if !errors.Is(err, react.ErrApprovalRequired) || tool.calls != 0 || len(approvals.approvals) != 1 {
		t.Fatalf("error=%v calls=%d approvals=%d", err, tool.calls, len(approvals.approvals))
	}
	if approvals.approvals[0].IdempotencyKey != approvalIdempotencyKey(run.ID, "call-1") || steps.waitingApproval != 1 {
		t.Fatalf("approval=%+v waiting=%d", approvals.approvals[0], steps.waitingApproval)
	}
}
