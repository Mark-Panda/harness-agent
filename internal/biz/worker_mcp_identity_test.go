package biz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
	"github.com/Mark-Panda/harness-agent/pkg/mcp"
	"github.com/google/uuid"
)

func TestMCPClientCacheIsolatesRemoteSessionsByUser(t *testing.T) {
	server, _ := statefulMCPServer(t, func() string { return "delete" })
	serverID := uuid.New()
	worker := newObservedWorker(nil)
	worker.mcps = faultMCPRepository{record: MCP{
		ID: serverID, Name: "slow", Endpoint: server.URL, Status: "enabled", RiskLevel: "low", AuthType: "none",
	}}
	worker.mcpCache = mcp.NewClientCache(time.Minute)
	worker.mcpClient = testMCPClientFactory(server)
	alice := mcpRunWithSnapshot(serverID.String())
	aliceNextRun := alice
	aliceNextRun.ID = uuid.New()
	bob := mcpRunWithSnapshot(serverID.String())

	for i, tc := range []struct {
		run  Run
		want string
	}{{alice, "session-1"}, {aliceNextRun, "session-1"}, {bob, "session-2"}} {
		tools, err := worker.prepareMCPTools(context.Background(), tc.run, RunFence{})
		if err != nil {
			t.Fatal(err)
		}
		tool, ok := tools.Lookup("mcp_slow_delete")
		if !ok {
			t.Fatal("expected MCP tool")
		}
		result, err := tool.Invoke(context.Background(), json.RawMessage(`{}`))
		if err != nil || !strings.Contains(string(result), tc.want) {
			t.Fatalf("request %d returned %s, %v, want %s", i, result, err, tc.want)
		}
	}
}

