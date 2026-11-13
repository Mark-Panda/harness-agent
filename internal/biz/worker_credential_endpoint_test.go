package biz

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/platform/crypto"
	"github.com/Mark-Panda/harness-agent/pkg/mcp"
	"github.com/google/uuid"
)

type credentialEndpointRepository struct {
	provider   Provider
	mcp        MCP
	credential ProviderCredential
}

func (r credentialEndpointRepository) GetProviderCredential(context.Context, uuid.UUID) (Provider, ProviderCredential, error) {
	return r.provider, r.credential, nil
}

func (r credentialEndpointRepository) GetMCPCredential(context.Context, uuid.UUID) (MCP, ProviderCredential, error) {
	return r.mcp, r.credential, nil
}

func sealEndpointTestCredential(t *testing.T) (*crypto.Keyring, ProviderCredential) {
	t.Helper()
	keyring, err := crypto.NewKeyring(crypto.Key{ID: "v1", Secret: make([]byte, 32)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, version, err := keyring.Seal([]byte("rotated-token"))
	if err != nil {
		t.Fatal(err)
	}
	return keyring, ProviderCredential{Encrypted: encrypted, KeyVersion: version}
}

func TestModelSnapshotUsesCredentialOnlyAtItsCurrentEndpoint(t *testing.T) {
	const currentURL = "https://current.models.example.com/v1"
	providerID := uuid.New()
	keyring, credential := sealEndpointTestCredential(t)
	repository := credentialEndpointRepository{
		provider: Provider{ID: providerID, Status: ProviderEnabled, BaseURL: currentURL}, credential: credential,
	}
	worker := &ExecutionWorker{providers: repository, opener: keyring}
	for _, tc := range []struct {
		name         string
		snapshotURLs []string
		wantIndex    int
	}{
		{name: "changed endpoint is ineligible", snapshotURLs: []string{"https://old.models.example.com/v1"}, wantIndex: -1},
		{name: "skip changed candidate without disabling other candidates", snapshotURLs: []string{"https://old.models.example.com/v1", currentURL}, wantIndex: 1},
		{name: "same endpoint accepts rotated key", snapshotURLs: []string{currentURL}},
		{name: "legacy snapshot uses current endpoint", snapshotURLs: []string{""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			snapshotCandidates := make([]snapshotCandidate, 0, len(tc.snapshotURLs))
			for i, endpoint := range tc.snapshotURLs {
				snapshotCandidates = append(snapshotCandidates, snapshotCandidate{
					Priority: i + 1, ProviderID: providerID.String(), ModelID: uuid.NewString(),
					ProviderBaseURL: endpoint, RemoteModelName: "chat", ContextWindowTokens: MinGenerationContextTokens, MaxOutputTokens: 4096,
				})
			}
			run := Run{ModelPolicySnapshot: map[string]any{"candidates": snapshotCandidates}}
			t.Run("run routing", func(t *testing.T) {
				candidates, _, err := worker.buildCandidates(ctx, run)
				if tc.wantIndex < 0 {
					if len(candidates) != 0 || !errors.Is(err, ErrInvalidRunSnapshot) {
						t.Fatalf("stale endpoint produced %d run candidates, error = %v", len(candidates), err)
					}
					return
				}
				if err != nil || len(candidates) != 1 {
					t.Fatalf("run candidates = %d, error = %v", len(candidates), err)
				}
				candidate := candidates[0]
				if candidate.Ref.ModelID != snapshotCandidates[tc.wantIndex].ModelID || candidate.BaseURL != currentURL || candidate.APIKey != "rotated-token" {
					t.Fatalf("unexpected run model or endpoint: %s %s", candidate.Ref.ModelID, candidate.BaseURL)
				}
			})
			t.Run("memory routing", func(t *testing.T) {
				resolved, err := resolveSessionModel(ctx, latestRunRepository{items: []Run{run}}, repository, keyring, uuid.New(), uuid.New())
				if tc.wantIndex < 0 {
					if !errors.Is(err, ErrSummaryModelUnavailable) {
						t.Fatalf("stale endpoint was eligible for a memory task: error = %v", err)
					}
					return
				}
				if err != nil || resolved.modelID != snapshotCandidates[tc.wantIndex].ModelID || resolved.baseURL != currentURL || resolved.apiKey != "rotated-token" {
					t.Fatalf("unexpected memory model or endpoint: %s %s, error = %v", resolved.modelID, resolved.baseURL, err)
				}
			})
		})
	}
}

func TestMCPSnapshotUsesCredentialOnlyAtItsCurrentEndpoint(t *testing.T) {
	keyring, credential := sealEndpointTestCredential(t)
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/current" || r.Header.Get("Authorization") != "Bearer rotated-token" {
			t.Errorf("MCP discovery sent a request to %q with the wrong destination or credential", r.URL.Path)
		}
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
			result = map[string]string{"protocolVersion": "2025-06-18"}
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		case "tools/list":
			result = map[string]any{"tools": []map[string]string{{"name": "search"}}}
		default:
			t.Errorf("unexpected MCP method %q", request.Method)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}))
	t.Cleanup(server.Close)
	currentURL := server.URL + "/current"
	for _, tc := range []struct {
		name     string
		endpoint string
		wantTool bool
	}{
		{name: "changed endpoint is skipped", endpoint: server.URL + "/old"},
		{name: "same endpoint accepts rotated key", endpoint: currentURL, wantTool: true},
		{name: "legacy snapshot uses current endpoint", wantTool: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := uuid.New()
			runs := &capturingRunRepository{}
			worker := &ExecutionWorker{
				mcps: credentialEndpointRepository{
					mcp: MCP{ID: id, Name: "docs", Endpoint: currentURL, Status: "enabled", RiskLevel: "low", AuthType: "bearer"}, credential: credential,
				},
				opener: keyring, runs: runs, logger: slog.Default(), config: WorkerConfig{MCPTimeout: time.Second},
				mcpClient: func(endpoint string, headers http.Header, timeout time.Duration, _ bool) (*mcp.Client, error) {
					return mcp.NewClientWithHeaders(endpoint, headers, timeout, mcp.WithHTTPClient(server.Client()))
				},
			}
			run := Run{ID: uuid.New(), UserID: uuid.New(), ConfigSnapshot: map[string]any{
				"published_snapshot": map[string]any{"mcps": []any{map[string]any{
					"id": id.String(), "name": "docs", "endpoint": tc.endpoint, "status": "enabled", "risk_level": "low",
				}}},
			}}
			before := requests.Load()
			registry, err := worker.prepareMCPTools(context.Background(), run, RunFence{})
			if err != nil {
				t.Fatalf("prepare MCP tools: %v", err)
			}
			if tc.wantTool {
				if len(registry.List()) != 1 || requests.Load() == before {
					t.Fatalf("expected discovery at the current endpoint, got %d tools", len(registry.List()))
				}
				return
			}
			if len(registry.List()) != 0 || requests.Load() != before {
				t.Fatalf("stale MCP endpoint caused %d requests and exposed %d tools", requests.Load()-before, len(registry.List()))
			}
			if len(runs.events) != 1 || runs.events[0].EventType != "tool.discovery.degraded" {
				t.Fatalf("expected an MCP degradation event, got %+v", runs.events)
			}
		})
	}
}
