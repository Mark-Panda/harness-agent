//go:build integration

package data

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
)

func TestAgentAndSessionRepositoriesEnforceOwnership(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	users := NewUserRepository(appDB, authDB)
	userA, err := users.CreateUser(ctx, biz.User{Username: "agent_a_" + suffix, PasswordHash: "hash", Role: biz.RoleUser, Status: biz.UserStatusActive, CredentialVersion: 1}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	userB, err := users.CreateUser(ctx, biz.User{Username: "agent_b_" + suffix, PasswordHash: "hash", Role: biz.RoleUser, Status: biz.UserStatusActive, CredentialVersion: 1}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	providerID := uuid.New()
	defer func() {
		_ = adminDB.Exec("DELETE FROM users WHERE id IN (?, ?)", userA.ID, userB.ID).Error
		_ = adminDB.Exec("DELETE FROM model_policies WHERE name = ?", "agent_policy_"+suffix).Error
		_ = adminDB.Exec("DELETE FROM llm_providers WHERE id = ?", providerID).Error
	}()

	catalog := NewCatalogRepository(appDB)
	provider, err := catalog.CreateProvider(ctx, userA.ID, biz.Provider{ID: providerID, Name: "agent_provider_" + suffix, ProviderType: "openai_compatible", BaseURL: "https://models.example.com", Status: biz.ProviderEnabled}, biz.ProviderCredential{Encrypted: []byte("ciphertext"), KeyVersion: "v1"}, catalogEvent(userA.ID, "provider.created", "llm_provider", providerID))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	modelID := uuid.New()
	model, err := catalog.CreateModel(ctx, userA.ID, biz.Model{ID: modelID, ProviderID: provider.ID, RemoteModelName: "generation", Kind: biz.ModelKindChat, ContextWindowTokens: biz.MinGenerationContextTokens, MaxOutputTokens: 8192, Status: biz.ModelEnabled, VerificationSource: "integration-test", VerifiedAt: &now}, catalogEvent(userA.ID, "model.created", "llm_model", modelID))
	if err != nil {
		t.Fatal(err)
	}
	policyID := uuid.New()
	policy, err := catalog.CreatePolicy(ctx, userA.ID, biz.ModelPolicy{ID: policyID, Name: "agent_policy_" + suffix, Version: 1, MinContextTokens: biz.MinGenerationContextTokens, Status: biz.PolicyDraft, Candidates: []biz.PolicyCandidate{{ModelID: model.ID, Priority: 1}}}, catalogEvent(userA.ID, "model_policy.created", "model_policy", policyID))
	if err != nil {
		t.Fatal(err)
	}
	policy.Status = biz.PolicyPublished
	policy, err = catalog.UpdatePolicy(ctx, userA.ID, policy, catalogEvent(userA.ID, "model_policy.published", "model_policy", policyID))
	if err != nil {
		t.Fatal(err)
	}

	agentService, _ := biz.NewAgentService(NewAgentRepository(appDB), biz.QuotaLimits{})
	principalA := biz.Principal{UserID: userA.ID, Role: biz.RoleUser}
	principalB := biz.Principal{UserID: userB.ID, Role: biz.RoleUser}
	agent, err := agentService.Create(ctx, principalA, biz.Agent{Name: "private-agent", Mode: biz.AgentModeReact, ModelPolicyID: &policy.ID, ModelPolicyVersion: &policy.Version})
	if err != nil {
		t.Fatal(err)
	}
	agent.Description = "updated draft"
	if _, err := agentService.Update(ctx, principalA, agent); err != nil {
		t.Fatal(err)
	}
	agent, err = agentService.Publish(ctx, principalA, agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _ := json.Marshal(agent.ConfigSnapshot)
	if strings.Contains(string(snapshot), "ciphertext") || strings.Contains(string(snapshot), "api_key") {
		t.Fatalf("agent snapshot leaked provider credentials: %s", snapshot)
	}
	if _, err := agentService.Get(ctx, principalB, agent.ID); !errors.Is(err, biz.ErrResourceNotFound) {
		t.Fatalf("cross-user agent lookup error = %v", err)
	}
	next, err := agentService.CreateVersion(ctx, principalA, agent.ID)
	if err != nil || next.Version != 2 || next.Status != biz.AgentDraft {
		t.Fatalf("CreateVersion() = %#v, %v", next, err)
	}
	if err := agentService.Delete(ctx, principalA, next.ID); err != nil {
		t.Fatal(err)
	}

	sessionService, _ := biz.NewSessionService(NewSessionRepository(appDB), biz.QuotaLimits{})
	session, err := sessionService.Create(ctx, principalA, biz.Session{AgentID: &agent.ID, Title: "private-session"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionService.Get(ctx, principalB, session.ID); !errors.Is(err, biz.ErrResourceNotFound) {
		t.Fatalf("cross-user session lookup error = %v", err)
	}
	if _, err := sessionService.AppendMessage(ctx, principalB, biz.Message{SessionID: session.ID, Content: "stolen"}); !errors.Is(err, biz.ErrResourceNotFound) {
		t.Fatalf("cross-user message append error = %v", err)
	}
	if _, err := sessionService.AppendMessage(ctx, principalA, biz.Message{SessionID: session.ID, Content: "first"}); err != nil {
		t.Fatal(err)
	}
	second, err := sessionService.Create(ctx, principalA, biz.Session{AgentID: &agent.ID, Title: "second"})
	if err != nil {
		t.Fatal(err)
	}
	page, err := sessionService.List(ctx, principalA, 1, "")
	if err != nil || len(page.Items) != 1 || page.NextCursor == "" {
		t.Fatalf("first session page = %#v, %v", page, err)
	}
	page, err = sessionService.List(ctx, principalA, 1, page.NextCursor)
	if err != nil || len(page.Items) != 1 || page.NextCursor != "" {
		t.Fatalf("second session page = %#v, %v", page, err)
	}
	messages, err := sessionService.ListMessages(ctx, principalA, session.ID, 20, "")
	if err != nil || len(messages.Items) != 1 || messages.Items[0].Content != "first" {
		t.Fatalf("message page = %#v, %v", messages, err)
	}
	otherMessages, err := NewSessionRepository(appDB).ListMessages(ctx, userB.ID, session.ID, 20, "")
	if err != nil || len(otherMessages.Items) != 0 {
		t.Fatalf("cross-user message list = %#v, %v", otherMessages, err)
	}
	if _, err := sessionService.Archive(ctx, principalA, second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := sessionService.AppendMessage(ctx, principalA, biz.Message{SessionID: second.ID, Content: "late"}); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("archived session append error = %v", err)
	}
}
