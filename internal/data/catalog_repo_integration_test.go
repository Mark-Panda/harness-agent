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

func TestCatalogRepositorySecurityAndStartupAdmission(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	actor, err := NewUserRepository(appDB, authDB).CreateUser(ctx, biz.User{
		Username: "catalog_admin_" + suffix, PasswordHash: "hash", Role: biz.RoleAdmin, Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	repository := NewCatalogRepository(appDB)
	providerID := uuid.New()
	defer func() {
		_ = adminDB.Exec("DELETE FROM model_policies WHERE name = ?", "policy_"+suffix).Error
		_ = adminDB.Exec("DELETE FROM llm_providers WHERE id = ?", providerID).Error
		_ = adminDB.Exec("DELETE FROM users WHERE id = ?", actor.ID).Error
	}()

	unscopedProvider := providerModel{ID: uuid.New(), Name: "denied_" + suffix, ProviderType: "openai_compatible", BaseURL: "https://example.com", APIKeyEncrypted: []byte("secret"), KeyVersion: "v1", Status: "enabled"}
	if err := appDB.WithContext(ctx).Create(&unscopedProvider).Error; err == nil {
		t.Fatal("application role wrote platform catalog without an administrative database scope")
	}

	provider, err := repository.CreateProvider(ctx, actor.ID, biz.Provider{
		ID: providerID, Name: "provider_" + suffix, ProviderType: "openai_compatible", BaseURL: "https://models.example.com", Status: biz.ProviderEnabled,
	}, biz.ProviderCredential{Encrypted: []byte("encrypted"), KeyVersion: "v1"}, catalogEvent(actor.ID, "provider.created", "llm_provider", providerID))
	if err != nil {
		t.Fatal(err)
	}
	var stored struct {
		APIKeyEncrypted []byte
	}
	if err := adminDB.Raw("SELECT api_key_encrypted FROM llm_providers WHERE id = ?", provider.ID).Scan(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if string(stored.APIKeyEncrypted) != "encrypted" {
		t.Fatalf("unexpected stored credential: %q", stored.APIKeyEncrypted)
	}

	now := time.Now().UTC()
	modelID := uuid.New()
	model, err := repository.CreateModel(ctx, actor.ID, biz.Model{
		ID: modelID, ProviderID: provider.ID, RemoteModelName: "generation-1m", Kind: biz.ModelKindChat,
		ContextWindowTokens: biz.MinGenerationContextTokens, MaxOutputTokens: 16384,
		Capabilities: biz.ModelCapabilities{Streaming: true, Tools: true}, Status: biz.ModelEnabled,
		VerificationSource: "integration-test", VerifiedAt: &now,
	}, catalogEvent(actor.ID, "model.created", "llm_model", modelID))
	if err != nil {
		t.Fatal(err)
	}
	if !model.Capabilities.Streaming || !model.Capabilities.Tools {
		t.Fatalf("model capabilities were not persisted: %#v", model.Capabilities)
	}

	policyID := uuid.New()
	policy, err := repository.CreatePolicy(ctx, actor.ID, biz.ModelPolicy{
		ID: policyID, Name: "policy_" + suffix, Version: 1, MinContextTokens: biz.MinGenerationContextTokens,
		Status: biz.PolicyDraft, Candidates: []biz.PolicyCandidate{{ModelID: model.ID, Priority: 1}},
	}, catalogEvent(actor.ID, "model_policy.created", "model_policy", policyID))
	if err != nil {
		t.Fatal(err)
	}
	policy.Status = biz.PolicyPublished
	if _, err := repository.UpdatePolicy(ctx, actor.ID, policy, catalogEvent(actor.ID, "model_policy.published", "model_policy", policy.ID)); err != nil {
		t.Fatal(err)
	}
	if err := repository.ValidateCatalog(ctx); err != nil {
		t.Fatalf("valid catalog failed startup admission: %v", err)
	}

	provider.Status = biz.ProviderDisabled
	if _, err := repository.UpdateProvider(ctx, actor.ID, provider, nil, catalogEvent(actor.ID, "provider.updated", "llm_provider", provider.ID)); err != nil {
		t.Fatal(err)
	}
	if err := repository.ValidateCatalog(ctx); err != nil {
		t.Fatalf("runtime provider disablement must not block restart: %v", err)
	}
	if err := adminDB.Exec("UPDATE llm_models SET model_kind = 'embedding' WHERE id = ?", model.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := repository.ValidateCatalog(ctx); err == nil {
		t.Fatal("startup admission accepted a published non-generation candidate")
	}

	if _, err := repository.GetPolicy(ctx, policy.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.GetModel(ctx, model.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.GetProvider(ctx, provider.ID); err != nil {
		t.Fatal(err)
	}
	if err := repository.DeletePolicy(ctx, actor.ID, policy.ID, catalogEvent(actor.ID, "model_policy.deleted", "model_policy", policy.ID)); err != nil {
		t.Fatal(err)
	}
	if err := repository.DeleteModel(ctx, actor.ID, model.ID, catalogEvent(actor.ID, "model.deleted", "llm_model", model.ID)); err != nil {
		t.Fatal(err)
	}
	if err := repository.DeleteProvider(ctx, actor.ID, provider.ID, catalogEvent(actor.ID, "provider.deleted", "llm_provider", provider.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.GetProvider(ctx, provider.ID); !errors.Is(err, biz.ErrResourceNotFound) {
		t.Fatalf("deleted provider lookup error = %v", err)
	}
}

func catalogEvent(userID uuid.UUID, action, aggregateType string, aggregateID uuid.UUID) biz.OutboxEvent {
	return biz.OutboxEvent{
		UserID: userID, EventType: action, AggregateType: aggregateType, AggregateID: aggregateID.String(),
		DedupeKey: action + ":" + aggregateID.String() + ":" + uuid.NewString(), Payload: map[string]any{"outcome": "success"},
	}
}
