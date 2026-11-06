package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// mockRPCServer 构造最小 MCP Streamable HTTP 服务，按 method 返回结果。
func mockRPCServer(t *testing.T, handler func(method string, params json.RawMessage, request *http.Request, response http.ResponseWriter) (any, bool)) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		result, respond := handler(body.Method, body.Params, r, w)
		if !respond {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		encoded, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": body.ID, "result": result})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(encoded)
	}))
	t.Cleanup(server.Close)
	return server
}

// newTestClient 构造信任测试证书的客户端。
func newTestClient(t *testing.T, server *httptest.Server, authType, credential string) *Client {
	t.Helper()
	header, value := AuthHeader(authType, credential)
	client, err := NewClient(server.URL, header, value, 5*time.Second, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// TestClientInitializeListAndCall 验证握手、分页工具发现、鉴权头与工具调用。
func TestClientInitializeListAndCall(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	server := mockRPCServer(t, func(method string, params json.RawMessage, request *http.Request, response http.ResponseWriter) (any, bool) {
		mu.Lock()
		methods = append(methods, method)
		mu.Unlock()
		switch method {
		case "initialize":
			if request.Header.Get("Authorization") != "Bearer secret" {
				t.Errorf("missing auth header: %v", request.Header)
			}
			response.Header().Set("Mcp-Session-Id", "session-1")
			return map[string]any{"protocolVersion": "2025-06-18"}, true
		case "notifications/initialized":
			return nil, false
		case "tools/list":
			var paging struct {
				Cursor string `json:"cursor"`
			}
			_ = json.Unmarshal(params, &paging)
			if paging.Cursor == "" {
				return map[string]any{"tools": []map[string]any{{"name": "Search.Docs", "description": "search", "inputSchema": map[string]any{"type": "object"}}}, "nextCursor": "page-2"}, true
			}
			if request.Header.Get("Mcp-Session-Id") != "session-1" {
				t.Errorf("session id was not propagated")
			}
			return map[string]any{"tools": []map[string]any{{"name": "fetch", "description": "fetch"}}}, true
		case "tools/call":
			return map[string]any{"content": []map[string]any{{"type": "text", "text": "result"}}, "isError": false}, true
		}
		t.Fatalf("unexpected method %s", method)
		return nil, false
	})
	client := newTestClient(t, server, "bearer", "secret")
	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 || tools[0].Name != "Search.Docs" || tools[1].Name != "fetch" {
		t.Fatalf("unexpected tools: %#v", tools)
	}
	result, err := client.CallTool(context.Background(), "fetch", json.RawMessage(`{"url":"https://example.com"}`))
	if err != nil || len(result.Content) != 1 || result.Content[0].Text != "result" {
		t.Fatalf("CallTool() = %#v, %v", result, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(methods) != 5 {
		t.Fatalf("unexpected method sequence: %v", methods)
	}
}

// TestClientParsesSSEAndRPCError 验证 SSE 响应解析与 JSON-RPC 错误分类。
func TestClientParsesSSEAndRPCError(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch body.Method {
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18"}}`))
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%s,\"error\":{\"code\":-32601,\"message\":\"tool missing\"}}\n\n", body.ID)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
		}
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server, "none", "")
	_, err := client.CallTool(context.Background(), "missing", nil)
	if !errors.Is(err, ErrRPC) || !strings.Contains(err.Error(), "tool missing") {
		t.Fatalf("CallTool() error = %v", err)
	}
}

// TestClientSendsMultipleAuthHeaders 验证握手请求携带完整的多请求头凭证。
func TestClientSendsMultipleAuthHeaders(t *testing.T) {
	server := mockRPCServer(t, func(method string, _ json.RawMessage, request *http.Request, response http.ResponseWriter) (any, bool) {
		switch method {
		case "initialize":
			if request.Header.Get("Authorization") != "Bearer token" ||
				request.Header.Get("X-Archery-Username") != "maxianfei" ||
				request.Header.Get("X-Archery-Password") != "secret" {
				t.Errorf("missing multi auth headers: %v", request.Header)
			}
			response.Header().Set("Mcp-Session-Id", "session-multi")
			return map[string]any{"protocolVersion": "2025-06-18"}, true
		case "notifications/initialized":
			return nil, false
		default:
			return map[string]any{}, true
		}
	})
	headers := AuthHeaders("header", `{"Authorization":"Bearer token","X-Archery-Username":"maxianfei","X-Archery-Password":"secret"}`)
	client, err := NewClientWithHeaders(server.URL, headers, 5*time.Second, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
}

// TestAllowedMCPIPPolicy 验证内网开关只放开 RFC1918，回环与元数据地址始终拒绝。
func TestAllowedMCPIPPolicy(t *testing.T) {
	cases := []struct {
		ip           string
		allowPrivate bool
		allowed      bool
	}{
		{"8.8.8.8", false, true},
		{"10.8.8.69", false, false},
		{"10.8.8.69", true, true},
		{"127.0.0.1", true, false},
		{"169.254.169.254", true, false},
		{"::1", true, false},
	}
	for _, test := range cases {
		if got := allowedMCPIP(net.ParseIP(test.ip), test.allowPrivate); got != test.allowed {
			t.Fatalf("allowedMCPIP(%s, %t) = %t, want %t", test.ip, test.allowPrivate, got, test.allowed)
		}
	}
}

// TestAuthHeadersSupportsJSONBundle 验证多请求头凭证与非法头名/换行注入过滤。
func TestAuthHeadersSupportsJSONBundle(t *testing.T) {
	headers := AuthHeaders("header", `{"Authorization":"Bearer token","X-Archery-Username":"maxianfei","X-Archery-Password":"secret","Bad Header":"x","Injected":"ok\r\nX-Evil: 1"}`)
	if got := headers.Get("Authorization"); got != "Bearer token" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := headers.Get("X-Archery-Username"); got != "maxianfei" {
		t.Fatalf("X-Archery-Username = %q", got)
	}
	if got := headers.Get("X-Archery-Password"); got != "secret" {
		t.Fatalf("X-Archery-Password = %q", got)
	}
	if headers.Get("Bad Header") != "" || headers.Get("Injected") != "" {
		t.Fatalf("invalid headers were accepted: %#v", headers)
	}
	// 单请求头字符串格式保持兼容。
	single := AuthHeaders("header", "X-Key: secret")
	if single.Get("X-Key") != "secret" {
		t.Fatalf("single header = %#v", single)
	}
	// bearer 与 oauth 保持原有行为。
	if AuthHeaders("bearer", "abc").Get("Authorization") != "Bearer abc" {
		t.Fatalf("bearer header = %#v", AuthHeaders("bearer", "abc"))
	}
}

// TestAuthHeaderVariants 验证不同鉴权类型生成正确的请求头。
func TestAuthHeaderVariants(t *testing.T) {
	tests := []struct {
		authType   string
		credential string
		name       string
		value      string
	}{
		{authType: "none", credential: "ignored", name: "", value: ""},
		{authType: "bearer", credential: "", name: "", value: ""},
		{authType: "bearer", credential: "token", name: "Authorization", value: "Bearer token"},
		{authType: "oauth", credential: "token", name: "Authorization", value: "Bearer token"},
		{authType: "header", credential: "X-Key: secret", name: "X-Key", value: "secret"},
		{authType: "header", credential: "opaque", name: "X-MCP-Key", value: "opaque"},
	}
	for _, test := range tests {
		name, value := AuthHeader(test.authType, test.credential)
		if name != test.name || value != test.value {
			t.Fatalf("AuthHeader(%s) = %q %q, want %q %q", test.authType, name, value, test.name, test.value)
		}
	}
}

// TestRetryAfterAndTruncate 验证 Retry-After 解析与错误信息截断。
func TestRetryAfterAndTruncate(t *testing.T) {
	if duration := ParseRetryAfter("5", time.Second); duration != 5*time.Second {
		t.Fatalf("Retry-After = %v", duration)
	}
	if duration := ParseRetryAfter("invalid", time.Second); duration != time.Second {
		t.Fatalf("fallback Retry-After = %v", duration)
	}
	if duration := ParseRetryAfter("9999", time.Second); duration != time.Second {
		t.Fatalf("oversized Retry-After = %v", duration)
	}
	long := strings.Repeat("x", 300)
	if truncated := truncate(long, 10); len(truncated) != 10 {
		t.Fatalf("truncate = %q", truncated)
	}
}

// TestSafeDialRejectsPrivateAddress 验证拨号阶段拒绝私网与元数据地址。
func TestSafeDialRejectsPrivateAddress(t *testing.T) {
	dial := safeDialContext(time.Second)
	for _, address := range []string{"127.0.0.1:443", "169.254.169.254:443", "10.0.0.1:443"} {
		if _, err := dial(context.Background(), "tcp", address); !errors.Is(err, ErrInvalidEndpoint) {
			t.Fatalf("dial %s error = %v", address, err)
		}
	}
	if _, err := dial(context.Background(), "tcp", "invalid"); !errors.Is(err, ErrInvalidEndpoint) {
		t.Fatalf("invalid address error = %v", err)
	}
}

// TestClientNotifyAndEmptySSE 验证通知接口与空 SSE 响应的错误处理。
func TestClientNotifyAndEmptySSE(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch body.Method {
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18"}}`))
		case "notifications/initialized":
			w.WriteHeader(http.StatusNoContent)
		case "tools/call":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(": comment\n\n"))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
		}
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server, "none", "")
	if err := client.notify(context.Background(), "notifications/initialized", map[string]any{}); err != nil {
		t.Fatalf("notify() = %v", err)
	}
	if _, err := client.CallTool(context.Background(), "tool", nil); !errors.Is(err, ErrTransport) {
		t.Fatalf("empty SSE error = %v", err)
	}
}

// TestRegistryBusinessErrorAndSpec 验证工具业务错误仍返回载荷，且 Spec/RiskLevel 可读取。
func TestRegistryBusinessErrorAndSpec(t *testing.T) {
	server := mockRPCServer(t, func(method string, _ json.RawMessage, _ *http.Request, _ http.ResponseWriter) (any, bool) {
		switch method {
		case "initialize":
			return map[string]any{"protocolVersion": "2025-06-18"}, true
		case "notifications/initialized":
			return nil, false
		case "tools/list":
			return map[string]any{"tools": []map[string]any{{"name": "run", "description": "run tool"}}}, true
		case "tools/call":
			return map[string]any{"content": []map[string]any{{"type": "text", "text": "failed"}}, "isError": true}, true
		}
		return nil, false
	})
	registry := NewRegistry([]Server{{Name: "ops", RiskLevel: "low", Client: newTestClient(t, server, "none", "")}})
	if err := registry.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	tool, _ := registry.Lookup("mcp_ops_run")
	bound, ok := tool.(binding)
	if !ok {
		t.Fatalf("registry tool type = %T", tool)
	}
	if bound.Spec().Name != "mcp_ops_run" || bound.RiskLevel() != "low" {
		t.Fatalf("unexpected binding: %#v", bound.Spec())
	}
	output, err := tool.Invoke(context.Background(), nil)
	if !errors.Is(err, ErrToolBusiness) {
		t.Fatalf("business error = %v", err)
	}
	if !strings.Contains(string(output), `"is_error":true`) {
		t.Fatalf("business error payload = %s", output)
	}
}

// TestCanonicalNameASCIIOnly 验证中文 MCP/工具名不会生成非法函数名。
func TestCanonicalNameASCIIOnly(t *testing.T) {
	name := canonicalToolName("客户系统", "发送消息")
	for _, r := range name {
		if r > 127 || !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			t.Fatalf("canonical name contains invalid character %q: %q", r, name)
		}
	}
	if name == "" || name == "mcp__" {
		t.Fatalf("canonical name is empty: %q", name)
	}
}

