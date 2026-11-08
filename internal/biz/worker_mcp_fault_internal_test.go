package biz

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/pkg/mcp"
	"github.com/google/uuid"
)

func TestPrepareMCPToolsCachesDiscoveryWithLongestToolTimeout(t *testing.T) {
	var discoveries atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode MCP request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var result any
		switch request.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18"}
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		case "tools/list":
			discoveries.Add(1)
			result = map[string]any{"tools": []map[string]string{{"name": "search"}}}
		default:
			t.Errorf("unexpected MCP method %s", request.Method)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}))
	t.Cleanup(server.Close)
	id := uuid.New()
	worker := &ExecutionWorker{
		mcps: faultMCPRepository{record: MCP{ID: id, Name: "live", Endpoint: server.URL, Status: "enabled", AuthType: "none",
			ToolTimeouts: map[string]int{"search": 10, "ignored": 0}}},
		mcpCache: mcp.NewClientCache(time.Minute),
		config:   WorkerConfig{MCPTimeout: time.Second}, logger: slog.Default(),
		mcpClient: func(endpoint string, headers http.Header, timeout time.Duration, _ bool) (*mcp.Client, error) {
			if endpoint != server.URL || timeout != 10*time.Second {
				t.Errorf("MCP client setup: endpoint=%s timeout=%v", endpoint, timeout)
			}
			return mcp.NewClientWithHeaders(endpoint, headers, timeout, mcp.WithHTTPClient(server.Client()))
		},
	}
	run := mcpRunWithSnapshot(id.String())
	for range 2 {
		tools, err := worker.prepareMCPTools(context.Background(), run, RunFence{})
		if err != nil || len(tools.List()) != 1 {
			t.Fatalf("prepare MCP tools: registry=%v err=%v", tools, err)
		}
	}
	if got := discoveries.Load(); got != 1 {
		t.Fatalf("tools/list called %d times, want one", got)
	}
}

// faultMCPRepository 返回固定的 MCP 元数据或注入读取错误。
type faultMCPRepository struct {
	record MCP
	err    error
}

// GetMCPCredential 实现执行层 MCP 凭证读取端口，测试中不返回任何密文。
func (f faultMCPRepository) GetMCPCredential(context.Context, uuid.UUID) (MCP, ProviderCredential, error) {
	if f.err != nil {
		return MCP{}, ProviderCredential{}, f.err
	}
	return f.record, ProviderCredential{}, nil
}

// mcpRunWithSnapshot 构造仅包含单个低风险 MCP 绑定的 Run。
func mcpRunWithSnapshot(mcpID string) Run {
	return Run{ID: uuid.New(), UserID: uuid.New(), ConfigSnapshot: map[string]any{
		"published_snapshot": map[string]any{"mcps": []any{map[string]any{
			"id": mcpID, "name": "slow", "risk_level": "low", "status": "enabled",
		}}},
	}}
}

