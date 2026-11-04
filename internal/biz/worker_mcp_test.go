package biz_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/service/mocks"
	"github.com/Mark-Panda/harness-agent/pkg/agent/model"
	"github.com/Mark-Panda/harness-agent/pkg/mcp"
	"github.com/google/uuid"
	"go.uber.org/mock/gomock"
)

// fakeMCPCredentials 返回固定的低风险 MCP 与空凭证。
type fakeMCPCredentials struct {
	record biz.MCP
}

// GetMCPCredential 实现 MCP 凭证读取端口。
func (f fakeMCPCredentials) GetMCPCredential(_ context.Context, _ uuid.UUID) (biz.MCP, biz.ProviderCredential, error) {
	return f.record, biz.ProviderCredential{}, nil
}

// mcpToolServer 构造最小 MCP TLS 服务，返回一个可调用工具。
func mcpToolServer(t *testing.T, toolResult string) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode MCP request: %v", err)
		}
		if body.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch body.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18"}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{
				"name": "search", "description": "search docs", "inputSchema": map[string]any{"type": "object"},
			}}}
		case "tools/call":
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": toolResult}}, "isError": false}
		default:
			t.Fatalf("unexpected MCP method %s", body.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		encoded, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": body.ID, "result": result})
		_, _ = w.Write(encoded)
	}))
	t.Cleanup(server.Close)
	return server
}

