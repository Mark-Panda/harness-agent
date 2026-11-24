package biz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
	"github.com/Mark-Panda/harness-agent/pkg/mcp"
	"github.com/google/uuid"
)

const toolRecoveryStepLimit = 500

type toolRecoveryStepReader interface {
	ListToolStepsForRecovery(ctx context.Context, userID, runID uuid.UUID, parentStepID *uuid.UUID) ([]ExecutionStep, error)
}

// toolStepsForRecovery reads the relevant tool steps without the execution tree display limit.
func (w *ExecutionWorker) toolStepsForRecovery(ctx context.Context, userID, runID uuid.UUID, parentStepID *uuid.UUID) ([]ExecutionStep, bool, error) {
	if reader, ok := w.steps.(toolRecoveryStepReader); ok {
		steps, err := reader.ListToolStepsForRecovery(ctx, userID, runID, parentStepID)
		return steps, true, err
	}
	if w.steps == nil {
		return nil, false, nil
	}
	steps, err := w.steps.ListSteps(ctx, userID, runID, toolRecoveryStepLimit)
	if err != nil {
		return nil, false, err
	}
	var tools []ExecutionStep
	for _, step := range steps {
		if step.Kind == StepKindTool && (parentStepID == nil || (step.ParentStepID != nil && *step.ParentStepID == *parentStepID)) {
			tools = append(tools, step)
		}
	}
	return tools, len(steps) < toolRecoveryStepLimit, nil
}

// expiredRunSettlement preserves unresolved tool effects even when the next Worker
// takes over after the deadline and can no longer resume the execution graph.
func (w *ExecutionWorker) expiredRunSettlement(ctx context.Context, run Run) (RunJobSettlement, error) {
	deadline := RunJobSettlement{Status: RunFailed, Code: "run_deadline_exceeded", Message: "run deadline has passed"}
	if run.AttemptNo == 0 {
		return deadline, nil
	}
	unknown := RunJobSettlement{
		Status: RunReconciliationRequired, Code: "tool_execution_unknown",
		Message: "run deadline passed before a tool outcome could be confirmed",
	}
	if w.approvals != nil {
		approvals, err := w.approvals.ListApprovals(ctx, run.UserID, run.ID)
		if err != nil {
			return RunJobSettlement{}, err
		}
		for _, approval := range approvals {
			if approval.Status == ApprovalExecuting {
				return unknown, nil
			}
		}
	}
	if w.steps != nil {
		steps, complete, err := w.toolStepsForRecovery(ctx, run.UserID, run.ID, nil)
		if err != nil {
			return RunJobSettlement{}, err
		}
		for _, step := range steps {
			if step.Status == StepReconciliationRequired || (step.Status == StepRunning && !approvalRequired(step.RiskLevel)) {
				return unknown, nil
			}
		}
		if !complete {
			return unknown, nil
		}
		missing, err := w.expiredRunMissingToolResult(ctx, run, steps)
		if err != nil {
			return RunJobSettlement{}, err
		}
		if missing {
			return unknown, nil
		}
	}
	return deadline, nil
}