// TestPrepareMCPToolsFailsClosedOnSlowOrUnavailableServer 验证 MCP 慢调用与不可用时降级为空工具集。
// 关键约束：MCP 故障必须只影响工具可用性，不得阻塞整个 Run 或暴露异常内容。
func TestPrepareMCPToolsFailsClosedOnSlowOrUnavailableServer(t *testing.T) {
	slow := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	t.Cleanup(slow.Close)
	broken := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(broken.Close)

	tests := []struct {
		name    string
		record  MCP
		factory func(endpoint string) (*mcp.Client, error)
	}{
		{
			name: "slow server times out",
			record: MCP{
				ID: uuid.New(), Name: "slow", Endpoint: slow.URL, AuthType: "none",
				RiskLevel: "low", Status: "enabled",
			},
			factory: func(string) (*mcp.Client, error) {
				return mcp.NewClient(slow.URL, "", "", 20*time.Millisecond, mcp.WithHTTPClient(slow.Client()))
			},
		},
		{
			name: "server returns 5xx",
			record: MCP{
				ID: uuid.New(), Name: "broken", Endpoint: broken.URL, AuthType: "none",
				RiskLevel: "low", Status: "enabled",
			},
			factory: func(string) (*mcp.Client, error) {
				return mcp.NewClient(broken.URL, "", "", time.Second, mcp.WithHTTPClient(broken.Client()))
			},
		},
		{
			name: "client factory fails",
			record: MCP{
				ID: uuid.New(), Name: "invalid", Endpoint: "http://127.0.0.1", AuthType: "none",
				RiskLevel: "low", Status: "enabled",
			},
			factory: func(string) (*mcp.Client, error) { return nil, context.DeadlineExceeded },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			worker := &ExecutionWorker{
				mcps:   faultMCPRepository{record: test.record},
				logger: slog.Default(),
				config: WorkerConfig{MCPTimeout: 20 * time.Millisecond}.normalize(),
				mcpClient: func(endpoint string, headers http.Header, timeout time.Duration, allowPrivate bool) (*mcp.Client, error) {
					return test.factory(endpoint)
				},
			}
			run := mcpRunWithSnapshot(test.record.ID.String())
			registry, err := worker.prepareMCPTools(context.Background(), run, RunFence{})
			if err != nil {
				t.Fatalf("prepare MCP tools: %v", err)
			}
			if len(registry.List()) != 0 {
				t.Fatalf("failed MCP must degrade to empty registry, got %d tools", len(registry.List()))
			}
		})
	}
}

// capturingRunRepository 记录写入的 Run 事件，用于断言降级事件。
type capturingRunRepository struct {
	stubRunRepository
	events []RunEvent
}

func (r *capturingRunRepository) AppendEventFenced(_ context.Context, event RunEvent, _ RunFence) (RunEvent, error) {
	r.events = append(r.events, event)
	return event, nil
}

// TestPrepareMCPToolsEmitsDegradationEvent 验证工具发现失败会写入显式降级事件。
func TestPrepareMCPToolsEmitsDegradationEvent(t *testing.T) {
	runs := &capturingRunRepository{}
	worker := &ExecutionWorker{
		mcps:      faultMCPRepository{err: context.DeadlineExceeded},
		runs:      runs,
		logger:    slog.Default(),
		config:    WorkerConfig{MCPTimeout: time.Second}.normalize(),
		mcpClient: func(string, http.Header, time.Duration, bool) (*mcp.Client, error) { return nil, nil },
	}
	run := mcpRunWithSnapshot(uuid.NewString())
	if _, err := worker.prepareMCPTools(context.Background(), run, RunFence{Owner: "worker", Token: 1}); err != nil {
		t.Fatalf("prepare MCP tools: %v", err)
	}
	if len(runs.events) != 1 || runs.events[0].EventType != "tool.discovery.degraded" {
		t.Fatalf("degradation events = %#v", runs.events)
	}
}

// TestPrepareMCPToolsSkipsCredentialReadFailure 验证 MCP 凭证读取失败时跳过该服务而不是整体失败。
func TestPrepareMCPToolsSkipsCredentialReadFailure(t *testing.T) {
	worker := &ExecutionWorker{
		mcps:      faultMCPRepository{err: context.DeadlineExceeded},
		logger:    slog.Default(),
		config:    WorkerConfig{MCPTimeout: time.Second}.normalize(),
		mcpClient: func(string, http.Header, time.Duration, bool) (*mcp.Client, error) { return nil, nil },
	}
	run := mcpRunWithSnapshot(uuid.NewString())
	registry, err := worker.prepareMCPTools(context.Background(), run, RunFence{})
	if err != nil {
		t.Fatalf("prepare MCP tools: %v", err)
	}
	if tools := registry.List(); len(tools) != 0 {
		t.Fatalf("credential failure must not expose tools: %d", len(tools))
	}
}
