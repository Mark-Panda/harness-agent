package react

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestInfrastructureErrorFormatting(t *testing.T) {
	var nilErr *InfrastructureError
	if got := nilErr.Error(); got != ErrToolInfrastructure.Error() {
		t.Fatalf("nil error text = %q", got)
	}
	if nilErr.Unwrap() != nil {
		t.Fatal("nil error must unwrap to nil")
	}
	base := errors.New("db down")
	wrapped := &InfrastructureError{Op: "start_step", Err: base}
	if got := wrapped.Error(); !strings.Contains(got, "start_step") || !strings.Contains(got, "db down") {
		t.Fatalf("error text = %q", got)
	}
	if !errors.Is(wrapped, ErrToolInfrastructure) {
		t.Fatal("errors.Is must match ErrToolInfrastructure")
	}
	if !errors.Is(wrapped, base) {
		t.Fatal("errors.Is must unwrap to base error")
	}
	noOp := &InfrastructureError{Err: base}
	if strings.Contains(noOp.Error(), "::") || !strings.HasPrefix(noOp.Error(), ErrToolInfrastructure.Error()) {
		t.Fatalf("empty op error text = %q", noOp.Error())
	}
}

func TestNewInfrastructureErrorNil(t *testing.T) {
	if err := NewInfrastructureError("op", nil); err != nil {
		t.Fatalf("nil base error must produce nil, got %v", err)
	}
	err := NewInfrastructureError("save_payload", fmt.Errorf("pg reset"))
	var infra *InfrastructureError
	if !errors.As(err, &infra) || infra.Op != "save_payload" {
		t.Fatalf("errors.As failed: %v", err)
	}
}

func TestApprovalRequiredError(t *testing.T) {
	err := &ApprovalRequiredError{ApprovalID: "a1", ToolCallID: "c1", ToolName: "shell", RiskLevel: "high"}
	text := err.Error()
	if !strings.Contains(text, "shell") || !strings.Contains(text, "high") {
		t.Fatalf("error text = %q", text)
	}
	if strings.Contains(text, "a1") || strings.Contains(text, "c1") {
		t.Fatalf("error text must not leak ids: %q", text)
	}
	if !errors.Is(err, ErrApprovalRequired) {
		t.Fatal("errors.Is must match ErrApprovalRequired")
	}
}

func TestToolCallContextRoundTrip(t *testing.T) {
	if _, ok := ToolCallFromContext(context.Background()); ok {
		t.Fatal("empty context must report missing tool call")
	}
	ctx := WithToolCall(context.Background(), "call-1", "search")
	info, ok := ToolCallFromContext(ctx)
	if !ok || info.ID != "call-1" || info.Name != "search" {
		t.Fatalf("info = %#v ok=%v", info, ok)
	}
	empty := WithToolCall(context.Background(), "", "search")
	if _, ok := ToolCallFromContext(empty); ok {
		t.Fatal("empty call id must be treated as missing")
	}
}

func TestLimitsNormalize(t *testing.T) {
	defaults := DefaultLimits()
	if defaults.MaxSteps <= 0 || defaults.ToolTimeout <= 0 || defaults.Deadline <= 0 ||
		defaults.MaxTotalTokens <= 0 || defaults.MaxCost <= 0 || defaults.DuplicateToolThreshold <= 0 ||
		defaults.MaxToolResultBytes <= 0 {
		t.Fatalf("defaults must be positive: %#v", defaults)
	}
	normalized := (Limits{}).Normalize()
	if normalized != defaults {
		t.Fatalf("zero limits must normalize to defaults: %#v vs %#v", normalized, defaults)
	}
	custom := Limits{MaxSteps: 3, ToolTimeout: time.Second, Deadline: time.Minute,
		MaxTotalTokens: 100, MaxCost: 0.5, DuplicateToolThreshold: 2, MaxToolResultBytes: 128}
	if got := custom.Normalize(); got != custom {
		t.Fatalf("positive limits must be preserved: %#v", got)
	}
	partial := Limits{MaxSteps: 7}.Normalize()
	if partial.MaxSteps != 7 || partial.ToolTimeout != defaults.ToolTimeout {
		t.Fatalf("partial normalize = %#v", partial)
	}
}

func TestInvalidArgumentsError(t *testing.T) {
	var nilErr *InvalidArgumentsError
	if got := nilErr.Error(); got != "invalid tool arguments" {
		t.Fatalf("nil detail text = %q", got)
	}
	empty := &InvalidArgumentsError{}
	if got := empty.Error(); got != "invalid tool arguments" {
		t.Fatalf("empty detail text = %q", got)
	}
	detailed := &InvalidArgumentsError{Detail: "field query required"}
	if got := detailed.Error(); !strings.Contains(got, "field query required") {
		t.Fatalf("detail text = %q", got)
	}
}

func TestEstimateTextTokens(t *testing.T) {
	cases := []struct {
		name string
		text string
		want int
	}{
		{"empty", "", 0},
		{"ascii four chars", "abcd", 1},
		{"ascii rounding", "abcde", 2},
		{"chinese per char", "你好世界", 4},
		{"mixed", "ab你", 1 + 1},
	}
	for _, tc := range cases {
		if got := EstimateTextTokens(tc.text); got != tc.want {
			t.Errorf("%s: got %d want %d", tc.name, got, tc.want)
		}
	}
}
