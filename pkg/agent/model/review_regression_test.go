package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
)

// TestEstimateToolTokens 覆盖工具定义 Token 估算。
func TestEstimateToolTokens(t *testing.T) {
	if tokens := EstimateToolTokens(nil); tokens != 0 {
		t.Fatalf("empty tools = %d", tokens)
	}
	tokens := EstimateToolTokens([]react.ToolSpec{{
		Name: "lookup", Description: "lookup data",
		Parameters: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
	}})
	if tokens <= 0 {
		t.Fatalf("tool tokens = %d", tokens)
	}
}

// TestRouterMinContextWindow 覆盖候选最小窗口计算。
func TestRouterMinContextWindow(t *testing.T) {
	router, err := NewRouter([]Candidate{
		{Ref: ModelRef{ProviderID: "p", ModelID: "a", ContextWindowTokens: 100}, BaseURL: "https://example.com", APIKey: "k"},
		{Ref: ModelRef{ProviderID: "p", ModelID: "b", ContextWindowTokens: 50}, BaseURL: "https://example.com", APIKey: "k"},
	}, nil, RouterConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if window := router.MinContextWindow(); window != 50 {
		t.Fatalf("minimum window = %d", window)
	}
	var nilRouter *Router
	if window := nilRouter.MinContextWindow(); window != 0 {
		t.Fatalf("nil router window = %d", window)
	}
}

// TestClassifyError 覆盖失败分类到稳定字符串的映射。
func TestClassifyError(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{nil, ""},
		{context.Canceled, "cancelled"},
		{context.DeadlineExceeded, "timeout"},
		{ErrContextExceeded, "context_exceeded"},
		{ErrRateLimited, "rate_limited"},
		{ErrAuthentication, "authentication_failed"},
		{ErrCapability, "capability_mismatch"},
		{ErrRetryable, "retryable_failure"},
		{ErrPermanent, "request_rejected"},
		{ErrNoCandidates, "routing_exhausted"},
		{ErrAttemptLimit, "routing_exhausted"},
		{ErrSwitchLimit, "routing_exhausted"},
		{errors.New("other"), "unknown"},
	} {
		if got := ClassifyError(tc.err); got != tc.want {
			t.Fatalf("ClassifyError(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// TestSanitizedMessageTruncates 覆盖错误文本截断与 UTF-8 安全。
func TestSanitizedMessageTruncates(t *testing.T) {
	if text := sanitizedMessage(nil); text != "" {
		t.Fatalf("nil message = %q", text)
	}
	text := sanitizedMessage(errors.New(strings.Repeat("中", 300)))
	if len(text) > 512 || !strings.HasPrefix(text, "中") {
		t.Fatalf("sanitized message length = %d", len(text))
	}
}

// TestSleepWithContext 覆盖取消上下文与零时长退避。
func TestSleepWithContext(t *testing.T) {
	if err := sleepWithContext(context.Background(), 0); err != nil {
		t.Fatalf("zero sleep = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepWithContext(ctx, time.Second); err == nil {
		t.Fatal("cancelled sleep must fail")
	}
}

// TestChatCapturesReasoningContent 覆盖非流式推理内容解析。
func TestChatCapturesReasoningContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"","reasoning_content":"hidden"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "key", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Chat(context.Background(), ChatRequest{
		Model:           ModelRef{ProviderID: "p", ModelID: "m", RemoteModelName: "m", ContextWindowTokens: 1_000_000},
		Messages:        []ChatMessage{{Role: "user", Content: "hi"}},
		MaxOutputTokens: 128,
	})
	if err != nil || response.ReasoningContent != "hidden" || response.Content != "" {
		t.Fatalf("reasoning response = %#v, %v", response, err)
	}
}

// TestChatStreamCapturesReasoningContent 覆盖流式推理内容解析。
func TestChatStreamCapturesReasoningContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"hidden\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "key", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var deltas []string
	response, err := client.ChatStream(context.Background(), ChatRequest{
		Model:           ModelRef{ProviderID: "p", ModelID: "m", RemoteModelName: "m", ContextWindowTokens: 1_000_000, Stream: true},
		Messages:        []ChatMessage{{Role: "user", Content: "hi"}},
		MaxOutputTokens: 128,
	}, func(delta string) { deltas = append(deltas, delta) })
	if err != nil || response.Content != "answer" || response.ReasoningContent != "hidden" {
		t.Fatalf("stream response = %#v, %v", response, err)
	}
	if len(deltas) != 1 || deltas[0] != "answer" {
		t.Fatalf("deltas = %#v", deltas)
	}
}

// TestRouterCompleteStream 覆盖 Router 流式入口与增量回调。
func TestRouterCompleteStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"streamed\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	router, err := NewRouter([]Candidate{{
		Ref: ModelRef{
			ProviderID: "p", ModelID: "m", RemoteModelName: "m",
			ContextWindowTokens: 1_000_000, MaxOutputTokens: 1024, Stream: true, Tools: true,
		},
		BaseURL: server.URL, APIKey: "key",
	}}, nil, RouterConfig{
		MinContextTokens: 1_000_000, MaxAttemptsPerModel: 1, MaxAttemptsPerRun: 2,
		MaxSwitchesPerRun: 1, RequestTimeout: 5 * time.Second,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var deltas []string
	completion, err := router.CompleteStream(context.Background(),
		[]react.Message{{Role: react.RoleUser, Content: "hi"}}, nil,
		func(delta string) { deltas = append(deltas, delta) })
	if err != nil || completion.Content != "streamed" || len(deltas) != 1 {
		t.Fatalf("stream completion = %#v, %v, deltas=%#v", completion, err, deltas)
	}
}

// TestToToolDefinitionsAndToolCalls 覆盖工具定义与调用转换。
func TestToToolDefinitionsAndToolCalls(t *testing.T) {
	definitions := toToolDefinitions([]react.ToolSpec{
		{Name: "lookup", Description: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)},
	})
	if len(definitions) != 1 || definitions[0].Name != "lookup" || !strings.Contains(string(definitions[0].Parameters), `"type"`) {
		t.Fatalf("tool definitions = %#v", definitions)
	}
	calls := toReactToolCalls([]ToolCall{{ID: "call-1", Name: "lookup", Arguments: `{"q":"x"}`}})
	if len(calls) != 1 || calls[0].Name != "lookup" || string(calls[0].Arguments) != `{"q":"x"}` {
		t.Fatalf("react tool calls = %#v", calls)
	}
	if calls := toReactToolCalls(nil); len(calls) != 0 {
		t.Fatalf("empty tool calls = %#v", calls)
	}
}