// TestCanonicalNameTruncation 验证超长工具名被稳定截断并保持唯一后缀。
func TestCanonicalNameTruncation(t *testing.T) {
	longServer := strings.Repeat("server", 20)
	longTool := strings.Repeat("tool", 20)
	first := canonicalToolName(longServer, longTool)
	second := canonicalToolName(longServer, longTool+"x")
	if len(first) > 60 || first == second {
		t.Fatalf("canonical names = %q, %q", first, second)
	}
	if truncateName("", 5) != "tool" {
		t.Fatal("empty truncate fallback failed")
	}
}

// TestClientRejectsUnsafeEndpoint 验证非 HTTPS 与带用户信息的端点被拒绝。
func TestClientRejectsUnsafeEndpoint(t *testing.T) {
	if _, err := NewClient("http://example.com/mcp", "", "", time.Second); !errors.Is(err, ErrInvalidEndpoint) {
		t.Fatalf("http endpoint error = %v", err)
	}
	if _, err := NewClient("https://user:pass@example.com/mcp", "", "", time.Second); !errors.Is(err, ErrInvalidEndpoint) {
		t.Fatalf("userinfo endpoint error = %v", err)
	}
	if _, err := NewClient("https://example.com/mcp#fragment", "", "", time.Second); !errors.Is(err, ErrInvalidEndpoint) {
		t.Fatalf("fragment endpoint error = %v", err)
	}
}