// TestWorkerExecutesLowRiskMCPTool 验证低风险 MCP 工具被模型调用、执行并回填上下文。
func TestWorkerExecutesLowRiskMCPTool(t *testing.T) {
	ctrl := gomock.NewController(t)
	userID, runID, sessionID := uuid.New(), uuid.New(), uuid.New()
	providerID, modelID, mcpID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	var modelCalls int
	var mu sync.Mutex
	var bodies []string
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		encoded, _ := json.Marshal(body)
		mu.Lock()
		modelCalls++
		call := modelCalls
		bodies = append(bodies, string(encoded))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = w.Write([]byte(`{"id":"req-1","choices":[{"message":{"content":"","tool_calls":[{"id":"call-1","function":{"name":"mcp_docs_search","arguments":"{\"query\":\"harness\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"req-2","choices":[{"message":{"content":"工具结果已收到"},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":8}}`))
	}))
	t.Cleanup(modelServer.Close)
	mcpServer := mcpToolServer(t, "mcp-result")

	jobs := mocks.NewMockJobQueue(ctrl)
	runs := mocks.NewMockRunRepository(ctrl)
	sessions := mocks.NewMockSessionRepository(ctrl)
	attempts := mocks.NewMockModelCallRepository(ctrl)
	steps := mocks.NewMockExecutionStepRepository(ctrl)
	stepSeq := int64(0)
	var muSteps sync.Mutex
	var stepKinds []biz.StepKind
	steps.EXPECT().StartStep(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, step biz.ExecutionStep) (biz.ExecutionStep, error) {
			muSteps.Lock()
			stepSeq++
			step.StepSeq = stepSeq
			stepKinds = append(stepKinds, step.Kind)
			muSteps.Unlock()
			return step, nil
		},
	).AnyTimes()
	steps.EXPECT().SavePayload(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, payload biz.ExecutionPayload) (biz.ExecutionPayload, error) {
			return payload, nil
		},
	).AnyTimes()
	steps.EXPECT().FinishStep(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(biz.ExecutionStep{}, nil).AnyTimes()

	worker, err := biz.NewExecutionWorker(biz.ExecutionWorkerDependencies{
		Jobs: jobs, Runs: runs, Sessions: sessions,
		Providers: fakeProviderCredentials{
			provider:   biz.Provider{ID: uuid.MustParse(providerID), Status: biz.ProviderEnabled, BaseURL: modelServer.URL},
			credential: biz.ProviderCredential{KeyVersion: "v1", Encrypted: []byte("ciphertext")},
		},
		Opener: fakeOpener{key: "test-key"}, Attempts: attempts, Steps: steps,
		MCPs: fakeMCPCredentials{record: biz.MCP{
			ID: uuid.MustParse(mcpID), Name: "docs", Endpoint: mcpServer.URL,
			AuthType: "none", RiskLevel: "low", Status: "enabled",
		}},
		MCPClientFactory: func(endpoint string, headers http.Header, timeout time.Duration, allowPrivate bool) (*mcp.Client, error) {
			return mcp.NewClientWithHeaders(endpoint, headers, timeout, mcp.WithHTTPClient(mcpServer.Client()), mcp.WithAllowPrivateEndpoints(allowPrivate))
		},
	}, biz.WorkerConfig{
		WorkerID: "worker-1", Concurrency: 1, PollInterval: time.Millisecond,
		Lease: 60 * time.Second, Heartbeat: 20 * time.Second, CancelPoll: time.Hour,
		Routing: model.RouterConfig{
			MinContextTokens: 1_000_000, MaxAttemptsPerModel: 2, MaxAttemptsPerRun: 6,
			MaxSwitchesPerRun: 2, RequestTimeout: 5 * time.Second,
			BackoffBase: time.Millisecond, MaxBackoff: time.Millisecond, MaxRetryAfter: time.Second,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	job := jobFixture(userID, runID)
	jobs.EXPECT().Claim(gomock.Any(), "worker-1", "agent.run", gomock.Any(), gomock.Any()).Return(job, true, nil)
	snapshot := snapshotFixture(modelServer.URL, providerID, modelID)
	run := runFixture(userID, runID, sessionID, snapshot)
	run.ConfigSnapshot = map[string]any{
		"agent": map[string]any{"name": "工具助手"},
		"published_snapshot": map[string]any{"mcps": []any{map[string]any{
			"id": mcpID, "name": "docs", "endpoint": mcpServer.URL,
			"auth_type": "none", "risk_level": "low", "status": "enabled",
		}}},
	}
	running := run
	running.Status = biz.RunRunning
	fence := biz.RunFence{Owner: "worker-1", Token: 9}
	runs.EXPECT().GetRun(gomock.Any(), userID, runID).Return(run, nil)
	runs.EXPECT().ClaimRun(gomock.Any(), userID, runID, "worker-1", gomock.Any(), gomock.Any()).Return(running, fence, nil)
	sessions.EXPECT().ListMessages(gomock.Any(), userID, sessionID, 50, "").Return(biz.Page[biz.Message]{
		Items: []biz.Message{{Role: biz.MessageUser, Content: "请搜索"}},
	}, nil)
	runs.EXPECT().SetRunAttempt(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), fence).Return(nil).AnyTimes()
	attempts.EXPECT().CreateModelCall(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, call biz.ModelCall) (biz.ModelCall, error) { return call, nil },
	).AnyTimes()
	attempts.EXPECT().FinishModelCall(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(biz.ModelCall{}, nil).AnyTimes()
	var eventTypes []string
	var muEvents sync.Mutex
	runs.EXPECT().AppendEventFenced(gomock.Any(), gomock.Any(), fence).DoAndReturn(
		func(_ context.Context, event biz.RunEvent, _ biz.RunFence) (biz.RunEvent, error) {
			muEvents.Lock()
			eventTypes = append(eventTypes, event.EventType)
			muEvents.Unlock()
			return event, nil
		},
	).AnyTimes()
	var muMessages sync.Mutex
	var persistedMessages []biz.Message
	sessions.EXPECT().AppendMessageIdempotent(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, message biz.Message, _ biz.OutboxEvent) (biz.Message, error) {
			muMessages.Lock()
			persistedMessages = append(persistedMessages, message)
			muMessages.Unlock()
			return message, nil
		},
	).AnyTimes()
	runs.EXPECT().FinishRunWithEvent(gomock.Any(), userID, runID, biz.RunRunning, biz.RunCompleted, gomock.Any(), gomock.Any(), fence, gomock.Any()).Return(
		biz.Run{ID: runID, Status: biz.RunCompleted}, nil,
	)
	jobs.EXPECT().Complete(gomock.Any(), job.ID, "worker-1", job.FencingToken).Return(nil)

	if processed, err := worker.ProcessOne(context.Background()); err != nil || !processed {
		t.Fatalf("ProcessOne() = %t, %v", processed, err)
	}
	muMessages.Lock()
	if len(persistedMessages) != 3 {
		muMessages.Unlock()
		t.Fatalf("persisted messages = %d, want assistant/tool/final", len(persistedMessages))
	}
	if persistedMessages[2].Content != "工具结果已收到" {
		muMessages.Unlock()
		t.Fatalf("unexpected final message: %#v", persistedMessages[2])
	}
	muMessages.Unlock()
	mu.Lock()
	defer mu.Unlock()
	if modelCalls != 2 {
		t.Fatalf("model calls = %d", modelCalls)
	}
	// 第二次模型请求必须包含工具返回，证明工具结果已回填上下文。
	if !strings.Contains(bodies[1], "mcp-result") {
		t.Fatalf("tool result was not fed back to the model: %s", bodies[1])
	}
	muSteps.Lock()
	defer muSteps.Unlock()
	hasTool := false
	for _, kind := range stepKinds {
		if kind == biz.StepKindTool {
			hasTool = true
		}
	}
	if !hasTool {
		t.Fatalf("tool step was not recorded: %v", stepKinds)
	}
	muEvents.Lock()
	defer muEvents.Unlock()
	hasToolCompleted := false
	for _, eventType := range eventTypes {
		if eventType == "tool.completed" {
			hasToolCompleted = true
		}
	}
	if !hasToolCompleted {
		t.Fatalf("tool.completed event missing: %v", eventTypes)
	}
}
