package einoext

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/pkg/agent/model"
	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
)

// TestReactAgentToolLoop 验证 Eino ReAct 能完成“模型调用 -> 工具 -> 最终回答”的闭环，
// 并通过适配层持久化助手与工具消息事件。
func TestReactAgentToolLoop(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		switch calls.Add(1) {
		case 1:
			fmt.Fprint(w, "data: {\"id\":\"req-1\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n")
		default:
			fmt.Fprint(w, "data: {\"id\":\"req-2\",\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	router, err := model.NewRouter([]model.Candidate{{
		Priority: 1,
		Ref: model.ModelRef{
			ProviderID: "provider", ModelID: "model", RemoteModelName: "test-model",
			ContextWindowTokens: 1_000_000, MaxOutputTokens: 4096, Tools: true,
		},
		BaseURL: server.URL, APIKey: "test-key",
	}}, nil, model.RouterConfig{
		MinContextTokens: 1_000_000, MaxAttemptsPerModel: 1, MaxAttemptsPerRun: 4,
		MaxSwitchesPerRun: 1, RequestTimeout: 5 * time.Second,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	registry := testRegistry{tools: map[string]react.Tool{
		"lookup": testTool{spec: react.ToolSpec{Name: "lookup", Description: "lookup"}},
	}}
	emitter := &testEmitter{}
	agent, err := NewReactAgent(context.Background(), ReactConfig{
		Router: router, Registry: registry, Emitter: emitter, MaxSteps: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := agent.Run(context.Background(), []react.Message{
		{Role: react.RoleSystem, Content: "be concise"},
		{Role: react.RoleUser, Content: "lookup"},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Content != "done" || result.Steps != 2 {
		t.Fatalf("result = %#v", result)
	}
	if len(emitter.messages) < 3 {
		t.Fatalf("expected assistant/tool/assistant messages, got %#v", emitter.messages)
	}
	roles := make([]string, 0, len(emitter.messages))
	for _, message := range emitter.messages {
		roles = append(roles, fmt.Sprint(message["role"]))
	}
	if roles[0] != "assistant" || roles[1] != "tool" || roles[len(roles)-1] != "assistant" {
		t.Fatalf("message roles = %v", roles)
	}
	if result.FinalMessageID == "" {
		t.Fatalf("final message id is empty")
	}
	// 确认工具调用消息保留了结构化 tool_calls，供下一轮上下文回放。
	payload, _ := json.Marshal(emitter.messages[0])
	if !containsToolCall(payload) {
		t.Fatalf("assistant tool call was not persisted: %s", payload)
	}
}

func containsToolCall(payload []byte) bool {
	var value map[string]any
	if json.Unmarshal(payload, &value) != nil {
		return false
	}
	calls, ok := value["tool_calls"].([]any)
	return ok && len(calls) > 0
}

// TestReactAgentPropagatesApproval 验证 Eino 图不会吞掉审批门禁错误。
func TestReactAgentPropagatesApproval(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"req-1\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"function\":{\"name\":\"write\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	router, err := model.NewRouter([]model.Candidate{{
		Priority: 1,
		Ref: model.ModelRef{
			ProviderID: "provider", ModelID: "model", RemoteModelName: "test-model",
			ContextWindowTokens: 1_000_000, MaxOutputTokens: 4096, Tools: true,
		},
		BaseURL: server.URL, APIKey: "test-key",
	}}, nil, model.RouterConfig{
		MinContextTokens: 1_000_000, MaxAttemptsPerModel: 1, MaxAttemptsPerRun: 4,
		MaxSwitchesPerRun: 1, RequestTimeout: 5 * time.Second,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	registry := testRegistry{tools: map[string]react.Tool{
		"write": testTool{spec: react.ToolSpec{Name: "write"}, err: react.ErrApprovalRequired},
	}}
	agent, err := NewReactAgent(context.Background(), ReactConfig{
		Router: router, Registry: registry, Emitter: &testEmitter{}, MaxSteps: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, runErr := agent.Run(context.Background(), []react.Message{{Role: react.RoleUser, Content: "write"}})
	if !IsApprovalRequired(runErr) {
		t.Fatalf("approval error was not preserved: %v", runErr)
	}
}
