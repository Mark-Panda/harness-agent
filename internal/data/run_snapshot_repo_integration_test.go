//go:build integration

package data

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
)

// TestRunSnapshotRepositoryReads 验证快照仓库组合读取 Agent、策略、候选模型与供应商元数据。
func TestRunSnapshotRepositoryReads(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	users := NewUserRepository(appDB, authDB)
	owner, err := users.CreateUser(ctx, biz.User{
		Username: "snapshot_owner_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := users.CreateUser(ctx, biz.User{
		Username: "snapshot_other_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	providerID := uuid.New()
	defer func() {
		_ = adminDB.Exec("DELETE FROM users WHERE id IN (?, ?)", owner.ID, stranger.ID).Error
		_ = adminDB.Exec("DELETE FROM model_policies WHERE name = ?", "snapshot_policy_"+suffix).Error
		_ = adminDB.Exec("DELETE FROM llm_models WHERE provider_id = ?", providerID).Error
		_ = adminDB.Exec("DELETE FROM llm_providers WHERE id = ?", providerID).Error
	}()

	catalog := NewCatalogRepository(appDB)
	provider, err := catalog.CreateProvider(ctx, owner.ID, biz.Provider{
		ID: providerID, Name: "snapshot_provider_" + suffix, ProviderType: "openai_compatible",
		BaseURL: "https://models.example.com", Status: biz.ProviderEnabled,
	}, biz.ProviderCredential{Encrypted: []byte("ciphertext"), KeyVersion: "v1"}, catalogEvent(owner.ID, "provider.created", "llm_provider", providerID))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	model, err := catalog.CreateModel(ctx, owner.ID, biz.Model{
		ID: uuid.New(), ProviderID: provider.ID, RemoteModelName: "snapshot-generation", Kind: biz.ModelKindChat,
		ContextWindowTokens: biz.MinGenerationContextTokens, MaxOutputTokens: 4096, Status: biz.ModelEnabled,
		VerificationSource: "integration-test", VerifiedAt: &now,
	}, catalogEvent(owner.ID, "model.created", "llm_model", uuid.New()))
	if err != nil {
		t.Fatal(err)
	}
	policy, err := catalog.CreatePolicy(ctx, owner.ID, biz.ModelPolicy{
		ID: uuid.New(), Name: "snapshot_policy_" + suffix, Version: 1, MinContextTokens: biz.MinGenerationContextTokens,
		Status: biz.PolicyPublished, Candidates: []biz.PolicyCandidate{{ModelID: model.ID, Priority: 1}},
	}, catalogEvent(owner.ID, "model_policy.created", "model_policy", uuid.New()))
	if err != nil {
		t.Fatal(err)
	}

	agentService, _ := biz.NewAgentService(NewAgentRepository(appDB), biz.QuotaLimits{})
	agent, err := agentService.Create(ctx, biz.Principal{UserID: owner.ID, Role: biz.RoleUser}, biz.Agent{
		Name: "snapshot-agent", Mode: biz.AgentModeReact, ModelPolicyID: &policy.ID, ModelPolicyVersion: &policy.Version,
	})
	if err != nil {
		t.Fatal(err)
	}

	snapshot := NewRunSnapshotRepository(NewAgentRepository(appDB), catalog)
	gotAgent, err := snapshot.GetAgent(ctx, owner.ID, agent.ID)
	if err != nil || gotAgent.ID != agent.ID || gotAgent.Name != "snapshot-agent" {
		t.Fatalf("GetAgent() = %#v, %v", gotAgent, err)
	}
	// 跨用户读取私有 Agent 必须未命中。
	if _, err := snapshot.GetAgent(ctx, stranger.ID, agent.ID); !errors.Is(err, biz.ErrResourceNotFound) {
		t.Fatalf("cross-user GetAgent error = %v", err)
	}
	gotPolicy, err := snapshot.GetPolicy(ctx, policy.ID)
	if err != nil || gotPolicy.ID != policy.ID || len(gotPolicy.Candidates) != 1 {
		t.Fatalf("GetPolicy() = %#v, %v", gotPolicy, err)
	}
	models, err := snapshot.ListModelsByIDs(ctx, []uuid.UUID{model.ID, uuid.New()})
	if err != nil || len(models) != 1 || models[0].ID != model.ID {
		t.Fatalf("ListModelsByIDs() = %#v, %v", models, err)
	}
	if models, err := snapshot.ListModelsByIDs(ctx, nil); err != nil || len(models) != 0 {
		t.Fatalf("ListModelsByIDs(nil) = %#v, %v", models, err)
	}
	gotProvider, err := snapshot.GetProvider(ctx, provider.ID)
	if err != nil || gotProvider.ID != provider.ID {
		t.Fatalf("GetProvider() = %#v, %v", gotProvider, err)
	}
}