// TestCallToolRetriesAfterSessionExpired 验证缓存客户端会话被服务端回收后自动重新握手并重试一次。
func TestCallToolRetriesAfterSessionExpired(t *testing.T) {
	var initializes int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch body.Method {
		case "initialize":
			count := atomic.AddInt32(&initializes, 1)
			w.Header().Set("Mcp-Session-Id", fmt.Sprintf("session-%d", count))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18"}}`))
		case "notifications/initialized":
			w.WriteHeader(http.StatusNoContent)
		case "tools/call":
			// 第一个会话已被服务端回收，返回 404；重新握手后的会话正常返回结果。
			if r.Header.Get("Mcp-Session-Id") == "session-1" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"ok"}]}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server, "none", "")
	if _, err := client.CallTool(context.Background(), "tool", nil); err != nil {
		t.Fatalf("call tool after session expiry = %v", err)
	}
	if got := atomic.LoadInt32(&initializes); got != 2 {
		t.Fatalf("initialize calls = %d, want 2", got)
	}
}

// TestRegistryKeepsRiskLevelsAndExposesTools 验证不同风险等级工具均可见，风险等级由上层审批门禁判断。
func TestRegistryKeepsRiskLevelsAndExposesTools(t *testing.T) {
	server := mockRPCServer(t, func(method string, _ json.RawMessage, _ *http.Request, _ http.ResponseWriter) (any, bool) {
		switch method {
		case "initialize":
			return map[string]any{"protocolVersion": "2025-06-18"}, true
		case "notifications/initialized":
			return nil, false
		case "tools/list":
			return map[string]any{"tools": []map[string]any{
				{"name": "Search.Docs", "description": "search"},
				{"name": "Delete.All", "description": "danger"},
			}}, true
		case "tools/call":
			return map[string]any{"content": []map[string]any{{"type": "text", "text": "ok"}}, "isError": false}, true
		}
		return nil, false
	})
	low := newTestClient(t, server, "none", "")
	high := newTestClient(t, server, "none", "")
	registry := NewRegistry([]Server{
		{Name: "Docs", RiskLevel: "low", Client: low},
		{Name: "Admin", RiskLevel: "high", Client: high},
	})
	if err := registry.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	specs := registry.List()
	if len(specs) != 4 {
		t.Fatalf("registered tools = %#v", specs)
	}
	if specs[0].Name != "mcp_docs_search_docs" {
		t.Fatalf("unexpected canonical name: %s", specs[0].Name)
	}
	tool, exists := registry.Lookup("mcp_docs_search_docs")
	if !exists {
		t.Fatal("canonical tool was not registered")
	}
	if tool.RiskLevel() != "low" {
		t.Fatalf("risk level = %s", tool.RiskLevel())
	}
	output, err := tool.Invoke(context.Background(), json.RawMessage(`{"query":"x"}`))
	if err != nil || !strings.Contains(string(output), `"is_error":false`) || !strings.Contains(string(output), `"text":"ok"`) {
		t.Fatalf("Invoke() = %s, %v", output, err)
	}
	highRisk, exists := registry.Lookup("mcp_admin_delete_all")
	if !exists {
		t.Fatal("high risk tool must remain visible for approval")
	}
	if highRisk.RiskLevel() != "high" {
		t.Fatalf("high risk level = %s", highRisk.RiskLevel())
	}
}

