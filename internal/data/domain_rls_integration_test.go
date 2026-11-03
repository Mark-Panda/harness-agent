//go:build integration

package data

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func TestDomainSchemaRLSAndModelConstraints(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	admin := openIntegrationDatabase(t, "postgres://harness:harness@localhost:5432/harness?sslmode=disable")
	app := openIntegrationDatabase(t, "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable")
	auth := openIntegrationDatabase(t, "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable")
	users := NewUserRepository(app, auth)
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	userA, err := users.CreateUser(ctx, biz.User{Username: "domain_a_" + suffix, PasswordHash: "hash", Role: biz.RoleUser, Status: biz.UserStatusActive, CredentialVersion: 1}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	userB, err := users.CreateUser(ctx, biz.User{Username: "domain_b_" + suffix, PasswordHash: "hash", Role: biz.RoleUser, Status: biz.UserStatusActive, CredentialVersion: 1}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	var providerID, policyID uuid.UUID
	defer func() {
		_ = admin.Exec("DELETE FROM users WHERE id IN (?, ?)", userA.ID, userB.ID).Error
		if policyID != uuid.Nil {
			_ = admin.Exec("DELETE FROM model_policies WHERE id = ?", policyID).Error
		}
		if providerID != uuid.Nil {
			_ = admin.Exec("DELETE FROM llm_models WHERE provider_id = ?", providerID).Error
			_ = admin.Exec("DELETE FROM llm_providers WHERE id = ?", providerID).Error
		}
	}()
	providerID = uuid.New()
	if err := admin.Exec("INSERT INTO llm_providers(id, name, base_url, api_key_encrypted, key_version) VALUES (?, ?, ?, ?, ?)",
		providerID, "provider_"+suffix, "https://example.com", []byte("encrypted"), "v1").Error; err != nil {
		t.Fatal(err)
	}
	if err := admin.Exec("INSERT INTO llm_models(provider_id, remote_model_name, model_kind, context_window_tokens, max_output_tokens) VALUES (?, ?, 'chat', 128000, 4096)", providerID).Error; err == nil {
		t.Fatal("database accepted a generation model below 1M context")
	}
	modelID := uuid.New()
	if err := admin.Exec("INSERT INTO llm_models(id, provider_id, remote_model_name, model_kind, context_window_tokens, max_output_tokens, verification_source, verified_at) VALUES (?, ?, 'model-1m', 'chat', 1000000, 16384, 'integration-test', now())", modelID, providerID).Error; err != nil {
		t.Fatal(err)
	}
	policyID = uuid.New()
	if err := admin.Exec("INSERT INTO model_policies(id, name, version, status) VALUES (?, 'default', 1, 'published')", policyID).Error; err != nil {
		t.Fatal(err)
	}
	if err := admin.Exec("INSERT INTO model_policy_candidates(policy_id, model_id, priority) VALUES (?, ?, 1)", policyID, modelID).Error; err != nil {
		t.Fatal(err)
	}
	agentID, sessionID, runID, stepID, planID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if err := admin.Exec("INSERT INTO agents(id, user_id, name, mode, model_policy_id, model_policy_version, status) VALUES (?, ?, 'agent', 'react', ?, 1, 'published')", agentID, userA.ID, policyID).Error; err != nil {
		t.Fatal(err)
	}
	if err := admin.Exec("INSERT INTO sessions(id, user_id, agent_id, title) VALUES (?, ?, ?, 'session')", sessionID, userA.ID, agentID).Error; err != nil {
		t.Fatal(err)
	}
	if err := admin.Exec("INSERT INTO session_members(session_id, user_id, member_role) VALUES (?, ?, 'owner')", sessionID, userA.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := admin.Exec("INSERT INTO messages(session_id, user_id, role, content) VALUES (?, ?, 'user', 'hello')", sessionID, userA.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := admin.Exec("INSERT INTO agent_runs(id, session_id, user_id, agent_id, agent_version, mode) VALUES (?, ?, ?, ?, 1, 'react')", runID, sessionID, userA.ID, agentID).Error; err != nil {
		t.Fatal(err)
	}
	if err := admin.Exec("INSERT INTO execution_steps(id, run_id, user_id, step_seq, kind, status) VALUES (?, ?, ?, 1, 'model', 'running')", stepID, runID, userA.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := admin.Exec("INSERT INTO execution_payloads(run_id, step_id, user_id, payload_type, capture_mode, content_json) VALUES (?, ?, ?, 'model_request', 'redacted', '{}')", runID, stepID, userA.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := admin.Exec("INSERT INTO run_events(run_id, user_id, seq, event_id, event_type, occurred_at) VALUES (?, ?, 1, gen_random_uuid(), 'meta', now())", runID, userA.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := admin.Exec("INSERT INTO model_calls(run_id, step_id, user_id, attempt_no, provider_id, model_id) VALUES (?, ?, ?, 1, ?, ?)", runID, stepID, userA.ID, providerID, modelID).Error; err != nil {
		t.Fatal(err)
	}
	if err := admin.Exec("INSERT INTO plans(id, run_id, session_id, user_id, version) VALUES (?, ?, ?, ?, 1)", planID, runID, sessionID, userA.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := admin.Exec("INSERT INTO plan_steps(plan_id, user_id, step_no, title) VALUES (?, ?, 1, 'step')", planID, userA.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := admin.Exec("INSERT INTO session_summaries(session_id, user_id, version, summary_text) VALUES (?, ?, 1, 'summary')", sessionID, userA.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := admin.Exec("INSERT INTO memory_facts(user_id, session_id, fact_key, fact_value, confidence) VALUES (?, ?, 'preference', '{\"value\":\"tea\"}', 0.9)", userA.ID, sessionID).Error; err != nil {
		t.Fatal(err)
	}
	if err := admin.Exec("INSERT INTO external_conversation_bindings(user_id, provider, tenant_key, chat_id, thread_id, session_id) VALUES (?, 'feishu', 'tenant', 'chat', ?, ?)", userA.ID, "thread_"+suffix, sessionID).Error; err != nil {
		t.Fatal(err)
	}
	if err := admin.Exec("INSERT INTO jobs(user_id, job_type, dedupe_key) VALUES (?, 'summary', ?)", userA.ID, "job_"+suffix).Error; err != nil {
		t.Fatal(err)
	}
	if err := admin.Exec("INSERT INTO outbox_events(user_id, event_type, aggregate_type, aggregate_id, dedupe_key) VALUES (?, 'run.started', 'run', ?, ?)", userA.ID, runID, "outbox_"+suffix).Error; err != nil {
		t.Fatal(err)
	}
	if err := admin.Exec("INSERT INTO idempotency_keys(user_id, key, request_hash, expires_at) VALUES (?, ?, decode(?, 'hex'), now() + interval '1 hour')", userA.ID, "key_"+suffix, fmt.Sprintf("%064x", 1)).Error; err != nil {
		t.Fatal(err)
	}
	if err := admin.Exec("INSERT INTO deletion_requests(user_id, resource_type, resource_id, requested_by) VALUES (?, 'session', ?, 'test')", userA.ID, sessionID).Error; err != nil {
		t.Fatal(err)
	}

	tables := []string{
		"agents", "sessions", "session_members", "messages", "agent_runs", "execution_steps", "execution_payloads",
		"run_events", "model_calls", "plans", "plan_steps", "session_summaries", "memory_facts",
		"external_conversation_bindings", "jobs", "outbox_events", "idempotency_keys", "deletion_requests",
	}
	for _, table := range tables {
		t.Run(table, func(t *testing.T) {
			var ownCount, otherCount int64
			if err := WithScope(ctx, app, Scope{UserID: userA.ID}, func(tx *gorm.DB) error {
				return tx.Table(table).Where("user_id = ?", userA.ID).Count(&ownCount).Error
			}); err != nil {
				t.Fatalf("own query: %v", err)
			}
			if err := WithScope(ctx, app, Scope{UserID: userB.ID}, func(tx *gorm.DB) error {
				return tx.Table(table).Where("user_id = ?", userA.ID).Count(&otherCount).Error
			}); err != nil {
				t.Fatalf("cross-user query: %v", err)
			}
			expected := int64(1)
			if table == "outbox_events" {
				expected = 2
			}
			if ownCount != expected || otherCount != 0 {
				t.Fatalf("own=%d other=%d expected=%d", ownCount, otherCount, expected)
			}
		})
	}
}
