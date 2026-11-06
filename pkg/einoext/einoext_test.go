package einoext

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
)

type testTool struct {
	spec react.ToolSpec
	err  error
}

func (t testTool) Spec() react.ToolSpec { return t.spec }
func (t testTool) RiskLevel() string    { return "low" }
func (t testTool) Invoke(_ context.Context, arguments json.RawMessage) (json.RawMessage, error) {
	if t.err != nil {
		return nil, t.err
	}
	return json.RawMessage(`{"content":[{"type":"text","text":"ok:"}` + string(arguments) + `}]}`), nil
}

type testRegistry struct{ tools map[string]react.Tool }

func (r testRegistry) Lookup(name string) (react.Tool, bool) {
	tool, ok := r.tools[name]
	return tool, ok
}
func (r testRegistry) List() []react.ToolSpec {
	result := make([]react.ToolSpec, 0, len(r.tools))
	for _, tool := range r.tools {
		result = append(result, tool.Spec())
	}
	return result
}

type testEmitter struct {
	events   []string
	messages []map[string]any
	next     int
	step     int
	lastID   string
}

func (e *testEmitter) Emit(eventType string, payload map[string]any) {
	e.events = append(e.events, eventType)
	if eventType == "message.created" {
		e.messages = append(e.messages, payload)
	}
}
func (e *testEmitter) NextMessageIndex() int {
	e.next++
	e.lastID = "msg-" + string(rune('0'+e.next))
	return e.next
}
func (e *testEmitter) NextStep() int         { e.step++; return e.step }
func (e *testEmitter) LastMessageID() string { return e.lastID }

func TestMessageRoundTrip(t *testing.T) {
	input := []react.Message{
		{Role: react.RoleSystem, Content: "system"},
		{Role: react.RoleUser, Content: "hello"},
		{Role: react.RoleAssistant, ToolCalls: []react.ToolCall{{ID: "call-1", Name: "lookup", Arguments: json.RawMessage(`{"q":"x"}`)}}},
		{Role: react.RoleTool, ToolCallID: "call-1", Name: "lookup", Content: `{"ok":true}`},
	}
	converted := ToReactMessages(ToSchemaMessages(input))
	if len(converted) != len(input) {
		t.Fatalf("message count = %d, want %d", len(converted), len(input))
	}
	if converted[2].ToolCalls[0].ID != "call-1" || converted[2].ToolCalls[0].Name != "lookup" ||
		string(converted[2].ToolCalls[0].Arguments) != `{"q":"x"}` {
		t.Fatalf("tool call round trip = %#v", converted[2].ToolCalls)
	}
}

func TestToolInfoRoundTrip(t *testing.T) {
	spec := react.ToolSpec{
		Name: "lookup", Description: "lookup data",
		Parameters: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`),
	}
	info, err := SpecToToolInfo(spec)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := ToolInfoToSpec(info)
	if err != nil {
		t.Fatal(err)
	}
	if roundTrip.Name != spec.Name || roundTrip.Description != spec.Description ||
		!strings.Contains(string(roundTrip.Parameters), `"required"`) {
		t.Fatalf("tool info round trip = %#v", roundTrip)
	}
}

func TestToolEmitsMessageAndGuardsLoop(t *testing.T) {
	registry := testRegistry{tools: map[string]react.Tool{
		"lookup": testTool{spec: react.ToolSpec{Name: "lookup", Description: "lookup"}},
	}}
	tools := WrapRegistry(registry, nil)
	if len(tools) != 1 {
		t.Fatalf("wrapped tools = %d", len(tools))
	}
	adapted := tools[0].(*Tool)
	emitter := &testEmitter{}
	ctx := WithEmitter(context.Background(), emitter)
	ctx = WithLoopGuard(ctx, NewLoopGuard(2))
	output, err := adapted.InvokableRun(ctx, `{"q":"x"}`)
	if err != nil || !strings.Contains(output, "ok") {
		t.Fatalf("tool output = %q, %v", output, err)
	}
	if len(emitter.messages) != 1 || emitter.messages[0]["role"] != "tool" {
		t.Fatalf("tool message = %#v", emitter.messages)
	}
	// 第二次相同调用达到阈值，返回可纠正的重复调用观察结果，而不是再次产生副作用。
	repeated, err := adapted.InvokableRun(ctx, `{"q":"x"}`)
	if err != nil || !strings.Contains(repeated, "repeated_tool_call") {
		t.Fatalf("repeated output = %q, %v", repeated, err)
	}
}

func TestToolPropagatesApprovalError(t *testing.T) {
	registry := testRegistry{tools: map[string]react.Tool{
		"write": testTool{spec: react.ToolSpec{Name: "write"}, err: react.ErrApprovalRequired},
	}}
	adapted := WrapRegistry(registry, nil)[0].(*Tool)
	_, err := adapted.InvokableRun(context.Background(), `{}`)
	if !errors.Is(err, react.ErrApprovalRequired) {
		t.Fatalf("approval error = %v", err)
	}
}
