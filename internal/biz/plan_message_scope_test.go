package biz

import (
	"testing"

	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
	"github.com/google/uuid"
)

// TestScopePlanStepMessageEventsSeparatesStepAttempts 验证同一步骤重试时消息主键不会复用。
func TestScopePlanStepMessageEventsSeparatesStepAttempts(t *testing.T) {
	stepID := uuid.New()
	var events []react.StepEvent
	sink := func(event react.StepEvent) {
		events = append(events, event)
	}
	messageEvent := react.StepEvent{Type: "message.created", Payload: map[string]any{"index": 0}}

	scopePlanStepMessageEvents(stepID, 1, sink)(messageEvent)
	scopePlanStepMessageEvents(stepID, 1, sink)(messageEvent)
	scopePlanStepMessageEvents(stepID, 2, sink)(messageEvent)

	if len(events) != 3 {
		t.Fatalf("events = %d", len(events))
	}
	first, _ := events[0].Payload["message_id"].(string)
	sameAttempt, _ := events[1].Payload["message_id"].(string)
	nextAttempt, _ := events[2].Payload["message_id"].(string)
	if first == "" || first != sameAttempt {
		t.Fatalf("same attempt must keep idempotent message id: %q vs %q", first, sameAttempt)
	}
	if first == nextAttempt {
		t.Fatalf("different step attempt must not reuse message id: %q", first)
	}
}