func TestMCPApprovalResumesOnlyTheOriginallySelectedTool(t *testing.T) {
	for _, tc := range []struct {
		name    string
		source  string
		change  string
		execute bool
		sandbox bool
	}{
		{"observed ReAct alias changed", "observed", "collision", true, false},
		{"unobserved ReAct alias changed", "unobserved", "collision", true, false},
		{"Plan tool alias changed", "plan", "collision", true, false},
		{"Plan original server unavailable", "plan", "unavailable", false, false},
		{"ReAct original tool name changed", "observed", "raw-name", false, false},
		{"ReAct endpoint changed", "observed", "endpoint", false, false},
		{"Sandbox routing alias changed", "observed", "collision", true, true},
		{"legacy approval lacks target identity", "legacy", "none", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			var toolRenamed atomic.Bool
			serverA, callsA := statefulMCPServer(t, func() string {
				if toolRenamed.Load() {
					return "DELETE"
				}
				return "delete"
			})
			serverB, callsB := statefulMCPServer(t, func() string { return "delete" })
			idA, idB := uuid.New(), uuid.New()
			// Deliberately omit the endpoint from the snapshot to exercise older Run snapshots.
			snapshot := map[string]any{"published_snapshot": map[string]any{"mcps": []any{
				map[string]any{"id": idB.String(), "name": "OPS", "status": "enabled"},
				map[string]any{"id": idA.String(), "name": "ops", "status": "enabled"},
			}}}
			run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), ConfigSnapshot: snapshot}
			records := testMCPRecords{
				idA: {ID: idA, Endpoint: serverA.URL, Status: "enabled", RiskLevel: "high", AuthType: "none"},
				idB: {ID: idB, Endpoint: serverB.URL, Status: "disabled", RiskLevel: "high", AuthType: "none"},
			}
			approvals := &fakeApprovalRepository{}
			worker := newObservedWorker(nil)
			var plan Plan
			var step PlanStep
			if tc.source == "plan" {
				var plans *memoryPlanRepo
				worker, plans, plan, step = planToolFixture(t, nil, approvals)
				run.ID, run.UserID, run.SessionID, run.Mode = plan.RunID, plan.UserID, plan.SessionID, AgentModePlan
				step.ToolName = "mcp_ops_delete"
				plan.Steps[0] = step
				plans.seedPlan(plan)
			}
			worker.approvals = approvals
			worker.sessions = fakeSessionRepository{}
			worker.runs = stubRunRepository{}
			worker.mcps = records
			worker.mcpCache = nil
			worker.mcpClient = testMCPClientFactory(serverA)
			sandboxRunner := &fakeSandboxRunner{output: json.RawMessage(`{}`)}
			if tc.sandbox {
				worker.config.RequireSandboxForHighRisk = true
				worker.sandbox = sandboxRunner
			}
			fence := RunFence{Owner: "plan-worker", Token: 1}
			initial, err := worker.prepareMCPTools(ctx, run, fence)
			if err != nil {
				t.Fatal(err)
			}
			arguments := json.RawMessage(`{}`)
			switch tc.source {
			case "legacy":
				if _, err := approvals.CreateApproval(ctx, ToolApproval{
					RunID: run.ID, UserID: run.UserID, ToolCallID: "call", ToolName: "mcp_ops_delete", Arguments: arguments,
				}); err != nil {
					t.Fatal(err)
				}
			case "plan":
				err := worker.executePlanToolStep(ctx, run, fence, PlanFence{Owner: fence.Owner, Token: fence.Token}, initial, plan, step)
				if !errors.Is(err, ErrApprovalPending) {
					t.Fatalf("direct tool step error = %v, want pending approval", err)
				}
			default:
				if tc.source == "observed" {
					worker.steps = &fakeStepRepository{}
				}
				tool, ok := worker.runnerRegistry(initial, run).Lookup("mcp_ops_delete")
				if !ok {
					t.Fatal("expected original tool")
				}
				_, err := tool.Invoke(react.WithToolCall(ctx, "call", "mcp_ops_delete"), arguments)
				var request *react.ApprovalRequiredError
				if !errors.As(err, &request) {
					t.Fatalf("tool error = %v, want pending approval", err)
				}
			}
			if len(approvals.approvals) != 1 || callsA.Load() != 0 || callsB.Load() != 0 {
				t.Fatalf("approval request invoked a tool or was not persisted: %+v", approvals.approvals)
			}
			if _, err := approvals.DecideApproval(ctx, run.UserID, approvals.approvals[0].ID, ApprovalDecisionApprove, run.UserID, time.Now()); err != nil {
				t.Fatal(err)
			}

			switch tc.change {
			case "collision":
				records[idB] = MCP{ID: idB, Endpoint: serverB.URL, Status: "enabled", RiskLevel: "high", AuthType: "none"}
			case "unavailable":
				delete(records, idA)
				records[idB] = MCP{ID: idB, Endpoint: serverB.URL, Status: "enabled", RiskLevel: "high", AuthType: "none"}
			case "endpoint":
				records[idA] = MCP{ID: idA, Endpoint: serverB.URL, Status: "enabled", RiskLevel: "high", AuthType: "none"}
			case "raw-name":
				toolRenamed.Store(true)
			}
			resumed, err := worker.prepareMCPTools(ctx, run, fence)
			if err != nil {
				t.Fatal(err)
			}
			if err := worker.resumeApprovals(ctx, run, fence, resumed); err != nil {
				t.Fatal(err)
			}
			stored := approvals.approvals[0]
			if callsB.Load() != 0 {
				t.Fatal("approval executed a different tool")
			}
			if tc.sandbox {
				request := sandboxRunner.request
				if sandboxRunner.calls != 1 || request.Tool != "" || request.MCP == nil || request.MCP.UserID != run.UserID ||
					request.MCP.ServerID != stored.MCPBinding.ServerID || request.MCP.OriginalName != stored.MCPBinding.OriginalName ||
					request.MCP.EndpointFingerprint != stored.MCPBinding.EndpointFingerprint || callsA.Load() != 0 ||
					stored.Status != ApprovalExecuted || stored.DeliveredAt == nil {
					t.Fatalf("sandbox execution did not bind to the approved target: %+v, approval: %+v", request, stored)
				}
				return
			}
			if sandboxRunner.calls != 0 {
				t.Fatalf("approval dispatched an unverified sandbox alias: %q", sandboxRunner.request.Tool)
			}
			if tc.execute {
				if callsA.Load() != 1 || stored.Status != ApprovalExecuted || stored.DeliveredAt == nil {
					t.Fatalf("original tool calls = %d, approval = %+v", callsA.Load(), stored)
				}
			} else if callsA.Load() != 0 || stored.Status != ApprovalFailed || stored.ErrorCode != "tool_unavailable" || stored.DeliveredAt == nil {
				t.Fatalf("unavailable tool calls = %d, approval = %+v", callsA.Load(), stored)
			}
		})
	}
}

type testMCPRecords map[uuid.UUID]MCP

type approvalBoundTestTool struct {
	react.Tool
	target mcp.ToolIdentity
}

func (t approvalBoundTestTool) MCPIdentity() mcp.ToolIdentity { return t.target }

func (records testMCPRecords) GetMCPCredential(_ context.Context, id uuid.UUID) (MCP, ProviderCredential, error) {
	if record, ok := records[id]; ok {
		return record, ProviderCredential{}, nil
	}
	return MCP{}, ProviderCredential{}, ErrResourceNotFound
}

func testMCPClientFactory(server *httptest.Server) func(string, http.Header, time.Duration, bool) (*mcp.Client, error) {
	return func(endpoint string, headers http.Header, timeout time.Duration, _ bool) (*mcp.Client, error) {
		return mcp.NewClientWithHeaders(endpoint, headers, timeout, mcp.WithHTTPClient(server.Client()))
	}
}

func statefulMCPServer(t *testing.T, toolName func() string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls, sessions atomic.Int32
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
			w.Header().Set("Mcp-Session-Id", fmt.Sprintf("session-%d", sessions.Add(1)))
			result = map[string]any{"protocolVersion": "2025-06-18"}
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		case "tools/list":
			result = map[string]any{"tools": []map[string]string{{"name": toolName()}}}
		case "tools/call":
			calls.Add(1)
			result = map[string]any{"content": []map[string]string{{"type": "text", "text": r.Header.Get("Mcp-Session-Id")}}}
		default:
			t.Errorf("unexpected MCP method %s", request.Method)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}))
	t.Cleanup(server.Close)
	return server, &calls
}
