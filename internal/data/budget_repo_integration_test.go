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

// TestUsageCountersIncrementAndRead 验证模型调用结算按用户、Agent、Provider 原子累加每日预算。
func TestUsageCountersIncrementAndRead(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	users := NewUserRepository(appDB, authDB)
	user, err := users.CreateUser(ctx, biz.User{
		Username: "budget_user_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	agentID, providerID := uuid.New(), uuid.New()
	if err := adminDB.Exec("INSERT INTO agents(id, user_id, name, mode, version, status) VALUES (?, ?, ?, 'react', 1, 'draft')",
		agentID, user.ID, "budget_agent_"+suffix).Error; err != nil {
		t.Fatal(err)
	}
	if err := adminDB.Exec("INSERT INTO llm_providers(id, name, base_url, api_key_encrypted, key_version) VALUES (?, ?, 'https://budget.example.com', ?, 'v1')",
		providerID, "budget_provider_"+suffix, []byte("ciphertext")).Error; err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = adminDB.Exec("DELETE FROM usage_counters WHERE scope_id IN (?, ?, ?)", user.ID, agentID, providerID).Error
		_ = adminDB.Exec("DELETE FROM llm_providers WHERE id = ?", providerID).Error
		_ = adminDB.Exec("DELETE FROM users WHERE id = ?", user.ID).Error
	}()

	sessions := NewSessionRepository(appDB)
	runs := NewRunRepository(appDB)
	calls := NewModelCallRepository(appDB)
	repository := NewBudgetRepository(appDB)
	sessionID, runID := uuid.New(), uuid.New()
	if _, err := sessions.CreateSession(ctx, biz.Session{
		ID: sessionID, UserID: user.ID, Title: "budget", Source: biz.SessionSourceWeb,
		Status: biz.SessionActive, Version: 1,
	}, testOutboxEvent(user.ID, "session.created")); err != nil {
		t.Fatal(err)
	}
	if _, err := runs.CreateRun(ctx, biz.Run{
		ID: runID, SessionID: sessionID, UserID: user.ID, AgentID: &agentID,
		Mode: biz.AgentModeReact, Status: biz.RunQueued,
		ConfigSnapshot: map[string]any{}, ModelPolicySnapshot: map[string]any{},
	}); err != nil {
		t.Fatal(err)
	}
	call, err := calls.CreateModelCall(ctx, biz.ModelCall{
		ID: uuid.New(), RunID: runID, UserID: user.ID, AttemptNo: 1,
		ProviderID: &providerID, Status: biz.ModelCallRunning, CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := calls.FinishModelCall(ctx, user.ID, call.ID, biz.ModelCallUpdate{
		Status: biz.ModelCallSucceeded, InputTokens: 1000, OutputTokens: 500,
		EstimatedCost: 0.25, CompletedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	window := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	usage, err := repository.GetUsage(ctx, biz.BudgetScopeUser, user.ID, window)
	if err != nil || usage.InputTokens != 1000 || usage.OutputTokens != 500 || usage.EstimatedCost != 0.25 {
		t.Fatalf("user usage = %#v, %v", usage, err)
	}
	agentUsage, err := repository.GetUsage(ctx, biz.BudgetScopeAgent, agentID, window)
	if err != nil || agentUsage.TotalTokens() != 1500 {
		t.Fatalf("agent usage = %#v, %v", agentUsage, err)
	}
	providerUsage, err := repository.GetUsage(ctx, biz.BudgetScopeProvider, providerID, window)
	if err != nil || providerUsage.TotalTokens() != 1500 {
		t.Fatalf("provider usage = %#v, %v", providerUsage, err)
	}
	// 重复结算必须冲突且不重复计数。
	if _, err := calls.FinishModelCall(ctx, user.ID, call.ID, biz.ModelCallUpdate{
		Status: biz.ModelCallSucceeded, InputTokens: 1000, OutputTokens: 500,
		EstimatedCost: 0.25, CompletedAt: now,
	}); err == nil {
		t.Fatal("duplicate settlement was accepted")
	}
	usage, err = repository.GetUsage(ctx, biz.BudgetScopeUser, user.ID, window)
	if err != nil || usage.TotalTokens() != 1500 {
		t.Fatalf("usage changed after duplicate settlement: %#v, %v", usage, err)
	}

	// 零用量 Attempt 不产生新的计数行，GetUsage 对缺失窗口返回零值。
	zeroCall, err := calls.CreateModelCall(ctx, biz.ModelCall{
		ID: uuid.New(), RunID: runID, UserID: user.ID, AttemptNo: 2,
		ProviderID: &providerID, Status: biz.ModelCallRunning, CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := calls.FinishModelCall(ctx, user.ID, zeroCall.ID, biz.ModelCallUpdate{
		Status: biz.ModelCallFailed, ErrorClass: "retryable_failure", CompletedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	usage, err = repository.GetUsage(ctx, biz.BudgetScopeUser, user.ID, window)
	if err != nil || usage.TotalTokens() != 1500 {
		t.Fatalf("zero usage changed counters: %#v, %v", usage, err)
	}
	nextWindow := window.Add(24 * time.Hour)
	if usage, err := repository.GetUsage(ctx, biz.BudgetScopeUser, user.ID, nextWindow); err != nil || usage.TotalTokens() != 0 {
		t.Fatalf("next window usage = %#v, %v", usage, err)
	}
}
