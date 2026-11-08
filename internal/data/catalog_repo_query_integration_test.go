//go:build integration

package data

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
)

// TestCatalogRepositoryQueryPaths 验证凭证读取、目录列举、模型更新与策略版本号。
func TestCatalogRepositoryQueryPaths(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	actor, err := NewUserRepository(appDB, authDB).CreateUser(ctx, biz.User{
		Username: "catalog_query_" + suffix, PasswordHash: "hash", Role: biz.RoleAdmin,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	providerID, policyID := uuid.New(), uuid.New()
	defer func() {
		_ = adminDB.Exec("DELETE FROM model_policies WHERE name = ?", "query_policy_"+suffix).Error
		_ = adminDB.Exec("DELETE FROM llm_models WHERE provider_id = ?", providerID).Error
		_ = adminDB.Exec("DELETE FROM llm_providers WHERE id = ?", providerID).Error
		_ = adminDB.Exec("DELETE FROM users WHERE id = ?", actor.ID).Error
	}()

	catalog := NewCatalogRepository(appDB)
	provider, err := catalog.CreateProvider(ctx, actor.ID, biz.Provider{
		ID: providerID, Name: "query_provider_" + suffix, ProviderType: "openai_compatible",
		BaseURL: "https://models.example.com", Status: biz.ProviderEnabled,
	}, biz.ProviderCredential{Encrypted: []byte("ciphertext-v2"), KeyVersion: "v2"}, catalogEvent(actor.ID, "provider.created", "llm_provider", providerID))
	if err != nil {
		t.Fatal(err)
	}

	// GetProviderCredential 返回密文与密钥版本，元数据不含密文。
	gotProvider, credential, err := catalog.GetProviderCredential(ctx, provider.ID)
	if err != nil || gotProvider.ID != provider.ID {
		t.Fatalf("GetProviderCredential() = %#v, %v", gotProvider, err)
	}
	if string(credential.Encrypted) != "ciphertext-v2" || credential.KeyVersion != "v2" {
		t.Fatalf("credential = %#v", credential)
	}

	// ListProviders 按名称排序且包含新建供应商。
	providers, err := catalog.ListProviders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range providers {
		if item.ID == provider.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("ListProviders missing %s", provider.ID)
	}

	now := time.Now().UTC()
	modelID := uuid.New()
	model, err := catalog.CreateModel(ctx, actor.ID, biz.Model{
		ID: modelID, ProviderID: provider.ID, RemoteModelName: "query-generation", Kind: biz.ModelKindChat,
		ContextWindowTokens: biz.MinGenerationContextTokens, MaxOutputTokens: 4096, Status: biz.ModelEnabled,
		VerificationSource: "integration-test", VerifiedAt: &now,
	}, catalogEvent(actor.ID, "model.created", "llm_model", modelID))
	if err != nil {
		t.Fatal(err)
	}

	// UpdateModel 修改输出预算与状态，ListModels 按供应商过滤可见。
	model.MaxOutputTokens = 8192
	model.Capabilities = biz.ModelCapabilities{Streaming: true}
	updated, err := catalog.UpdateModel(ctx, actor.ID, model, catalogEvent(actor.ID, "model.updated", "llm_model", model.ID))
	if err != nil || updated.MaxOutputTokens != 8192 || !updated.Capabilities.Streaming {
		t.Fatalf("UpdateModel() = %#v, %v", updated, err)
	}
	allModels, err := catalog.ListModels(ctx, nil)
	if err != nil || len(allModels) == 0 {
		t.Fatalf("ListModels(nil) = %#v, %v", allModels, err)
	}
	scoped, err := catalog.ListModels(ctx, &provider.ID)
	if err != nil || len(scoped) != 1 || scoped[0].ID != model.ID {
		t.Fatalf("ListModels(provider) = %#v, %v", scoped, err)
	}
	emptyProvider := uuid.New()
	if scoped, err := catalog.ListModels(ctx, &emptyProvider); err != nil || len(scoped) != 0 {
		t.Fatalf("ListModels(empty provider) = %#v, %v", scoped, err)
	}

	// 策略版本号：新名从 1 开始，已有最大版本 +1。
	if next, err := catalog.NextPolicyVersion(ctx, "query_policy_"+suffix); err != nil || next != 1 {
		t.Fatalf("NextPolicyVersion(new) = %d, %v", next, err)
	}
	policy, err := catalog.CreatePolicy(ctx, actor.ID, biz.ModelPolicy{
		ID: policyID, Name: "query_policy_" + suffix, Version: 1, MinContextTokens: biz.MinGenerationContextTokens,
		Status: biz.PolicyDraft, Candidates: []biz.PolicyCandidate{{ModelID: model.ID, Priority: 1}},
	}, catalogEvent(actor.ID, "model_policy.created", "model_policy", policyID))
	if err != nil {
		t.Fatal(err)
	}
	if next, err := catalog.NextPolicyVersion(ctx, "query_policy_"+suffix); err != nil || next != 2 {
		t.Fatalf("NextPolicyVersion(existing) = %d, %v", next, err)
	}
	policy.Status = biz.PolicyPublished
	if _, err := catalog.UpdatePolicy(ctx, actor.ID, policy, catalogEvent(actor.ID, "model_policy.published", "model_policy", policy.ID)); err != nil {
		t.Fatal(err)
	}
	policies, err := catalog.ListPolicies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, item := range policies {
		if item.ID == policy.ID {
			found = true
			if len(item.Candidates) != 1 || item.Candidates[0].ModelID != model.ID {
				t.Fatalf("policy candidates = %#v", item.Candidates)
			}
		}
	}
	if !found {
		t.Fatalf("ListPolicies missing %s", policy.ID)
	}
}
