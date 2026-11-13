package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestClientExecuteAuthenticatesAndParses 验证 Sandbox 调用携带认证头并正确解析输出。
func TestClientExecuteAuthenticatesAndParses(t *testing.T) {
	var received Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sandbox-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":{"ok":true}}`))
	}))
	defer server.Close()
	client, err := NewClient(Config{Endpoint: server.URL, Token: "sandbox-token", AllowInsecure: true, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	response, err := client.Execute(context.Background(), Request{
		Tool: "mcp_admin_delete", Arguments: json.RawMessage(`{"id":1}`), RiskLevel: "high", TimeoutMS: 1000,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(string(response.Output), `"ok":true`) {
		t.Fatalf("output = %s", response.Output)
	}
	if received.Tool != "mcp_admin_delete" || received.RiskLevel != "high" || string(received.Arguments) != `{"id":1}` {
		t.Fatalf("received request = %+v", received)
	}
}

func TestClientUsesMCPIdentityWithoutAliasFallback(t *testing.T) {
	target := MCPTarget{
		UserID: uuid.New(), ServerID: uuid.New(), OriginalName: "delete",
		EndpointFingerprint: strings.Repeat("a", 64),
	}
	for _, status := range []int{http.StatusOK, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request Request
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil || r.URL.Path != "/execute/mcp" ||
					request.Tool != "" || request.MCP == nil || *request.MCP != target {
					t.Errorf("unexpected dispatch route %s, request %+v, error %v", r.URL.Path, request, err)
				}
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"output":{}}`))
			}))
			defer server.Close()
			client, err := NewClient(Config{Endpoint: server.URL, Token: "t", AllowInsecure: true})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Execute(context.Background(), Request{MCP: &target, Arguments: json.RawMessage(`{}`), RiskLevel: "high"})
			if status == http.StatusOK && err != nil || status != http.StatusOK && !errors.Is(err, ErrSandboxUnavailable) {
				t.Fatalf("execute with status %d = %v", status, err)
			}
		})
	}
}

// TestClientRejectsInsecureConfigAndBadRequests 验证不安全配置、非法参数与上游错误被拒绝。
func TestClientRejectsInsecureConfigAndBadRequests(t *testing.T) {
	if _, err := NewClient(Config{Endpoint: "http://sandbox:8080", Token: "t"}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("plain http without allow_insecure error = %v", err)
	}
	if _, err := NewClient(Config{Endpoint: "https://sandbox:8080"}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("missing token error = %v", err)
	}
	client, err := NewClient(Config{Endpoint: "https://sandbox.example", Token: "t"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if _, err := client.Execute(context.Background(), Request{Tool: "", Arguments: json.RawMessage(`{}`)}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("empty tool error = %v", err)
	}
	if _, err := client.Execute(context.Background(), Request{Tool: "x", Arguments: json.RawMessage(`not-json`)}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid arguments error = %v", err)
	}
}

// TestClientHandlesUpstreamFailureAndOversize 验证上游 5xx、超大响应与非法输出被分类。
func TestClientHandlesUpstreamFailureAndOversize(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"down"}`))
	}))
	defer server.Close()
	client, err := NewClient(Config{Endpoint: server.URL, Token: "t", AllowInsecure: true})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if _, err := client.Execute(context.Background(), Request{Tool: "x", Arguments: json.RawMessage(`{}`)}); !errors.Is(err, ErrSandboxUnavailable) || !errors.Is(err, ErrToolOutcomeUnknown) {
		t.Fatalf("upstream error = %v", err)
	}

	largeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"output":{"data":"` + strings.Repeat("x", 2048) + `"}}`))
	}))
	defer largeServer.Close()
	small, err := NewClient(Config{Endpoint: largeServer.URL, Token: "t", AllowInsecure: true, MaxResponseBytes: 128})
	if err != nil {
		t.Fatalf("new small client: %v", err)
	}
	if _, err := small.Execute(context.Background(), Request{Tool: "x", Arguments: json.RawMessage(`{}`)}); !errors.Is(err, ErrResponseTooLarge) || !errors.Is(err, ErrToolOutcomeUnknown) {
		t.Fatalf("oversize error = %v", err)
	}

	invalidOutput := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"output":`))
	}))
	defer invalidOutput.Close()
	invalidClient, err := NewClient(Config{Endpoint: invalidOutput.URL, Token: "t", AllowInsecure: true})
	if err != nil {
		t.Fatalf("new invalid client: %v", err)
	}
	if _, err := invalidClient.Execute(context.Background(), Request{Tool: "x", Arguments: json.RawMessage(`{}`)}); !errors.Is(err, ErrSandboxUnavailable) || !errors.Is(err, ErrToolOutcomeUnknown) {
		t.Fatalf("invalid output error = %v", err)
	}
}

// TestClientRespectsContextTimeout 验证调用受上下文超时约束。
func TestClientRespectsContextTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte(`{"output":{}}`))
	}))
	defer server.Close()
	client, err := NewClient(Config{Endpoint: server.URL, Token: "t", AllowInsecure: true, Timeout: time.Second})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := client.Execute(ctx, Request{Tool: "x", Arguments: json.RawMessage(`{}`)}); !errors.Is(err, ErrSandboxUnavailable) {
		t.Fatalf("timeout error = %v", err)
	}
}
