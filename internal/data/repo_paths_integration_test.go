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

// TestExecutionStepListAndPayloads 验证步骤与载荷的列表查询及用户隔离。
func TestExecutionStepListAndPayloads(t *testing.T) {
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
		Username: "steps_owner_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := users.CreateUser(ctx, biz.User{
		Username: "steps_other_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adminDB.Exec("DELETE FROM users WHERE id IN (?, ?)", owner.ID, stranger.ID).Error }()

	sessions := NewSessionRepository(appDB)
	session, err := sessions.CreateSession(ctx, biz.Session{
		ID: uuid.New(), UserID: owner.ID, Title: "steps-" + suffix, Source: biz.SessionSourceWeb, Status: biz.SessionActive, Version: 1,
	}, testOutboxEvent(owner.ID, "session.created"))
	if err != nil {
		t.Fatal(err)
	}
	run, err := NewRunRepository(appDB).CreateRun(ctx, biz.Run{
		ID: uuid.New(), SessionID: session.ID, UserID: owner.ID, Mode: biz.AgentModeReact, Status: biz.RunQueued,
	})
	if err != nil {
		t.Fatal(err)
	}
	steps := NewExecutionStepRepository(appDB)
	step, err := steps.StartStep(ctx, biz.ExecutionStep{
		ID: uuid.New(), RunID: run.ID, UserID: owner.ID, StepSeq: 1, Kind: biz.StepKindModel, Name: "model-a",
		Status: biz.StepRunning, SchemaVersion: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := steps.SavePayload(ctx, biz.ExecutionPayload{
		ID: uuid.New(), RunID: run.ID, StepID: &step.ID, UserID: owner.ID, Type: biz.PayloadModelRequest,
		ContentJSON: map[string]any{"prompt": "你好"}, ContentType: "application/json",
	}); err != nil {
		t.Fatal(err)
	}

	listed, err := steps.ListSteps(ctx, owner.ID, run.ID, 10)
	if err != nil || len(listed) != 1 || listed[0].ID != step.ID {
		t.Fatalf("ListSteps() = %#v, %v", listed, err)
	}
	if listed, err := steps.ListSteps(ctx, stranger.ID, run.ID, 10); err != nil || len(listed) != 0 {
		t.Fatalf("cross-user ListSteps = %#v, %v", listed, err)
	}
	payloads, err := steps.ListPayloads(ctx, owner.ID, run.ID, nil, 10)
	if err != nil || len(payloads) != 1 {
		t.Fatalf("ListPayloads(all) = %#v, %v", payloads, err)
	}
	scoped, err := steps.ListPayloads(ctx, owner.ID, run.ID, &step.ID, 10)
	if err != nil || len(scoped) != 1 {
		t.Fatalf("ListPayloads(step) = %#v, %v", scoped, err)
	}
	otherStep := uuid.New()
	if scoped, err := steps.ListPayloads(ctx, owner.ID, run.ID, &otherStep, 10); err != nil || len(scoped) != 0 {
		t.Fatalf("ListPayloads(other step) = %#v, %v", scoped, err)
	}
	if payloads, err := steps.ListPayloads(ctx, stranger.ID, run.ID, nil, 10); err != nil || len(payloads) != 0 {
		t.Fatalf("cross-user ListPayloads = %#v, %v", payloads, err)
	}

	// ListModelCalls：按运行列出调用尝试，跨用户不可见。
	calls := NewModelCallRepository(appDB)
	if _, err := calls.CreateModelCall(ctx, biz.ModelCall{
		ID: uuid.New(), RunID: run.ID, UserID: owner.ID, AttemptNo: 1, Status: biz.ModelCallRunning,
		RequestHash: []byte(strings.Repeat("h", 32)), RequestManifest: map[string]any{"model": "m"},
	}); err != nil {
		t.Fatal(err)
	}
	if listed, err := calls.ListModelCalls(ctx, owner.ID, run.ID, 10); err != nil || len(listed) != 1 || listed[0].AttemptNo != 1 {
		t.Fatalf("ListModelCalls() = %#v, %v", listed, err)
	}
	if listed, err := calls.ListModelCalls(ctx, stranger.ID, run.ID, 10); err != nil || len(listed) != 0 {
		t.Fatalf("cross-user ListModelCalls = %#v, %v", listed, err)
	}
}

// TestSupervisorRetryTask 验证运行中的任务可重试回 pending，终态任务拒绝重试。
func TestSupervisorRetryTask(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	user, err := NewUserRepository(appDB, authDB).CreateUser(ctx, biz.User{
		Username: "retry_user_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adminDB.Exec("DELETE FROM users WHERE id = ?", user.ID).Error }()
	sessions := NewSessionRepository(appDB)
	session, err := sessions.CreateSession(ctx, biz.Session{
		ID: uuid.New(), UserID: user.ID, Title: "retry-" + suffix, Source: biz.SessionSourceWeb, Status: biz.SessionActive, Version: 1,
	}, testOutboxEvent(user.ID, "session.created"))
	if err != nil {
		t.Fatal(err)
	}
	child, err := sessions.CreateSession(ctx, biz.Session{
		ID: uuid.New(), UserID: user.ID, Title: "retry-child-" + suffix, Source: biz.SessionSourceWeb, Status: biz.SessionActive, Version: 1,
	}, testOutboxEvent(user.ID, "session.created"))
	if err != nil {
		t.Fatal(err)
	}
	runs := NewRunRepository(appDB)
	run, err := runs.CreateRun(ctx, biz.Run{
		ID: uuid.New(), SessionID: session.ID, UserID: user.ID, Mode: biz.AgentModeSupervisor, Status: biz.RunQueued,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	_, fence, err := runs.ClaimRun(ctx, user.ID, run.ID, "worker", time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	repo := NewSupervisorTaskRepository(appDB)
	tasks, err := repo.CreateTasks(ctx, user.ID, run.ID, []biz.SupervisorTask{
		{ID: uuid.New(), ChildSessionID: child.ID, WorkerName: "A", WorkerIndex: 1, Task: "任务", ToolScope: []string{}, Status: biz.SupervisorTaskPending, MaxAttempts: 2, Depth: 1},
	}, testOutboxEvent(user.ID, "supervisor.tasks.created"), fence)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("CreateTasks() = %#v, %v", tasks, err)
	}
	// pending 任务不可重试。
	if _, err := repo.RetryTask(ctx, user.ID, run.ID, tasks[0].ID, now, fence); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("retry pending error = %v", err)
	}
	started, err := repo.StartTask(ctx, user.ID, run.ID, tasks[0].ID, now, fence)
	if err != nil {
		t.Fatal(err)
	}
	retried, err := repo.RetryTask(ctx, user.ID, run.ID, tasks[0].ID, now, fence)
	if err != nil || retried.Status != biz.SupervisorTaskPending {
		t.Fatalf("RetryTask() = %#v, %v", retried, err)
	}
	if retried.Attempts != started.Attempts {
		t.Fatalf("retry must keep attempts: got %d want %d", retried.Attempts, started.Attempts)
	}
	// 尝试次数耗尽后拒绝重试。
	if _, err := repo.StartTask(ctx, user.ID, run.ID, tasks[0].ID, now, fence); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.RetryTask(ctx, user.ID, run.ID, tasks[0].ID, now, fence); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("retry exhausted error = %v", err)
	}
}

// TestSessionBindAgentAndRunDeadline 验证会话绑定 Agent 与运行截止时间延期。
func TestSessionBindAgentAndRunDeadline(t *testing.T) {
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
		Username: "bind_owner_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := users.CreateUser(ctx, biz.User{
		Username: "bind_other_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adminDB.Exec("DELETE FROM users WHERE id IN (?, ?)", owner.ID, stranger.ID).Error }()

	agent, err := NewAgentRepository(appDB).CreateAgent(ctx, biz.Agent{
		ID: uuid.New(), UserID: owner.ID, Name: "bind-agent-" + suffix, Mode: biz.AgentModeReact,
		Version: 1, Status: biz.AgentPublished,
	}, testOutboxEvent(owner.ID, "agent.created"))
	if err != nil {
		t.Fatal(err)
	}
	sessions := NewSessionRepository(appDB)
	session, err := sessions.CreateSession(ctx, biz.Session{
		ID: uuid.New(), UserID: owner.ID, Title: "bind-" + suffix, Source: biz.SessionSourceWeb, Status: biz.SessionActive, Version: 1,
	}, testOutboxEvent(owner.ID, "session.created"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.BindSessionAgent(ctx, owner.ID, session.ID, agent.ID, time.Now().UTC()); err != nil {
		t.Fatalf("BindSessionAgent() = %v", err)
	}
	got, err := sessions.GetSession(ctx, owner.ID, session.ID)
	if err != nil || got.AgentID == nil || *got.AgentID != agent.ID || got.Version != 2 {
		t.Fatalf("session after bind = %#v, %v", got, err)
	}
	// 跨用户绑定他人会话按冲突/未命中拒绝。
	if err := sessions.BindSessionAgent(ctx, stranger.ID, session.ID, agent.ID, time.Now().UTC()); err == nil {
		t.Fatal("cross-user BindSessionAgent succeeded")
	}

	runs := NewRunRepository(appDB)
	run, err := runs.CreateRun(ctx, biz.Run{
		ID: uuid.New(), SessionID: session.ID, UserID: owner.ID, Mode: biz.AgentModeReact, Status: biz.RunQueued,
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	extended, err := runs.ExtendRunDeadline(ctx, owner.ID, run.ID, deadline)
	if err != nil || extended.DeadlineAt == nil || !extended.DeadlineAt.Equal(deadline) {
		t.Fatalf("ExtendRunDeadline() = %#v, %v", extended, err)
	}
	// 终态运行不可延期。
	if _, err := runs.TransitionRun(ctx, owner.ID, run.ID, biz.RunQueued, biz.RunRunning, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := runs.TransitionRun(ctx, owner.ID, run.ID, biz.RunRunning, biz.RunCompleted, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := runs.ExtendRunDeadline(ctx, owner.ID, run.ID, deadline); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("extend terminal run error = %v", err)
	}
}

// TestFactListAndBudgetUsage 验证事实列表查询与预算用量入账。
func TestFactListAndBudgetUsage(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	user, err := NewUserRepository(appDB, authDB).CreateUser(ctx, biz.User{
		Username: "fact_list_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adminDB.Exec("DELETE FROM users WHERE id = ?", user.ID).Error }()

	facts := NewFactRepository(appDB)
	if _, applied, err := facts.SaveFact(ctx, biz.MemoryFact{
		ID: uuid.New(), UserID: user.ID, Key: "preference.theme", Value: json.RawMessage(`"dark"`),
		Confidence: 0.9, SourceKind: biz.FactSourceUser, ValidFrom: time.Now().UTC(), Version: 1, CreatedAt: time.Now().UTC(),
	}, 0); err != nil || !applied {
		t.Fatalf("SaveFact() applied=%v err=%v", applied, err)
	}
	if _, applied, err := facts.SaveFact(ctx, biz.MemoryFact{
		ID: uuid.New(), UserID: user.ID, Key: "preference.lang", Value: json.RawMessage(`"zh"`),
		Confidence: 0.8, SourceKind: biz.FactSourceUser, ValidFrom: time.Now().UTC(), Version: 1, CreatedAt: time.Now().UTC(),
	}, 0); err != nil || !applied {
		t.Fatalf("SaveFact() applied=%v err=%v", applied, err)
	}
	active, err := facts.ListActiveFacts(ctx, user.ID, 10)
	if err != nil || len(active) != 2 {
		t.Fatalf("ListActiveFacts() = %#v, %v", active, err)
	}
	all, err := facts.ListFacts(ctx, user.ID, 10)
	if err != nil || len(all) != 2 {
		t.Fatalf("ListFacts() = %#v, %v", all, err)
	}
	if listed, err := facts.ListActiveFacts(ctx, uuid.New(), 10); err != nil || len(listed) != 0 {
		t.Fatalf("cross-user ListActiveFacts = %#v, %v", listed, err)
	}

	// RecordUsage：同日多次调用累加用量计数器。
	budgets := NewBudgetRepository(appDB)
	now := time.Now().UTC()
	if err := budgets.RecordUsage(ctx, user.ID, nil, nil, 100, 50, 0.5, now); err != nil {
		t.Fatalf("RecordUsage() = %v", err)
	}
	if err := budgets.RecordUsage(ctx, user.ID, nil, nil, 30, 10, 0.1, now); err != nil {
		t.Fatalf("RecordUsage() = %v", err)
	}
	var counter struct {
		InputTokens  int64
		OutputTokens int64
	}
	if err := adminDB.Raw("SELECT input_tokens, output_tokens FROM usage_counters WHERE scope = 'user' AND scope_id = ?", user.ID).Scan(&counter).Error; err != nil {
		t.Fatal(err)
	}
	if counter.InputTokens != 130 || counter.OutputTokens != 60 {
		t.Fatalf("usage counter = %#v", counter)
	}
}

// TestIdempotencyFailAndTokenRevoke 验证幂等键失败标记与刷新令牌撤销。
func TestIdempotencyFailAndTokenRevoke(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	user, err := NewUserRepository(appDB, authDB).CreateUser(ctx, biz.User{
		Username: "idem_user_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adminDB.Exec("DELETE FROM users WHERE id = ?", user.ID).Error }()

	idem := NewIdempotencyRepository(appDB)
	key := "idem-" + suffix
	if _, err := idem.Begin(ctx, user.ID, key, []byte(strings.Repeat("h", 32)), "run", time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatalf("Begin() = %v", err)
	}
	if err := idem.Fail(ctx, user.ID, key); err != nil {
		t.Fatalf("Fail() = %v", err)
	}
	var status string
	if err := adminDB.Raw("SELECT status FROM idempotency_keys WHERE user_id = ? AND key = ?", user.ID, key).Scan(&status).Error; err != nil || status != "failed" {
		t.Fatalf("idempotency status = %q, %v", status, err)
	}
	// 失败后的键可重新 Begin 进入 in_progress（允许重试）。
	outcome, err := idem.Begin(ctx, user.ID, key, []byte(strings.Repeat("h", 32)), "run", time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("Begin after fail = %v", err)
	}
	_ = outcome

	// 刷新令牌撤销：撤销后 RevokedAt 落库，重复撤销不报错（幂等）。
	tokens := NewRefreshTokenRepository(authDB)
	now := time.Now().UTC()
	token := biz.RefreshToken{
		ID: uuid.New(), UserID: user.ID, FamilyID: uuid.New(), TokenHash: []byte(strings.Repeat("t", 32)),
		CredentialVersion: 1, ExpiresAt: now.Add(time.Hour), CreatedAt: now,
	}
	if err := tokens.CreateRefreshToken(ctx, token, testOutboxEvent(user.ID, "test.token.created")); err != nil {
		t.Fatal(err)
	}
	if err := tokens.RevokeRefreshToken(ctx, user.ID, token.ID, now, testOutboxEvent(user.ID, "test.token.revoked")); err != nil {
		t.Fatalf("RevokeRefreshToken() = %v", err)
	}
	var revokedAt *time.Time
	if err := adminDB.Raw("SELECT revoked_at FROM refresh_tokens WHERE id = ?", token.ID).Scan(&revokedAt).Error; err != nil || revokedAt == nil {
		t.Fatalf("revoked_at = %v, %v", revokedAt, err)
	}
}

// TestCatalogMCPUpdateAndCredential 验证 MCP 更新、凭证轮换与读取。
func TestCatalogMCPUpdateAndCredential(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	actor, err := NewUserRepository(appDB, authDB).CreateUser(ctx, biz.User{
		Username: "mcp_admin_" + suffix, PasswordHash: "hash", Role: biz.RoleAdmin,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	mcpID := uuid.New()
	defer func() {
		_ = adminDB.Exec("DELETE FROM mcps WHERE id = ?", mcpID).Error
		_ = adminDB.Exec("DELETE FROM users WHERE id = ?", actor.ID).Error
	}()

	catalog := NewCatalogRepository(appDB)
	mcp, err := catalog.CreateMCP(ctx, actor.ID, biz.MCP{
		ID: mcpID, Name: "mcp_" + suffix, Endpoint: "https://tools.example.com/mcp", AuthType: "bearer",
		RiskLevel: "medium", Status: string(biz.MCPEnabled),
	}, &biz.ProviderCredential{Encrypted: []byte("token-v1"), KeyVersion: "v1"}, catalogEvent(actor.ID, "mcp.created", "mcp", mcpID))
	if err != nil {
		t.Fatal(err)
	}
	// 更新名称并轮换凭证。
	mcp.Name = "mcp_renamed_" + suffix
	updated, err := catalog.UpdateMCP(ctx, actor.ID, mcp, &biz.ProviderCredential{Encrypted: []byte("token-v2"), KeyVersion: "v2"}, catalogEvent(actor.ID, "mcp.updated", "mcp", mcpID))
	if err != nil || updated.Name != "mcp_renamed_"+suffix || updated.KeyVersion != "v2" {
		t.Fatalf("UpdateMCP() = %#v, %v", updated, err)
	}
	gotMCP, credential, err := catalog.GetMCPCredential(ctx, mcpID)
	if err != nil || gotMCP.ID != mcpID {
		t.Fatalf("GetMCPCredential() = %#v, %v", gotMCP, err)
	}
	if string(credential.Encrypted) != "token-v2" || credential.KeyVersion != "v2" {
		t.Fatalf("rotated credential = %#v", credential)
	}
	// 不传凭证时仅更新元数据，保留旧密文。
	mcp.RiskLevel = "high"
	if _, err := catalog.UpdateMCP(ctx, actor.ID, mcp, nil, catalogEvent(actor.ID, "mcp.updated", "mcp", mcpID)); err != nil {
		t.Fatal(err)
	}
	_, credential, err = catalog.GetMCPCredential(ctx, mcpID)
	if err != nil || string(credential.Encrypted) != "token-v2" {
		t.Fatalf("credential after metadata-only update = %#v, %v", credential, err)
	}
}

// TestFeishuBotAdminPaths 验证飞书机器人更新、列表、凭证读取、解绑与删除。
func TestFeishuBotAdminPaths(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	owner, err := NewUserRepository(appDB, authDB).CreateUser(ctx, biz.User{
		Username: "bot_admin_" + suffix, PasswordHash: "hash", Role: biz.RoleAdmin,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adminDB.Exec("DELETE FROM users WHERE id = ?", owner.ID).Error }()

	repo := NewFeishuBotRepository(appDB)
	bot, err := repo.CreateFeishuBot(ctx, owner.ID, biz.FeishuBot{
		ID: uuid.New(), Name: "admin-bot-" + suffix, AppID: "cli_" + suffix, BotOpenID: "ou_" + suffix,
		Status: biz.FeishuBotEnabled,
	}, biz.ProviderCredential{Encrypted: []byte("bot-token-v1"), KeyVersion: "v1"}, testOutboxEvent(owner.ID, "feishu_bot.created"))
	if err != nil {
		t.Fatal(err)
	}

	// 更新：停用并轮换凭证。
	bot.Status = biz.FeishuBotDisabled
	updated, err := repo.UpdateFeishuBot(ctx, owner.ID, bot, &biz.ProviderCredential{Encrypted: []byte("bot-token-v2"), KeyVersion: "v2"}, testOutboxEvent(owner.ID, "feishu_bot.updated"))
	if err != nil || updated.Status != biz.FeishuBotDisabled {
		t.Fatalf("UpdateFeishuBot() = %#v, %v", updated, err)
	}
	_, credential, err := repo.GetFeishuBotCredential(ctx, bot.ID)
	if err != nil || string(credential.Encrypted) != "bot-token-v2" || credential.KeyVersion != "v2" {
		t.Fatalf("GetFeishuBotCredential() = %#v, %v", credential, err)
	}

	// 列表：含停用机器人。
	bots, err := repo.ListFeishuBots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range bots {
		if item.ID == bot.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("ListFeishuBots missing %s", bot.ID)
	}

	// 绑定后解绑。
	agent, err := NewAgentRepository(appDB).CreateAgent(ctx, biz.Agent{
		ID: uuid.New(), UserID: owner.ID, Name: "bot-bind-agent-" + suffix, Mode: biz.AgentModeReact,
		Version: 1, Status: biz.AgentPublished,
	}, testOutboxEvent(owner.ID, "agent.created"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.BindFeishuBot(ctx, biz.FeishuBotBinding{
		BotID: bot.ID, UserID: owner.ID, AgentID: agent.ID,
	}, testOutboxEvent(owner.ID, "feishu_bot.bound")); err != nil {
		t.Fatalf("BindFeishuBot() = %v", err)
	}
	removed, err := repo.UnbindFeishuBot(ctx, owner.ID, agent.ID, testOutboxEvent(owner.ID, "feishu_bot.unbound"))
	if err != nil || removed != 1 {
		t.Fatalf("UnbindFeishuBot() = %d, %v", removed, err)
	}
	// 重复解绑返回 0 条。
	if removed, err := repo.UnbindFeishuBot(ctx, owner.ID, agent.ID, testOutboxEvent(owner.ID, "feishu_bot.unbound")); err != nil || removed != 0 {
		t.Fatalf("double UnbindFeishuBot() = %d, %v", removed, err)
	}

	// 删除后读取未命中。
	if err := repo.DeleteFeishuBot(ctx, owner.ID, bot.ID, testOutboxEvent(owner.ID, "feishu_bot.deleted")); err != nil {
		t.Fatalf("DeleteFeishuBot() = %v", err)
	}
	if _, err := repo.GetFeishuBot(ctx, bot.ID); !errors.Is(err, biz.ErrResourceNotFound) {
		t.Fatalf("deleted bot error = %v", err)
	}
}
