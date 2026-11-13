package einoext

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/pkg/agent/model"
	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
)

// newGuardTestAgent 构造指向固定响应服务器的 ReAct Agent。
func newGuardTestAgent(t *testing.T, handler http.HandlerFunc, registry react.Registry) *ReactAgent {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	router, err := model.NewRouter([]model.Candidate{{
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
	agent, err := NewReactAgent(context.Background(), ReactConfig{
		Router: router, Registry: registry, Emitter: &testEmitter{}, MaxSteps: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	return agent
}

// TestReactAgentRejectsEmptyResponse 验证空正文且无工具调用时不会被当作成功。
func TestReactAgentRejectsEmptyResponse(t *testing.T) {
	agent := newGuardTestAgent(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":""},"finish_reason":"stop"}]}`)
	}, testRegistry{})
	_, err := agent.Run(context.Background(), []react.Message{{Role: react.RoleUser, Content: "test"}})
	if !errors.Is(err, react.ErrEmptyResponse) {
		t.Fatalf("empty response error = %v", err)
	}
}

// TestReactAgentRejectsReasoningOnlyResponse 验证只返回推理内容时不会被当作成功。
func TestReactAgentRejectsReasoningOnlyResponse(t *testing.T) {
	agent := newGuardTestAgent(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"","reasoning_content":"hidden"},"finish_reason":"stop"}]}`)
	}, testRegistry{})
	_, err := agent.Run(context.Background(), []react.Message{{Role: react.RoleUser, Content: "test"}})
	if !errors.Is(err, react.ErrReasoningOnly) {
		t.Fatalf("reasoning-only response error = %v", err)
	}
}

// TestCompletionResponseErrorClassification 覆盖异常结束原因与空回答的分类。
func TestCompletionResponseErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		name       string
		completion react.Completion
		want       error
	}{
		{name: "answer", completion: react.Completion{Content: "ok"}},
		{name: "tool call without content", completion: react.Completion{ToolCalls: []react.ToolCall{{ID: "call-1"}}}},
		{name: "empty", completion: react.Completion{}, want: react.ErrEmptyResponse},
		{name: "reasoning only", completion: react.Completion{ReasoningContent: "thinking"}, want: react.ErrReasoningOnly},
		{name: "content filtered", completion: react.Completion{FinishReason: "content_filter"}, want: react.ErrContentFiltered},
		{
			name:       "truncated tool call",
			completion: react.Completion{FinishReason: "length", ToolCalls: []react.ToolCall{{ID: "call-1"}}},
			want:       react.ErrIncompleteToolCall,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := completionResponseError(tc.completion)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestTruncateToolOutputKeepsHeadAndTail 验证超限结果保留首尾且总长不超上限。
func TestTruncateToolOutputKeepsHeadAndTail(t *testing.T) {
	head := strings.Repeat("H", 40*1024)
	middle := strings.Repeat("M", 40*1024)
	tail := strings.Repeat("T", 40*1024)
	output := head + middle + tail
	truncated := truncateToolOutput(output)
	if len(truncated) > maximumToolOutputBytes {
		t.Fatalf("truncated length = %d, exceeds %d", len(truncated), maximumToolOutputBytes)
	}
	if !strings.HasPrefix(truncated, strings.Repeat("H", 1024)) {
		t.Fatal("head content was not preserved")
	}
	if !strings.HasSuffix(truncated, strings.Repeat("T", 1024)) {
		t.Fatal("tail content was not preserved")
	}
	if !strings.Contains(truncated, "tool result truncated") || !strings.Contains(truncated, "original") {
		t.Fatalf("truncation note missing: %s", truncated[:200])
	}
	if got := truncateUTF8FromEnd("中文尾部", 4); got == "" || !strings.HasSuffix("中文尾部", got) {
		t.Fatalf("utf8 tail = %q", got)
	}
}

// TestLoopGuardResetsPerRound 验证重复阈值只作用于当前模型轮次。
func TestLoopGuardResetsPerRound(t *testing.T) {
	guard := NewLoopGuard(2)
	if content, err := guard.checkToolCall(context.Background(), "lookup", `{"q":"x"}`); err != nil || content != "" {
		t.Fatalf("first call = %q, %v", content, err)
	}
	if content, err := guard.checkToolCall(context.Background(), "lookup", `{"q":"x"}`); err != nil || content == "" {
		t.Fatalf("repeated call should be corrected: %q, %v", content, err)
	}
	// 新一轮模型调用允许再次使用相同工具，合法轮询不得被跨轮累计拦截。
	guard.ResetRound()
	if content, err := guard.checkToolCall(context.Background(), "lookup", `{"q":"x"}`); err != nil || content != "" {
		t.Fatalf("call after reset = %q, %v", content, err)
	}
}

// barrierTool 在调用时等待同一批次的其他工具，用于验证并行执行。
type barrierTool struct {
	name    string
	barrier *sync.WaitGroup
	calls   atomic.Int64
}

func (t *barrierTool) Spec() react.ToolSpec {
	return react.ToolSpec{Name: t.name, Description: t.name, Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (t *barrierTool) RiskLevel() string { return "low" }
func (t *barrierTool) Invoke(context.Context, json.RawMessage) (json.RawMessage, error) {
	t.calls.Add(1)
	t.barrier.Done()
	// 两个同批次工具都进入 Invoke 才能通过；串行执行会在这里超时。
	done := make(chan struct{})
	go func() {
		t.barrier.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		return nil, errors.New("tool calls were executed sequentially")
	}
	return json.RawMessage(`{"ok":"` + t.name + `"}`), nil
}

type barrierRegistry struct{ tools map[string]*barrierTool }

func (r barrierRegistry) Lookup(name string) (react.Tool, bool) {
	tool, ok := r.tools[name]
	return tool, ok
}
func (r barrierRegistry) List() []react.ToolSpec {
	specs := make([]react.ToolSpec, 0, len(r.tools))
	for _, tool := range r.tools {
		specs = append(specs, tool.Spec())
	}
	return specs
}

// TestReactAgentRunsToolCallsInParallel 验证同一轮多个独立工具调用并行执行。
func TestReactAgentRunsToolCallsInParallel(t *testing.T) {
	barrier := &sync.WaitGroup{}
	barrier.Add(2)
	first := &barrierTool{name: "first", barrier: barrier}
	second := &barrierTool{name: "second", barrier: barrier}
	registry := barrierRegistry{tools: map[string]*barrierTool{"first": first, "second": second}}
	var calls atomic.Int64
	agent := newGuardTestAgent(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			fmt.Fprint(w, `{"choices":[{"message":{"content":"","tool_calls":[`+
				`{"id":"call-1","function":{"name":"first","arguments":"{}"}},`+
				`{"id":"call-2","function":{"name":"second","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"done"},"finish_reason":"stop"}]}`)
	}, registry)
	result, err := agent.Run(context.Background(), []react.Message{{Role: react.RoleUser, Content: "run both"}})
	if err != nil || result.Content != "done" {
		t.Fatalf("parallel result = %#v, %v", result, err)
	}
	if first.calls.Load() != 1 || second.calls.Load() != 1 {
		t.Fatalf("parallel calls = %d/%d", first.calls.Load(), second.calls.Load())
	}
}
