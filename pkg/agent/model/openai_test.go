package model

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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

// TestChatRejectsContextAndStreaming 验证 1M+ 硬准入与流式请求不被静默降级。
func TestChatRejectsContextAndStreaming(t *testing.T) {
	client, err := NewClient("https://models.example.com", "secret-key", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Chat(context.Background(), ChatRequest{Model: ModelRef{ContextWindowTokens: 128_000}}); !errors.Is(err, ErrContextExceeded) {
		t.Fatalf("low context error = %v", err)
	}
	if _, err := client.Chat(context.Background(), ChatRequest{Model: ModelRef{ContextWindowTokens: 1_000_000, Stream: true}}); !errors.Is(err, ErrCapability) {
		t.Fatalf("streaming error = %v", err)
	}
	// 估算输入加上输出预算超过窗口时必须拒绝，避免供应商侧 400。
	longMessages := []ChatMessage{{Role: "user", Content: strings.Repeat("x", 4_000_000)}}
	_, err = client.Chat(context.Background(), ChatRequest{
		Model:    ModelRef{ContextWindowTokens: 1_000_000, MaxOutputTokens: 100},
		Messages: longMessages, MaxOutputTokens: 100,
	})
	if !errors.Is(err, ErrContextExceeded) {
		t.Fatalf("budget overflow error = %v", err)
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
