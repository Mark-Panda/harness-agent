package model

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestChatSuccessAndToolCalls 验证请求格式、鉴权头、工具调用解析与用量映射。
func TestChatSuccessAndToolCalls(t *testing.T) {
	var received struct {
		Model    string        `json:"model"`
		Messages []ChatMessage `json:"messages"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer secret-key" {
			t.Fatalf("missing bearer token: %q", r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"req-123",
			"choices":[{"message":{"content":"done","tool_calls":[{"id":"call-1","function":{"name":"search","arguments":"{\"q\":\"x\"}"}}]},"finish_reason":"tool_calls"}],
			"usage":{"prompt_tokens":10,"completion_tokens":20}
		}`)
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "secret-key", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Chat(context.Background(), ChatRequest{
		Model:           ModelRef{ProviderID: "p", ModelID: "m", RemoteModelName: "generation", ContextWindowTokens: 1_000_000, MaxOutputTokens: 8192, Tools: true},
		Messages:        []ChatMessage{{Role: "user", Content: "hello"}},
		MaxOutputTokens: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if received.Model != "generation" || len(received.Messages) != 1 {
		t.Fatalf("unexpected request payload: %#v", received)
	}
	if response.Content != "done" || response.ProviderRequestID != "req-123" || response.InputTokens != 10 || response.OutputTokens != 20 {
		t.Fatalf("unexpected response: %#v", response)
	}
	if len(response.ToolCalls) != 1 || response.ToolCalls[0].Name != "search" || response.FinishReason != "tool_calls" {
		t.Fatalf("tool calls were not parsed: %#v", response.ToolCalls)
	}
}

// TestChatDisablesThinkingWhenConfigured 验证关闭思考模式的模型会在请求体带上 thinking 参数。
func TestChatDisablesThinkingWhenConfigured(t *testing.T) {
	for _, testCase := range []struct {
		name            string
		disableThinking bool
		wantThinking    bool
	}{
		{name: "disabled", disableThinking: true, wantThinking: true},
		{name: "default", disableThinking: false, wantThinking: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var received map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
					t.Fatal(err)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"req-thinking","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
			}))
			defer server.Close()
			client, err := NewClient(server.URL, "secret-key", 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Chat(context.Background(), ChatRequest{
				Model: ModelRef{
					ProviderID: "p", ModelID: "m", RemoteModelName: "deepseek-flash",
					ContextWindowTokens: 1_000_000, MaxOutputTokens: 1024, DisableThinking: testCase.disableThinking,
				},
				Messages:        []ChatMessage{{Role: "user", Content: "hello"}},
				MaxOutputTokens: 1024,
			})
			if err != nil {
				t.Fatal(err)
			}
			thinking, exists := received["thinking"]
			if exists != testCase.wantThinking {
				t.Fatalf("thinking field exists = %v, want %v (body=%v)", exists, testCase.wantThinking, received)
			}
			if exists {
				payload, ok := thinking.(map[string]any)
				if !ok || payload["type"] != "disabled" {
					t.Fatalf("thinking payload = %#v", thinking)
				}
			}
		})
	}
}

