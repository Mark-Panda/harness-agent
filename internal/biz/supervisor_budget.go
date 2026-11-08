package biz

import (
	"context"
	"sync"

	"github.com/Mark-Panda/harness-agent/pkg/agent/model"
	"github.com/google/uuid"
)

// supervisorBudget tracks settled usage and holds for calls that can run concurrently.
type supervisorBudget struct {
	mu        sync.Mutex
	changed   chan struct{}
	used      supervisorUsage
	held      supervisorUsage
	maxTokens int64
	maxCost   float64
}

func (w *ExecutionWorker) newSupervisorBudget(used supervisorUsage) *supervisorBudget {
	return &supervisorBudget{changed: make(chan struct{}), used: used,
		maxTokens: w.config.SupervisorMaxTokens, maxCost: w.config.SupervisorMaxCost}
}

func (b *supervisorBudget) usage() supervisorUsage {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

func (b *supervisorBudget) ReserveAttempt(ctx context.Context, candidate model.Candidate, inputTokens int64, maxOutput int) (int, func(*model.AttemptRecord) error, error) {
	for {
		if err := ctx.Err(); err != nil {
			return 0, nil, err
		}
		b.mu.Lock()
		capacity := b.outputCapacity(b.used.add(b.held), candidate, inputTokens, maxOutput)
		if capacity > 0 {
			hold := supervisorUsage{
				input: inputTokens, output: int64(capacity),
				cost: float64(inputTokens)*candidate.InputPricePerMillion/1_000_000 + float64(capacity)*candidate.OutputPricePerMillion/1_000_000,
			}
			b.held = b.held.add(hold)
			b.mu.Unlock()
			return capacity, func(record *model.AttemptRecord) error { return b.settle(hold, record) }, nil
		}
		// A worker only waits when released reservations could let its request fit.
		canWait := b.outputCapacity(b.used, candidate, inputTokens, maxOutput) > 0
		changed := b.changed
		b.mu.Unlock()
		if !canWait {
			return 0, nil, ErrSupervisorBudget
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		}
	}
}

func (b *supervisorBudget) settle(hold supervisorUsage, record *model.AttemptRecord) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.held.input -= hold.input
	b.held.output -= hold.output
	b.held.cost -= hold.cost
	if b.held.input == 0 && b.held.output == 0 {
		b.held.cost = 0
	}
	close(b.changed)
	b.changed = make(chan struct{})
	if record == nil {
		return nil
	}
	b.used = b.used.add(supervisorUsage{
		input: int64(record.InputTokens), output: int64(record.OutputTokens), cost: record.EstimatedCost,
	})
	// Provider-reported usage can exceed an estimate. Preserve the actual usage and
	// prevent this response from starting tool calls if the shared limit was breached.
	total := b.used.add(b.held)
	if (b.maxTokens > 0 && total.input+total.output > b.maxTokens) || (b.maxCost > 0 && total.cost > b.maxCost) {
		return ErrSupervisorBudget
	}
	return nil
}

func (b *supervisorBudget) outputCapacity(used supervisorUsage, candidate model.Candidate, input int64, maxOutput int) int {
	if b.maxTokens > 0 {
		maxOutput = int(min(int64(maxOutput), b.maxTokens-used.input-used.output-input))
	}
	if b.maxCost <= 0 || maxOutput <= 0 {
		return maxOutput
	}
	// Find the greatest cap within the remaining cost using the same pricing
	// arithmetic as the provider attempt, including zero-priced output.
	low, high := 0, maxOutput
	for low < high {
		middle := low + (high-low+1)/2
		cost := float64(input)*candidate.InputPricePerMillion/1_000_000 +
			float64(middle)*candidate.OutputPricePerMillion/1_000_000
		if used.cost+cost <= b.maxCost {
			low = middle
		} else {
			high = middle - 1
		}
	}
	return low
}

func (u supervisorUsage) add(other supervisorUsage) supervisorUsage {
	return supervisorUsage{input: u.input + other.input, output: u.output + other.output, cost: u.cost + other.cost}
}

func (w *ExecutionWorker) recoverSupervisorBudget(ctx context.Context, run Run, tasks []SupervisorTask) (*supervisorBudget, error) {
	if reader, ok := w.attempts.(interface {
		SumModelCallUsage(context.Context, uuid.UUID, uuid.UUID) (ModelCallUsage, error)
	}); ok {
		usage, err := reader.SumModelCallUsage(ctx, run.UserID, run.ID)
		if err != nil {
			return nil, err
		}
		if usage.Count > 0 {
			return w.newSupervisorBudget(supervisorUsage{input: usage.InputTokens, output: usage.OutputTokens, cost: usage.EstimatedCost}), nil
		}
	}
	// Older records may only carry usage in events or task results.
	used, hasEvents, err := w.supervisorUsageFromEvents(ctx, run)
	if err != nil {
		return nil, err
	}
	if !hasEvents {
		for _, task := range tasks {
			used = used.add(supervisorUsage{
				input: supervisorInt64(task.Result["input_tokens"]), output: supervisorInt64(task.Result["output_tokens"]),
				cost: eventNumber(task.Result["estimated_cost"]),
			})
		}
	}
	return w.newSupervisorBudget(used), nil
}