// TestRegistryPropagatesIdempotencyKey 验证审批幂等键通过 _meta 传入 MCP 工具调用。
func TestRegistryPropagatesIdempotencyKey(t *testing.T) {
	var mutex sync.Mutex
	var callParams map[string]any
	server := mockRPCServer(t, func(method string, params json.RawMessage, _ *http.Request, _ http.ResponseWriter) (any, bool) {
		switch method {
		case "initialize":
			return map[string]any{"protocolVersion": "2025-06-18"}, true
		case "notifications/initialized":
			return nil, false
		case "tools/list":
			return map[string]any{"tools": []map[string]any{{"name": "Execute", "description": "run"}}}, true
		case "tools/call":
			var decoded map[string]any
			_ = json.Unmarshal(params, &decoded)
			mutex.Lock()
			callParams = decoded
			mutex.Unlock()
			return map[string]any{"content": []map[string]any{{"type": "text", "text": "ok"}}, "isError": false}, true
		}
		return nil, false
	})
	client := newTestClient(t, server, "none", "")
	registry := NewRegistry([]Server{{Name: "ops", RiskLevel: "high", Client: client}})
	if err := registry.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	tool, exists := registry.Lookup("mcp_ops_execute")
	if !exists {
		t.Fatal("high risk tool must be available for approval execution")
	}
	ctx := WithIdempotencyKey(context.Background(), "idem-1")
	if key := IdempotencyKeyFromContext(ctx); key != "idem-1" {
		t.Fatalf("idempotency key = %q", key)
	}
	if _, err := tool.Invoke(ctx, json.RawMessage(`{"id":"x"}`)); err != nil {
		t.Fatal(err)
	}
	mutex.Lock()
	defer mutex.Unlock()
	meta, _ := callParams["_meta"].(map[string]any)
	if meta == nil || meta["idempotency_key"] != "idem-1" {
		t.Fatalf("idempotency key was not propagated: %#v", callParams)
	}
	if key := IdempotencyKeyFromContext(context.Background()); key != "" {
		t.Fatalf("unexpected key without injection: %q", key)
	}
}