// TestChatContextBudgetOverflow 验证窗口内预算溢出会被拒绝，避免供应商侧 400。
func TestChatContextBudgetOverflow(t *testing.T) {
	client, err := NewClient("https://models.example.com", "secret-key", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// 估算输入加上输出预算超过窗口时必须拒绝。
	longMessages := []ChatMessage{{Role: "user", Content: strings.Repeat("x", 4_000_000)}}
	_, err = client.Chat(context.Background(), ChatRequest{
		Model:    ModelRef{ContextWindowTokens: 1_000_000, MaxOutputTokens: 100},
		Messages: longMessages, MaxOutputTokens: 100,
	})
	if !errors.Is(err, ErrContextExceeded) {
		t.Fatalf("budget overflow error = %v", err)
	}
}

// TestChatEstimatesUsageWhenMissing 验证非流式响应省略 usage 时也会回填预算用量。
func TestChatEstimatesUsageWhenMissing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"req-no-usage","choices":[{"message":{"content":"","tool_calls":[{"id":"call-1","function":{"name":"search","arguments":"{\"q\":\"中文查询\"}"}}]},"finish_reason":"tool_calls"}]}`)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "secret-key", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Chat(context.Background(), ChatRequest{
		Model:    ModelRef{RemoteModelName: "generation", ContextWindowTokens: 1_000_000, MaxOutputTokens: 100, Tools: true},
		Messages: []ChatMessage{{Role: "user", Content: "hello"}}, MaxOutputTokens: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.InputTokens <= 0 || response.OutputTokens <= 0 {
		t.Fatalf("estimated usage = %#v", response)
	}
}

// TestChatStreamParsesSSE 验证流式请求携带 stream 参数，并正确累计文本、工具调用与用量。
func TestChatStreamParsesSSE(t *testing.T) {
	var sawStream bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		sawStream = payload["stream"] == true
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"req-s\",\"choices\":[{\"delta\":{\"content\":\"你\"},\"finish_reason\":\"\"}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"好\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "secret-key", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var deltas []string
	response, err := client.ChatStream(context.Background(), ChatRequest{
		Model: ModelRef{ContextWindowTokens: 1_000_000, MaxOutputTokens: 100, Stream: true},
	}, func(delta string) { deltas = append(deltas, delta) })
	if err != nil {
		t.Fatal(err)
	}
	if !sawStream {
		t.Fatalf("stream flag was not sent")
	}
	if response.Content != "你好" || len(deltas) != 2 || deltas[0] != "你" || deltas[1] != "好" {
		t.Fatalf("stream content = %q deltas = %#v", response.Content, deltas)
	}
	if response.InputTokens != 5 || response.OutputTokens != 2 || response.FinishReason != "stop" {
		t.Fatalf("stream usage = %#v", response)
	}
}

// TestChatStreamEstimatesUsageWhenMissing 验证流式响应省略 usage 时按请求内容回填输入 Token，
// 避免预算与成本统计因供应商不返回用量而恒为零。
func TestChatStreamEstimatesUsageWhenMissing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "secret-key", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.ChatStream(context.Background(), ChatRequest{
		Model:    ModelRef{ContextWindowTokens: 1_000_000, MaxOutputTokens: 100, Stream: true},
		Messages: []ChatMessage{{Role: "system", Content: "you are helpful"}, {Role: "user", Content: "explain vector search"}},
	}, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if response.InputTokens <= 0 {
		t.Fatalf("estimated input tokens = %d", response.InputTokens)
	}
	if response.OutputTokens <= 0 {
		t.Fatalf("estimated output tokens = %d", response.OutputTokens)
	}
}

// TestChatStreamRejectsMalformedEvent 验证损坏的数据帧不会被静默跳过并形成不完整工具调用。
func TestChatStreamRejectsMalformedEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {not-json}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "secret-key", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ChatStream(context.Background(), ChatRequest{
		Model: ModelRef{ContextWindowTokens: 1_000_000, MaxOutputTokens: 100, Stream: true},
	}, func(string) {})
	if !errors.Is(err, ErrRetryable) {
		t.Fatalf("malformed stream error = %v", err)
	}
}

func TestChatStreamRequiresCompletionEvent(t *testing.T) {
	const text = "data: {\"id\":\"request\",\"choices\":[{\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n"
	const usage = "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":21,\"completion_tokens\":7}}\n\n"
	const done = "data: [DONE]\n\n"
	finish := func(reason string) string {
		return "data: {\"choices\":[{\"delta\":{},\"finish_reason\":" + strconv.Quote(reason) + "}]}\n\n"
	}
	for _, tc := range []struct {
		name       string
		body       string
		wantText   string
		wantFinish string
		wantUsage  bool
		wantError  bool
	}{
		{name: "text ends at EOF", body: text + usage, wantText: "partial", wantUsage: true, wantError: true},
		{name: "blank reason", body: text + finish(" "), wantText: "partial", wantError: true},
		{name: "unknown reason", body: text + finish("in_progress"), wantText: "partial", wantError: true},
		{name: "reasoning ends at EOF", body: "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"reasoning\"}}]}\n\n", wantError: true},
		{name: "usage without completion", body: usage, wantUsage: true, wantError: true},
		{name: "empty stream", wantError: true},
		{name: "incomplete done event", body: text + "data: [DONE]\n", wantText: "partial", wantError: true},
		{name: "incomplete finish event", body: text + strings.TrimSuffix(finish("stop"), "\n"), wantText: "partial", wantError: true},
		{name: "malformed event after usage", body: text + usage + "data: {broken\n\n" + done, wantText: "partial", wantUsage: true, wantError: true},
		{name: "done only", body: text + done, wantText: "partial"},
		{name: "null error", body: text + "data: {\"error\":null}\n\n" + done, wantText: "partial"},
		{name: "empty with done", body: done},
		{name: "stop reason only", body: text + finish("stop") + usage, wantText: "partial", wantFinish: "stop", wantUsage: true},
		{name: "multiline stop event", body: text + ": keepalive\r\ndata: {\"choices\":[\r\ndata: {\"finish_reason\":\"stop\"}]}\r\n\r\n", wantText: "partial", wantFinish: "stop"},
		{name: "length reason only", body: text + finish("length"), wantText: "partial", wantFinish: "length"},
		{name: "content filter reason only", body: finish("content_filter"), wantFinish: "content_filter"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(server.Close)
			client, err := NewClient(server.URL, "secret-key", time.Second)
			if err != nil {
				t.Fatal(err)
			}
			var deltas strings.Builder
			response, err := client.ChatStream(context.Background(), ChatRequest{Model: ModelRef{Stream: true}}, func(delta string) {
				deltas.WriteString(delta)
			})
			if tc.wantError {
				if !errors.Is(err, ErrRetryable) {
					t.Fatalf("stream error = %v, want ErrRetryable", err)
				}
			} else if err != nil || response.FinishReason != tc.wantFinish {
				t.Fatalf("finish reason = %q, error = %v", response.FinishReason, err)
			}
			if response.Content != tc.wantText || deltas.String() != tc.wantText {
				t.Fatalf("content = %q, deltas = %q, want %q", response.Content, deltas.String(), tc.wantText)
			}
			if tc.wantUsage && (response.InputTokens != 21 || response.OutputTokens != 7) {
				t.Fatalf("usage = (%d, %d), want (21, 7)", response.InputTokens, response.OutputTokens)
			}
			if tc.wantText != "" && response.ProviderRequestID != "request" {
				t.Fatalf("provider request ID = %q, want request", response.ProviderRequestID)
			}
		})
	}
}

func TestChatEstimatesHiddenReasoningUsage(t *testing.T) {
	const done = "data: [DONE]\n\n"
	reasoning := strings.Repeat("r", 2000)
	reasoningDelta := func(part string) string {
		return `data: {"choices":[{"delta":{"reasoning_content":` + strconv.Quote(part) + `}}]}` + "\n\n"
	}
	stream := reasoningDelta(reasoning[:1001]) + reasoningDelta(reasoning[1001:])
	jsonBody := `{"choices":[{"message":{"reasoning_content":` + strconv.Quote(reasoning) + `},"finish_reason":"stop"}]}`
	withAnswer := `{"choices":[{"message":{"content":"done","reasoning_content":` + strconv.Quote(reasoning) + `},"finish_reason":"stop"}]}`
	for _, tc := range []struct {
		name        string
		contentType string
		body        string
		stream      bool
		wantUsage   int
		wantContent string
		wantError   bool
	}{
		{name: "SSE", contentType: "text/event-stream", body: stream + done, stream: true, wantUsage: 500},
		{name: "interrupted SSE", contentType: "text/event-stream", body: stream, stream: true, wantUsage: 500, wantError: true},
		{name: "SSE with provider usage", contentType: "text/event-stream", body: stream + "data: {\"usage\":{\"completion_tokens\":42}}\n\n" + done, stream: true, wantUsage: 42},
		{name: "non-stream", contentType: "application/json", body: jsonBody, wantUsage: 500},
		{name: "JSON fallback", contentType: "application/json", body: jsonBody, stream: true, wantUsage: 500},
		{name: "JSON fallback with answer", contentType: "application/json", body: withAnswer, stream: true, wantUsage: 508, wantContent: "done"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(server.Close)
			client, err := NewClient(server.URL, "secret-key", time.Second)
			if err != nil {
				t.Fatal(err)
			}
			request := ChatRequest{Model: ModelRef{Stream: true}}
			var response ChatResponse
			var deltas strings.Builder
			if tc.stream {
				response, err = client.ChatStream(context.Background(), request, func(delta string) { deltas.WriteString(delta) })
			} else {
				response, err = client.Chat(context.Background(), request)
			}
			if tc.wantError && !errors.Is(err, ErrRetryable) || !tc.wantError && err != nil {
				t.Fatalf("chat error = %v, want retryable = %t", err, tc.wantError)
			}
			if response.OutputTokens != tc.wantUsage {
				t.Fatalf("output tokens = %d, want %d", response.OutputTokens, tc.wantUsage)
			}
			if response.Content != tc.wantContent || deltas.String() != tc.wantContent {
				t.Fatalf("visible content or deltas did not match %q", tc.wantContent)
			}
		})
	}
}

func TestChatStreamToolCallsRequireTerminationAndCountPayload(t *testing.T) {
	for _, tc := range []struct {
		name        string
		termination string
		wantError   bool
	}{
		{name: "clean EOF", wantError: true},
		{name: "blank finish reason", termination: "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\" \"}]}\n\n", wantError: true},
		{name: "output limit", termination: "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n", wantError: true},
		{name: "filtered", termination: "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"content_filter\"}]}\n\n", wantError: true},
		{name: "finish reason", termination: "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n"},
		{name: "done", termination: "data: [DONE]\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			arguments := `{"query":"` + strings.Repeat("a", 400) + `"}`
			frame := `data: {"id":"tool-request","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-1","function":{"name":"search","arguments":` + strconv.Quote(arguments) + `}}]}}]}`
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, frame+"\n\n"+tc.termination)
			}))
			t.Cleanup(server.Close)
			client, err := NewClient(server.URL, "secret-key", time.Second)
			if err != nil {
				t.Fatal(err)
			}
			response, err := client.ChatStream(context.Background(), ChatRequest{
				Model: ModelRef{Stream: true, Tools: true}, Messages: []ChatMessage{{Role: "user", Content: "search"}},
			}, func(string) {})
			if tc.wantError {
				if !errors.Is(err, ErrRetryable) || len(response.ToolCalls) != 0 {
					t.Fatalf("unterminated tools = %+v, error = %v", response.ToolCalls, err)
				}
			} else if err != nil || len(response.ToolCalls) != 1 || response.ToolCalls[0].Arguments != arguments {
				t.Fatalf("terminated tools = %+v, error = %v", response.ToolCalls, err)
			}
			if response.OutputTokens < 100 {
				t.Fatalf("output token estimate omitted tool arguments: %d", response.OutputTokens)
			}
		})
	}
}

func TestChatStreamProviderErrorDiscardsToolCalls(t *testing.T) {
	const partial = `data: {"id":"tool-request","choices":[{"delta":{"content":"partial","tool_calls":[{"index":0,"id":"call-1","function":{"name":"search","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n"
	const usage = `data: {"choices":[],"usage":{"prompt_tokens":23,"completion_tokens":17}}` + "\n\n"
	for _, tc := range []struct {
		name       string
		body       string
		class      error
		wantInput  int
		wantOutput int
		close      bool
	}{
		{
			name:  "named server error",
			body:  usage + "event: error\n" + `data: {"error":{"code":"server_error","message":"private prompt secret-key"}}` + "\n\n",
			class: ErrRetryable, wantInput: 23, wantOutput: 17,
		},
		{
			name:  "unnamed rate limit with usage",
			body:  `data: {"error":{"code":"rate_limit_exceeded","message":"private prompt secret-key"},"usage":{"prompt_tokens":23,"completion_tokens":17}}` + "\n\n",
			class: ErrRateLimited, wantInput: 23, wantOutput: 17,
		},
		{
			name:  "named context error without envelope",
			body:  "event: error\r\n" + `data: {"code":"context_length_exceeded","message":"private prompt secret-key","choices":[{"delta":{"content":"untrusted"}}]}` + "\r\n\r\n",
			class: ErrContextExceeded, wantInput: 7, wantOutput: 12,
		},
		{
			name:  "policy error takes priority over malformed choices",
			body:  `data: {"error":{"code":"content_policy_violation","message":"private prompt secret-key"},"choices":false}` + "\n\n",
			class: ErrPermanent, wantInput: 7, wantOutput: 12,
		},
		{
			name:  "named error takes priority over completion sentinel",
			body:  "event: error\ndata: [DONE]\n\n",
			class: ErrRetryable, wantInput: 7, wantOutput: 12,
		},
		{
			name:  "stream closes during named error",
			body:  "event: error\ndata: {",
			class: ErrRetryable, wantInput: 7, wantOutput: 12, close: true,
		},
		{
			name:  "unnamed authentication error",
			body:  `data: {"error":{"type":"authentication_error","message":"private prompt secret-key"}}` + "\n\n",
			class: ErrAuthentication, wantInput: 7, wantOutput: 12,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Retry-After", "4")
				_, _ = io.WriteString(w, partial+tc.body)
				if !tc.close {
					_, _ = io.WriteString(w, "data: [DONE]\n\n")
				}
			}))
			t.Cleanup(server.Close)
			client, err := NewClient(server.URL, "secret-key", time.Second)
			if err != nil {
				t.Fatal(err)
			}
			var visible strings.Builder
			result, err := client.ChatStream(context.Background(), ChatRequest{
				Model: ModelRef{Stream: true, Tools: true}, Messages: []ChatMessage{{Role: "user", Content: "search"}},
			}, func(delta string) { visible.WriteString(delta) })
			if !errors.Is(err, tc.class) || len(result.ToolCalls) != 0 {
				t.Fatalf("stream tools = %+v, error = %v, want no tools and %v", result.ToolCalls, err, tc.class)
			}
			if strings.Contains(err.Error(), "secret-key") || strings.Contains(err.Error(), "private prompt") {
				t.Fatalf("provider error leaked response body: %v", err)
			}
			if tc.class == ErrRateLimited {
				var responseErr *ResponseError
				if !errors.As(err, &responseErr) || responseErr.RetryAfter != 4*time.Second {
					t.Fatalf("provider rate limit lost Retry-After: %v", err)
				}
			}
			if result.Content != "partial" || visible.String() != "partial" || result.ProviderRequestID != "tool-request" {
				t.Fatalf("unexpected partial result = %+v, visible = %q", result, visible.String())
			}
			if result.InputTokens != tc.wantInput || result.OutputTokens != tc.wantOutput {
				t.Fatalf("usage = %d/%d, want %d/%d", result.InputTokens, result.OutputTokens, tc.wantInput, tc.wantOutput)
			}
		})
	}
}

func TestChatJSONProviderErrorTakesPriorityOverChoices(t *testing.T) {
	const fields = `{
		"id":"request",
		"error":{"type":"invalid_request_error","code":"content_policy_violation","message":"private prompt secret-key"},
		"usage":{"prompt_tokens":23,"completion_tokens":17}`
	for _, tc := range []struct {
		name    string
		choices string
	}{
		{name: "no choices"},
		{name: "valid choices", choices: `,"choices":[{"message":{"content":"untrusted","tool_calls":[{"id":"call-1","function":{"name":"search","arguments":"{}"}}]},"finish_reason":"tool_calls"}]`},
		{name: "invalid choices", choices: `,"choices":false`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, fields+tc.choices+"}")
			}))
			t.Cleanup(server.Close)
			client, err := NewClient(server.URL, "secret-key", time.Second)
			if err != nil {
				t.Fatal(err)
			}
			var visible strings.Builder
			result, err := client.ChatStream(context.Background(), ChatRequest{
				Model: ModelRef{Stream: true, Tools: true},
			}, func(delta string) { visible.WriteString(delta) })
			if !errors.Is(err, ErrPermanent) || len(result.ToolCalls) != 0 || result.Content != "" || visible.Len() != 0 {
				t.Fatalf("JSON provider error returned result = %+v, visible = %q, error = %v", result, visible.String(), err)
			}
			if strings.Contains(err.Error(), "secret-key") || strings.Contains(err.Error(), "private prompt") {
				t.Fatalf("provider error leaked response body: %v", err)
			}
			if result.InputTokens != 23 || result.OutputTokens != 17 || result.ProviderRequestID != "request" {
				t.Fatalf("JSON provider error lost reported usage or request ID: %+v", result)
			}
		})
	}
}

