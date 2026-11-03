package model

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
)

// TestReActModelConvertsToolsAndMessages 验证工具定义、助手 tool_calls 与返回值转换。
func TestReActModelConvertsToolsAndMessages(t *testing.T) {
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"req-react",
			"choices":[{"message":{"content":"","tool_calls":[{"id":"call-2","function":{"name":"search","arguments":"{\"q\":\"next\"}"}}]},"finish_reason":"tool_calls"}],
			"usage":{"prompt_tokens":30,"completion_tokens":7}
		}`)
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "secret-key", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := NewReActModel(client, ModelRef{
		RemoteModelName: "generation", ContextWindowTokens: 1_000_000, MaxOutputTokens: 4096, Tools: true,
	}, 0.2)
	if err != nil {
		t.Fatal(err)
	}
	messages := []react.Message{
		{Role: react.RoleSystem, Content: "system"},
		{Role: react.RoleUser, Content: "hello"},
		{Role: react.RoleAssistant, ToolCalls: []react.ToolCall{{ID: "call-1", Name: "search", Arguments: json.RawMessage(`{"q":"first"}`)}}},
		{Role: react.RoleTool, ToolCallID: "call-1", Name: "search", Content: `{"result":"ok"}`},
	}
	completion, err := adapter.Complete(context.Background(), messages, []react.ToolSpec{
		{Name: "search", Description: "search docs", Parameters: json.RawMessage(`{"type":"object"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if completion.Usage.InputTokens != 30 || completion.Usage.OutputTokens != 7 {
		t.Fatalf("usage was not mapped: %#v", completion.Usage)
	}
	if len(completion.ToolCalls) != 1 || completion.ToolCalls[0].ID != "call-2" || string(completion.ToolCalls[0].Arguments) != `{"q":"next"}` {
		t.Fatalf("tool calls were not converted: %#v", completion.ToolCalls)
	}

	// 协议请求必须携带 tools 定义和助手消息的 tool_calls 回放。
	tools, ok := received["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools were not sent: %#v", received["tools"])
	}
	chatMessages, _ := received["messages"].([]any)
	if len(chatMessages) != 4 {
		t.Fatalf("unexpected message count: %#v", chatMessages)
	}
	assistant, _ := chatMessages[2].(map[string]any)
	if _, exists := assistant["tool_calls"]; !exists {
		t.Fatalf("assistant tool_calls were not replayed: %#v", assistant)
	}
}

// TestNewReActModelValidation 验证适配器构造的 1M+ 与依赖校验。
func TestNewReActModelValidation(t *testing.T) {
	client, _ := NewClient("https://models.example.com", "key", time.Second)
	if _, err := NewReActModel(nil, ModelRef{}, 0); err == nil {
		t.Fatal("nil client was accepted")
	}
	if _, err := NewReActModel(client, ModelRef{RemoteModelName: "small", ContextWindowTokens: 128_000}, 0); err == nil {
		t.Fatal("sub-1M model was accepted")
	}
	if _, err := NewReActModel(client, ModelRef{ContextWindowTokens: 1_000_000}, 0); err == nil {
		t.Fatal("empty model name was accepted")
	}
}
