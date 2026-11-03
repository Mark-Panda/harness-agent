package einoext

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"

	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
)

// scriptedModel 按顺序返回预设 Completion，并记录收到的消息与工具。
type scriptedModel struct {
	completion react.Completion
	messages   []react.Message
	received   []react.ToolSpec
}

// Complete 返回预设结果并记录入参。
func (m *scriptedModel) Complete(_ context.Context, messages []react.Message, tools []react.ToolSpec) (react.Completion, error) {
	m.messages = messages
	m.received = tools
	return m.completion, nil
}

// fakeTool 是可控结果的执行器工具。
type fakeTool struct {
	spec   react.ToolSpec
	output json.RawMessage
	err    error
	calls  int
}

// Spec 返回工具定义。
func (t *fakeTool) Spec() react.ToolSpec { return t.spec }

// Invoke 返回预设结果。
func (t *fakeTool) Invoke(context.Context, json.RawMessage) (json.RawMessage, error) {
	t.calls++
	return t.output, t.err
}

// RiskLevel 返回低风险。
func (t *fakeTool) RiskLevel() string { return "low" }

// TestMessageConversionRoundTrip 验证消息与工具调用在 Eino 与执行器之间往返保真。
func TestMessageConversionRoundTrip(t *testing.T) {
	source := []react.Message{
		{Role: react.RoleSystem, Content: "system"},
		{Role: react.RoleUser, Content: "hello"},
		{Role: react.RoleAssistant, ToolCalls: []react.ToolCall{{ID: "call-1", Name: "search", Arguments: json.RawMessage(`{"q":"x"}`)}}},
		{Role: react.RoleTool, Content: `{"ok":true}`, ToolCallID: "call-1", Name: "search"},
	}
	converted := MessagesToEino(source)
	if converted[0].Role != schema.System || converted[1].Role != schema.User ||
		converted[2].Role != schema.Assistant || converted[3].Role != schema.Tool {
		t.Fatalf("roles were not converted: %#v", converted)
	}
	if len(converted[2].ToolCalls) != 1 || converted[2].ToolCalls[0].Function.Name != "search" ||
		converted[2].ToolCalls[0].Function.Arguments != `{"q":"x"}` {
		t.Fatalf("tool call was not converted: %#v", converted[2])
	}
	roundTrip := MessagesFromEino(converted)
	if len(roundTrip) != len(source) {
		t.Fatalf("round trip length = %d", len(roundTrip))
	}
	for index := range source {
		if roundTrip[index].Role != source[index].Role || roundTrip[index].Content != source[index].Content {
			t.Fatalf("message %d = %#v, want %#v", index, roundTrip[index], source[index])
		}
	}
	if len(roundTrip[2].ToolCalls) != 1 || roundTrip[2].ToolCalls[0].ID != "call-1" {
		t.Fatalf("round trip tool call = %#v", roundTrip[2].ToolCalls)
	}
	// 非法参数 JSON 回退为空对象，避免把损坏数据传给模型。
	invalid := MessagesFromEino([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
		ID: "call-2", Function: schema.FunctionCall{Name: "search", Arguments: "not-json"},
	}}}})
	if string(invalid[0].ToolCalls[0].Arguments) != `{}` {
		t.Fatalf("invalid arguments = %s", invalid[0].ToolCalls[0].Arguments)
	}
	if MessagesFromEino(nil) == nil || len(MessagesFromEino([]*schema.Message{nil})) != 0 {
		t.Fatal("nil messages were not skipped")
	}
}