// TestChatErrorClassification 验证 429、5xx、4xx 的错误分类与密钥脱敏。
func TestChatErrorClassification(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		header     map[string]string
		expectKind error
	}{
		{name: "rate limited", status: http.StatusTooManyRequests, body: `{}`, header: map[string]string{"Retry-After": "30"}, expectKind: ErrRateLimited},
		{name: "server error", status: http.StatusBadGateway, body: `{}`, expectKind: ErrRetryable},
		{name: "bad request", status: http.StatusBadRequest, body: `{"error":"invalid schema secret-key"}`, expectKind: ErrPermanent},
		// 401/403 属于 Endpoint 凭证失效，必须归为可切换错误而不是普通永久失败。
		{name: "unauthorized", status: http.StatusUnauthorized, body: `{"error":"bad key"}`, expectKind: ErrAuthentication},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for key, value := range test.header {
					w.Header().Set(key, value)
				}
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			client, err := NewClient(server.URL, "secret-key", 2*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Chat(context.Background(), ChatRequest{
				Model:    ModelRef{RemoteModelName: "generation", ContextWindowTokens: 1_000_000, MaxOutputTokens: 100},
				Messages: []ChatMessage{{Role: "user", Content: "hi"}}, MaxOutputTokens: 10,
			})
			if !errors.Is(err, test.expectKind) {
				t.Fatalf("error = %v, want kind %v", err, test.expectKind)
			}
			if strings.Contains(err.Error(), "secret-key") {
				t.Fatalf("API key leaked in error: %v", err)
			}
			if test.status == http.StatusTooManyRequests && !strings.Contains(err.Error(), "30") {
				t.Fatalf("Retry-After was not surfaced: %v", err)
			}
		})
	}
}

