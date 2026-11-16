package biz

import (
	"context"
	"errors"
	"testing"

	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
)

func TestCompleteTextValidatesAndContinuesOnce(t *testing.T) {
	messages := []react.Message{{Role: react.RoleUser, Content: "question"}}
	calls := 0
	completion, err := completeText(context.Background(), messages, func(_ context.Context, got []react.Message) (react.Completion, error) {
		calls++
		if calls == 1 {
			return react.Completion{Content: "first", FinishReason: "length", Usage: react.Usage{InputTokens: 2, OutputTokens: 3}, EstimatedCost: 0.25}, nil
		}
		if len(got) != 3 || got[1].Content != "first" || !got[1].Ephemeral || !got[2].Ephemeral {
			t.Fatalf("continuation messages = %#v", got)
		}
		return react.Completion{Content: " second", FinishReason: "stop", Usage: react.Usage{InputTokens: 5, OutputTokens: 7}, EstimatedCost: 0.5}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || completion.Content != "first second" || completion.Usage.InputTokens != 7 ||
		completion.Usage.OutputTokens != 10 || completion.EstimatedCost != 0.75 {
		t.Fatalf("completeText() = %#v, calls=%d", completion, calls)
	}
}

func TestCompleteTextRejectsInvalidResponses(t *testing.T) {
	for _, tc := range []struct {
		name      string
		first     react.Completion
		continued react.Completion
		want      error
		wantCalls int
	}{
		{name: "filtered", first: react.Completion{FinishReason: "content_filter"}, want: react.ErrContentFiltered, wantCalls: 1},
		{name: "empty", first: react.Completion{}, want: react.ErrEmptyResponse, wantCalls: 1},
		{name: "reasoning only", first: react.Completion{ReasoningContent: "hidden"}, want: react.ErrReasoningOnly, wantCalls: 1},
		{name: "unexpected tools", first: react.Completion{Content: "{}", ToolCalls: []react.ToolCall{{ID: "call"}}}, want: react.ErrIncompleteToolCall, wantCalls: 1},
		{name: "continuation tools", first: react.Completion{Content: "partial", FinishReason: "length"}, continued: react.Completion{Content: "tail", ToolCalls: []react.ToolCall{{ID: "call"}}}, want: react.ErrIncompleteToolCall, wantCalls: 2},
		{name: "continuation still truncated", first: react.Completion{Content: "partial", FinishReason: "length"}, continued: react.Completion{Content: "tail", FinishReason: "length"}, want: react.ErrStepLimit, wantCalls: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			completion, err := completeText(context.Background(), nil, func(context.Context, []react.Message) (react.Completion, error) {
				calls++
				if calls == 1 {
					return tc.first, nil
				}
				return tc.continued, nil
			})
			if !errors.Is(err, tc.want) || calls != tc.wantCalls {
				t.Fatalf("completeText() = %#v, %v, calls=%d; want %v and %d calls", completion, err, calls, tc.want, tc.wantCalls)
			}
		})
	}
}

func TestCompleteTextRetainsFailedContinuationUsage(t *testing.T) {
	calls := 0
	completion, err := completeText(context.Background(), nil, func(context.Context, []react.Message) (react.Completion, error) {
		calls++
		if calls == 1 {
			return react.Completion{Content: "partial", FinishReason: "length", Usage: react.Usage{InputTokens: 2, OutputTokens: 3}}, nil
		}
		return react.Completion{Usage: react.Usage{InputTokens: 5, OutputTokens: 7}}, react.ErrEmptyResponse
	})
	if !errors.Is(err, react.ErrEmptyResponse) {
		t.Fatalf("completeText() error = %v", err)
	}
	if completion.Usage.InputTokens != 7 || completion.Usage.OutputTokens != 10 {
		t.Fatalf("failed continuation usage was lost: %#v", completion.Usage)
	}
}