// TestRegistryRejectsInvalidServers 验证空客户端与非法风险等级不会进入注册表。
func TestRegistryRejectsInvalidServers(t *testing.T) {
	server := mockRPCServer(t, func(method string, _ json.RawMessage, _ *http.Request, _ http.ResponseWriter) (any, bool) {
		switch method {
		case "initialize":
			return map[string]any{"protocolVersion": "2025-06-18"}, true
		case "notifications/initialized":
			return nil, false
		case "tools/list":
			return map[string]any{"tools": []map[string]any{{"name": "run", "description": "run"}}}, true
		}
		return nil, false
	})
	client := newTestClient(t, server, "none", "")
	registry := NewRegistry([]Server{
		{Name: "missing", RiskLevel: "low"},
		{Name: "invalid", RiskLevel: "extreme", Client: client},
		{Name: "default", RiskLevel: "", Client: client},
	})
	if err := registry.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, exists := registry.Lookup("mcp_missing_run"); exists {
		t.Fatal("server without client was registered")
	}
	if _, exists := registry.Lookup("mcp_invalid_run"); exists {
		t.Fatal("invalid risk level was accepted")
	}
	tool, exists := registry.Lookup("mcp_default_run")
	if !exists || tool.RiskLevel() != "low" {
		t.Fatalf("default risk tool = %#v, %t", tool, exists)
	}
}

// TestPublicIPValidation 验证回环、私网与元数据地址被拒绝，公网地址放行。
func TestPublicIPValidation(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "10.1.2.3", "192.168.1.1", "172.16.0.1", "169.254.169.254", "::1", "fc00::1"} {
		ip := net.ParseIP(raw)
		if isPublicIP(ip) {
			t.Fatalf("%s must not be treated as public", raw)
		}
	}
	if !isPublicIP(net.ParseIP("8.8.8.8")) {
		t.Fatal("public address was rejected")
	}
}

// TestRegistryEmpty 验证没有可用工具时返回明确错误。
func TestRegistryEmpty(t *testing.T) {
	registry := NewRegistry(nil)
	if err := registry.Prepare(context.Background()); !errors.Is(err, ErrEmptyRegistry) {
		t.Fatalf("empty registry error = %v", err)
	}
}