// TestClientValidationAndRetryAfter 验证构造校验与 Retry-After 解析上限。
func TestClientValidationAndRetryAfter(t *testing.T) {
	if _, err := NewClient("", "key", time.Second); err == nil {
		t.Fatal("empty base URL was accepted")
	}
	if _, err := NewClient("https://models.example.com", "", time.Second); err == nil {
		t.Fatal("empty API key was accepted")
	}
	if duration := RetryAfter("120", time.Second); duration != 120*time.Second {
		t.Fatalf("RetryAfter(120) = %v", duration)
	}
	if duration := RetryAfter("9999", time.Minute); duration != time.Minute {
		t.Fatalf("RetryAfter should be capped, got %v", duration)
	}
	if duration := RetryAfter("invalid", time.Minute); duration != time.Minute {
		t.Fatalf("RetryAfter fallback = %v", duration)
	}
}

// TestModelsListsProviderCatalog 验证从供应商 /models 接口读取模型名，并过滤空标识。
func TestModelsListsProviderCatalog(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/models" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer secret-key" {
			t.Fatalf("missing bearer token")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{
				{"id": "deepseek-chat", "owned_by": "deepseek"},
				{"id": "  ", "owned_by": "empty"},
				{"id": "qwen-max", "owned_by": "alibaba"},
			},
		})
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "secret-key", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	items, err := client.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].ID != "deepseek-chat" || items[1].ID != "qwen-max" {
		t.Fatalf("Models() = %#v", items)
	}
}
