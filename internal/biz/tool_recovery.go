package biz

import (
	"context"
	"errors"
	"fmt"

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
	completed := make(map[uuid.UUID]bool)
	hasCompleted := false
	for _, step := range steps {
		if step.Status == StepCompleted {
			hasCompleted = true
			if step.ParentStepID != nil {
				completed[*step.ParentStepID] = true
			}
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
	case AgentModePlan:
		if len(completed) == 0 || w.plans == nil {
			return false, nil
		}
		plan, err := w.plans.GetPlanByRun(ctx, run.UserID, run.ID)
		if err != nil {
			return false, err
		}
		for _, step := range plan.Steps {
			if step.Status == PlanStepUnknown ||
				(step.Status == PlanStepRunning && step.ExecutionStepID != nil && completed[*step.ExecutionStepID]) {
				return true, nil
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
