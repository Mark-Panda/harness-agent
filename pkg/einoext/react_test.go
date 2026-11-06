package einoext

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestReactAgentStepLimitFinalSummary 验证步数耗尽后会执行一次不带工具的收尾调用，
// 基于已有工具观测返回结论，而不是直接让整轮失败。
func TestReactAgentStepLimitFinalSummary(t *testing.T) {
	var calls atomic.Int32
	var summaryBody atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		call := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if call <= 2 {
			fmt.Fprint(w, "data: {\"id\":\"req-tool\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n")
		} else {
			summaryBody.Store(string(raw))
			fmt.Fprint(w, "data: {\"id\":\"req-final\",\"choices\":[{\"delta\":{\"content\":\"最终结论\"},\"finish_reason\":\"stop\"}]}\n\n")
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
		MinContextTokens: 1_000_000, MaxAttemptsPerModel: 1, MaxAttemptsPerRun: 8,
		MaxSwitchesPerRun: 1, RequestTimeout: 5 * time.Second,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	registry := testRegistry{tools: map[string]react.Tool{
		"lookup": testTool{spec: react.ToolSpec{Name: "lookup", Description: "lookup"}},
	}}
	agent, err := NewReactAgent(context.Background(), ReactConfig{
		Router: router, Registry: registry, Emitter: &testEmitter{}, MaxSteps: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := agent.Run(context.Background(), []react.Message{{Role: react.RoleUser, Content: "lookup"}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Content != "最终结论" {
		t.Fatalf("final summary = %q", result.Content)
	}
	if result.Steps != 3 {
		t.Fatalf("steps = %d, want 3", result.Steps)
	}
	body, _ := summaryBody.Load().(string)
	if strings.Contains(body, `"tools"`) {
		t.Fatalf("step-limit summary must not send tools: %s", body)
	}
	if !strings.Contains(body, "已达到工具调用步数上限") {
		t.Fatalf("step-limit instruction missing from summary request: %s", body)
	}
}

// TestReactAgentNonStreamProviderToolCall 验证供应商忽略流式请求、直接返回 JSON 时，
// Eino 仍能识别工具调用并完成闭环。
func TestReactAgentNonStreamProviderToolCall(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch calls.Add(1) {
		case 1:
			fmt.Fprint(w, `{"id":"req-1","choices":[{"message":{"content":"","tool_calls":[{"id":"call-1","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
		default:
			fmt.Fprint(w, `{"id":"req-2","choices":[{"message":{"content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":8}}`)
		}
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
	agent, err := NewReactAgent(context.Background(), ReactConfig{
		Router: router, Registry: registry, Emitter: &testEmitter{}, MaxSteps: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := agent.Run(context.Background(), []react.Message{{Role: react.RoleUser, Content: "lookup"}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Content != "done" || calls.Load() != 2 {
		t.Fatalf("result = %#v, calls = %d", result, calls.Load())
	}
}

// countingTool 记录真实执行次数，用于验证预算超限后不再产生工具副作用。
type countingTool struct {
	calls *atomic.Int32
}

func (t countingTool) Spec() react.ToolSpec {
	return react.ToolSpec{Name: "lookup", Description: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}
}

func (t countingTool) RiskLevel() string { return "low" }

func (t countingTool) Invoke(context.Context, json.RawMessage) (json.RawMessage, error) {
	t.calls.Add(1)
	return json.RawMessage(`{"ok":true}`), nil
}

// TestReactAgentEnforcesTokenBudget 验证 Token 预算超限时在工具执行前终止，
// 并返回统一的预算错误供 Worker 映射失败原因。
func TestReactAgentEnforcesTokenBudget(t *testing.T) {
	var serverCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		serverCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"req-1\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":50}}\n\n")
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
	toolCalls := &atomic.Int32{}
	registry := testRegistry{tools: map[string]react.Tool{
		"lookup": countingTool{calls: toolCalls},
	}}
	agent, err := NewReactAgent(context.Background(), ReactConfig{
		Router: router, Registry: registry, Emitter: &testEmitter{},
		MaxSteps: 4, MaxTotalTokens: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, runErr := agent.Run(context.Background(), []react.Message{{Role: react.RoleUser, Content: "lookup"}})
	if !errors.Is(runErr, react.ErrBudgetExceeded) {
		t.Fatalf("budget error = %v", runErr)
	}
	if toolCalls.Load() != 0 {
		t.Fatalf("tool executed after budget exceeded: %d", toolCalls.Load())
	}
	if serverCalls.Load() != 1 {
		t.Fatalf("model calls = %d, want 1", serverCalls.Load())
	}
}

// TestReactAgentPersistsUnknownToolObservation 验证未知工具也会落库为 tool 消息，
// 避免会话历史留下没有结果的 tool_calls 导致后续请求被供应商拒绝。
func TestReactAgentPersistsUnknownToolObservation(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		switch calls.Add(1) {
		case 1:
			fmt.Fprint(w, "data: {\"id\":\"req-1\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-missing\",\"function\":{\"name\":\"missing\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n")
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
	emitter := &testEmitter{}
	agent, err := NewReactAgent(context.Background(), ReactConfig{
		Router: router, Registry: testRegistry{tools: map[string]react.Tool{}}, Emitter: emitter, MaxSteps: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := agent.Run(context.Background(), []react.Message{{Role: react.RoleUser, Content: "call missing"}})
	if err != nil || result.Content != "done" {
		t.Fatalf("result = %#v, %v", result, err)
	}
	found := false
	for _, message := range emitter.messages {
		if message["role"] != "tool" {
			continue
		}
		if content, _ := message["content"].(string); strings.Contains(content, "unknown_tool") {
			found = true
			if message["tool_call_id"] != "call-missing" {
				t.Fatalf("unknown tool message lost call id: %#v", message)
			}
		}
	}
	if !found {
		t.Fatalf("unknown tool observation was not persisted: %#v", emitter.messages)
	}
}

// TestReactAgentContinuesTruncatedAnswer 验证输出被长度截断后会续写一次，
// 并把两段文本合并为同一条最终回答。
func TestReactAgentContinuesTruncatedAnswer(t *testing.T) {
	var calls atomic.Int32
	var continuationBody atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		switch calls.Add(1) {
		case 1:
			fmt.Fprint(w, "data: {\"id\":\"req-1\",\"choices\":[{\"delta\":{\"content\":\"前半\"},\"finish_reason\":\"length\"}]}\n\n")
		default:
			continuationBody.Store(string(raw))
			fmt.Fprint(w, "data: {\"id\":\"req-2\",\"choices\":[{\"delta\":{\"content\":\"后半\"},\"finish_reason\":\"stop\"}]}\n\n")
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
	agent, err := NewReactAgent(context.Background(), ReactConfig{
		Router: router, Registry: testRegistry{tools: map[string]react.Tool{}}, Emitter: &testEmitter{}, MaxSteps: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := agent.Run(context.Background(), []react.Message{{Role: react.RoleUser, Content: "写一段长文"}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Content != "前半后半" {
		t.Fatalf("continued content = %q", result.Content)
	}
	if result.Steps != 2 {
		t.Fatalf("steps = %d, want 2", result.Steps)
	}
	body, _ := continuationBody.Load().(string)
	if !strings.Contains(body, "从中断处继续") {
		t.Fatalf("continuation instruction missing: %s", body)
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
