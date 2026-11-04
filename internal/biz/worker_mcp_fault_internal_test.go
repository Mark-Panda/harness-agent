package biz

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/pkg/mcp"
	"github.com/google/uuid"
)

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
			registry := worker.prepareMCPTools(context.Background(), run)
			if len(registry.List()) != 0 {
				t.Fatalf("failed MCP must degrade to empty registry, got %d tools", len(registry.List()))
			}
		})
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
	if tools := worker.prepareMCPTools(context.Background(), run).List(); len(tools) != 0 {
		t.Fatalf("credential failure must not expose tools: %d", len(tools))
	}
}