func (w *ExecutionWorker) expiredRunMissingToolResult(ctx context.Context, run Run, steps []ExecutionStep) (bool, error) {
	hasCompleted := false
	for _, step := range steps {
		if step.Status == StepCompleted {
			hasCompleted = true
			break
		}
	}
	if !hasCompleted {
		return false, nil
	}
	switch run.Mode {
	case AgentModeReact:
		history, _, err := w.loadHistory(ctx, run)
		if errors.Is(err, ErrTriggerMessageMissing) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		history = boundHistoryByTrigger(run, history)
		results := indexToolResults(history)
		for index, message := range history {
			if message.Role != MessageAssistant || message.RunID == nil || *message.RunID != run.ID {
				continue
			}
			for _, call := range messageToolCalls(message.ToolCalls) {
				if _, exists := results[index][call.ID]; !exists && missingReactToolResult(run, call.ID, call.Name, steps) != nil {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

// indexToolResults associates a result with its assistant message and call ID.
// A provider can reuse the same call ID in a later model response.
func indexToolResults(history []Message) map[int]map[string]Message {
	results := make(map[int]map[string]Message)
	lastAssistant := make(map[uuid.UUID]int)
	for index, message := range history {
		runID := uuid.Nil
		if message.RunID != nil {
			runID = *message.RunID
		}
		switch message.Role {
		case MessageAssistant:
			lastAssistant[runID] = index
		case MessageTool:
			owner, ok := lastAssistant[runID]
			if !ok || message.ToolCallID == "" {
				continue
			}
			if results[owner] == nil {
				results[owner] = make(map[string]Message)
			}
			results[owner][message.ToolCallID] = message
		}
	}
	return results
}

// recoverInterruptedReactTools blocks a resumed Run before it can dispatch any more tools.
func (w *ExecutionWorker) recoverInterruptedReactTools(ctx context.Context, run Run) error {
	if w.steps == nil {
		return nil
	}
	steps, complete, err := w.toolStepsForRecovery(ctx, run.UserID, run.ID, nil)
	if err != nil {
		return err
	}
	var uncertain error
	for _, step := range steps {
		if step.Status == StepReconciliationRequired {
			uncertain = ErrToolResultPersistence
			continue
		}
		// An approval-gated tool cannot execute from its running step. Approval status is checked separately.
		if step.Status != StepRunning || approvalRequired(step.RiskLevel) {
			continue
		}
		uncertain = ErrToolResultPersistence
		if _, err := w.steps.FinishStep(context.WithoutCancel(ctx), run.UserID, step.ID, StepUpdate{
			Status: StepReconciliationRequired, ErrorCode: "tool_execution_unknown", FinishedAt: w.clock(),
		}); err != nil && !errors.Is(err, ErrResourceConflict) {
			uncertain = errors.Join(uncertain, err)
		}
	}
	if !complete {
		uncertain = errors.Join(ErrToolResultPersistence, uncertain)
	}
	return uncertain
}

type reactToolRecovery struct {
	assistantID uuid.UUID
	call        react.ToolCall
}

// recoverUndispatchedReactTools restores only calls that crashed before StartStep.
// Any persisted step is conservative evidence that dispatch may have happened and requires reconciliation.
func (w *ExecutionWorker) recoverUndispatchedReactTools(ctx context.Context, run Run, fence RunFence, tools react.Registry) error {
	history, _, err := w.loadHistory(ctx, run)
	if err != nil {
		return err
	}
	history = boundHistoryByTrigger(run, history)
	results := indexToolResults(history)
	callOccurrences := make(map[string]int)
	var recovered []reactToolRecovery
	for index, message := range history {
		if message.Role != MessageAssistant || message.RunID == nil || *message.RunID != run.ID {
			continue
		}
		for _, call := range messageToolCalls(message.ToolCalls) {
			callOccurrences[call.ID]++
			if _, exists := results[index][call.ID]; !exists {
				recovered = append(recovered, reactToolRecovery{assistantID: message.ID, call: call})
			}
		}
	}
	if len(recovered) == 0 {
		return nil
	}
	if w.steps == nil {
		return fmt.Errorf("%w: cannot prove that %d tool call(s) were not dispatched", ErrToolResultPersistence, len(recovered))
	}
	steps, complete, err := w.toolStepsForRecovery(ctx, run.UserID, run.ID, nil)
	if err != nil {
		return err
	}
	if !complete {
		return fmt.Errorf("%w: tool recovery history is incomplete", ErrToolResultPersistence)
	}
	// Classify the whole batch before dispatching anything. A mixed safe/unknown batch must not gain new side effects.
	for _, item := range recovered {
		if item.assistantID == uuid.Nil || callOccurrences[item.call.ID] != 1 {
			return fmt.Errorf("%w: tool call %s cannot be associated with one assistant message", ErrToolResultPersistence, item.call.ID)
		}
		key := approvalIdempotencyKey(run.ID, item.call.ID)
		for _, step := range steps {
			if step.ToolName == item.call.Name && (step.IdempotencyKey == key || step.IdempotencyKey == "") {
				return fmt.Errorf("%w: tool %s already has execution step %s in state %s",
					ErrToolResultPersistence, item.call.Name, step.ID, step.Status)
			}
		}
	}
	ctx = context.WithValue(ctx, runToolFenceContextKey{}, fence)
	registry := w.runnerRegistry(tools, run)
	for _, item := range recovered {
		if err := w.recoverUndispatchedReactTool(ctx, run, fence, registry, item); err != nil {
			return err
		}
	}
	return nil
}

func (w *ExecutionWorker) recoverUndispatchedReactTool(ctx context.Context, run Run, fence RunFence, registry react.Registry, item reactToolRecovery) error {
	call := item.call
	tool, exists := registry.Lookup(call.Name)
	if !exists {
		return fmt.Errorf("%w: tool %s is unavailable during recovery", react.ErrUnknownTool, call.Name)
	}
	key := approvalIdempotencyKey(run.ID, call.ID)
	toolContext := mcp.WithIdempotencyKey(ctx, key)
	toolContext = react.WithToolCall(toolContext, call.ID, call.Name)
	output, invokeErr := tool.Invoke(toolContext, call.Arguments)
	status := "completed"
	content := string(output)
	errorMessage := ""
	if invokeErr != nil {
		if errors.Is(invokeErr, react.ErrApprovalRequired) || errors.Is(invokeErr, react.ErrToolInfrastructure) {
			return invokeErr
		}
		if ctxErr := toolContext.Err(); ctxErr != nil {
			return ctxErr
		}
		status = "failed"
		var invalidArguments *react.InvalidArgumentsError
		switch {
		case errors.As(invokeErr, &invalidArguments):
			status = "invalid_arguments"
		case errors.Is(invokeErr, context.DeadlineExceeded):
			status = "timeout"
		}
		errorMessage = sanitizeExecutionError(invokeErr)
		payload := map[string]any{"error": status, "detail": errorMessage}
		if len(output) > 0 {
			payload["output"] = string(output)
		}
		encoded, _ := json.Marshal(payload)
		content = string(encoded)
	}
	eventPayload := map[string]any{
		"tool": call.Name, "tool_call_id": call.ID, "status": status, "recovered": true,
	}
	if errorMessage != "" {
		eventPayload["error"] = errorMessage
	}
	if _, err := w.runs.AppendEventFenced(context.WithoutCancel(ctx), RunEvent{
		RunID: run.ID, UserID: run.UserID, EventType: "tool.completed", Payload: eventPayload,
	}, fence); err != nil {
		return errors.Join(ErrToolResultPersistence, err)
	}
	messageID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("run:%s:assistant:%s:recovered-tool:%s", run.ID, item.assistantID, call.ID)))
	if _, err := w.persistRunnerMessageRecord(ctx, run, fence, map[string]any{
		"index": 0, "message_id": messageID.String(), "role": string(MessageTool),
		"content": content, "tool_call_id": call.ID, "name": call.Name,
	}); err != nil {
		return errors.Join(ErrToolResultPersistence, err)
	}
	return nil
}

func missingReactToolResult(run Run, callID, toolName string, steps []ExecutionStep) error {
	key := approvalIdempotencyKey(run.ID, callID)
	for _, step := range steps {
		// Older low-risk steps only recorded the tool name. Any matching completed call
		// might have dispatched and crashed before its model-facing observation was saved.
		if step.Status == StepCompleted && step.ToolName == toolName &&
			(step.IdempotencyKey == key || step.IdempotencyKey == "") {
			return fmt.Errorf("%w: observation for tool %s is missing", ErrToolResultPersistence, toolName)
		}
	}
	return nil
}