// TestToolInfoConversion 验证工具参数 Schema 往返并保留 JSON Schema 结构。
func TestToolInfoConversion(t *testing.T) {
	spec := react.ToolSpec{
		Name: "mcp_docs_search", Description: "search docs",
		Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`),
	}
	info, err := ToolInfoFromSpec(spec)
	if err != nil || info.Name != spec.Name || info.ParamsOneOf == nil {
		t.Fatalf("ToolInfoFromSpec() = %#v, %v", info, err)
	}
	roundTrip, err := SpecFromToolInfo(info)
	if err != nil || roundTrip.Name != spec.Name || roundTrip.Description != spec.Description {
		t.Fatalf("SpecFromToolInfo() = %#v, %v", roundTrip, err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(roundTrip.Parameters, &decoded); err != nil || decoded["type"] != "object" {
		t.Fatalf("parameters = %s, %v", roundTrip.Parameters, err)
	}
	if _, err := ToolInfoFromSpec(react.ToolSpec{Name: "bad", Parameters: json.RawMessage(`not-json`)}); err == nil {
		t.Fatal("invalid parameters were accepted")
	}
	if _, err := ToolInfoFromSpec(react.ToolSpec{}); err == nil {
		t.Fatal("empty tool name was accepted")
	}
	if _, err := SpecFromToolInfo(nil); err == nil {
		t.Fatal("nil tool info was accepted")
	}
}

// TestToolAdapter 验证 Eino 工具适配器校验参数并透传结果。
func TestToolAdapter(t *testing.T) {
	inner := &fakeTool{spec: react.ToolSpec{Name: "search", Description: "search"}, output: json.RawMessage(`{"ok":true}`)}
	adapted, err := NewTool(inner)
	if err != nil {
		t.Fatal(err)
	}
	info, err := adapted.Info(context.Background())
	if err != nil || info.Name != "search" {
		t.Fatalf("Info() = %#v, %v", info, err)
	}
	output, err := adapted.InvokableRun(context.Background(), `{"q":"x"}`)
	if err != nil || output != `{"ok":true}` || inner.calls != 1 {
		t.Fatalf("InvokableRun() = %q, %v, calls=%d", output, err, inner.calls)
	}
	if _, err := adapted.InvokableRun(context.Background(), "not-json"); err == nil {
		t.Fatal("invalid arguments were accepted")
	}
	if _, err := adapted.InvokableRun(context.Background(), ""); err != nil || inner.calls != 2 {
		t.Fatalf("empty arguments were rejected: %v", err)
	}
	if _, err := NewTool(nil); err == nil {
		t.Fatal("nil tool was accepted")
	}
}

// TestChatModelAdapter 验证 Generate、Stream 与 WithTools 的适配语义。
func TestChatModelAdapter(t *testing.T) {
	inner := &scriptedModel{completion: react.Completion{
		Content: "answer",
		ToolCalls: []react.ToolCall{{
			ID: "call-1", Name: "search", Arguments: json.RawMessage(`{"q":"x"`),
		}},
	}}
	adapted, err := NewChatModel(inner)
	if err != nil {
		t.Fatal(err)
	}
	// 未绑定工具时执行器收到空工具列表。
	message, err := adapted.Generate(context.Background(), []*schema.Message{
		{Role: schema.System, Content: "system"},
		{Role: schema.User, Content: "hi"},
	})
	if err != nil || message.Content != "answer" || len(message.ToolCalls) != 1 {
		t.Fatalf("Generate() = %#v, %v", message, err)
	}
	if len(inner.messages) != 2 || inner.messages[0].Role != react.RoleSystem {
		t.Fatalf("converted messages = %#v", inner.messages)
	}
	if len(inner.received) != 0 {
		t.Fatalf("unbound tools = %#v", inner.received)
	}

	withTools, err := adapted.WithTools([]*schema.ToolInfo{{
		Name: "search", Desc: "search",
		ParamsOneOf: schema.NewParamsOneOfByJSONSchema(&jsonschema.Schema{Type: "object"}),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := withTools.Generate(context.Background(), []*schema.Message{{Role: schema.User, Content: "hi"}}); err != nil {
		t.Fatal(err)
	}
	if len(inner.received) != 1 || inner.received[0].Name != "search" {
		t.Fatalf("bound tools = %#v", inner.received)
	}

	reader, err := adapted.Stream(context.Background(), []*schema.Message{{Role: schema.User, Content: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	chunks := 0
	for {
		_, recvErr := reader.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			t.Fatal(recvErr)
		}
		chunks++
	}
	if chunks != 1 {
		t.Fatalf("stream chunks = %d", chunks)
	}

	if _, err := NewChatModel(nil); err == nil {
		t.Fatal("nil model was accepted")
	}
	if _, err := adapted.WithTools([]*schema.ToolInfo{nil}); err == nil {
		t.Fatal("nil tool info was accepted")
	}
}
