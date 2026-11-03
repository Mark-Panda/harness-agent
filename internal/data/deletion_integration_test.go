//go:build integration

package data

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/platform/audit"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// countDeletionRows 统计满足条件的行数，用于验证删除结果。
func countDeletionRows(t *testing.T, ctx context.Context, db *gorm.DB, query string, args ...any) int64 {
	t.Helper()
	var count int64
	if err := db.WithContext(ctx).Raw(query, args...).Scan(&count).Error; err != nil {
		t.Fatal(err)
	}
	return count
}

// TestDeletionExecutorCleansSessionAndMemory 验证会话删除清理全部派生数据与缓存，记忆删除保留会话原文。
func TestDeletionExecutorCleansSessionAndMemory(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL and Redis")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	users := NewUserRepository(appDB, authDB)
	owner, err := users.CreateUser(ctx, biz.User{
		Username: "deletion_owner_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	other, err := users.CreateUser(ctx, biz.User{
		Username: "deletion_other_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adminDB.Exec("DELETE FROM users WHERE id IN (?, ?)", owner.ID, other.ID).Error }()
	sessions := NewSessionRepository(appDB)
	summaries := NewSummaryRepository(appDB)
	facts := NewFactRepository(appDB)
	vectors := NewMemoryRepository(appDB)
	jobs := NewJobRepository(appDB)
	runs := NewRunRepository(appDB)
	redisClient := integrationRedisClient(t)
	hot, err := NewHotMessageStore(redisClient, 10, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	// 会话 A：应被完整删除。
	sessionA := createMemoryIntegrationSession(t, ctx, sessions, owner.ID, "deletion-a-"+suffix)
	messagesA := appendMemoryIntegrationMessages(t, ctx, sessions, owner.ID, sessionA, 2)
	if err := hot.Save(ctx, owner.ID, sessionA, messagesA); err != nil {
		t.Fatal(err)
	}
	firstID, firstAt := messagesA[0].ID, messagesA[0].CreatedAt
	summary := biz.SessionSummary{
		ID: uuid.New(), SessionID: sessionA, UserID: owner.ID, Version: 1, SummaryText: "待删除摘要",
		SourceMessageIDs: []uuid.UUID{firstID}, SourceEndMessageID: &firstID, SourceEndAt: &firstAt, CoveredMessageCount: 1,
	}
	if _, applied, err := summaries.SaveSummary(ctx, summary, 0, testOutboxEvent(owner.ID, "memory.summary.created")); err != nil || !applied {
		t.Fatalf("save summary: applied=%v err=%v", applied, err)
	}
	fact := biz.MemoryFact{
		ID: uuid.New(), UserID: owner.ID, SessionID: &sessionA, Key: "preference.language",
		Value: json.RawMessage(`"中文"`), Confidence: 0.9, SourceKind: biz.FactSourceUser,
		Version: 1, ValidFrom: time.Now().UTC(),
	}
	if _, applied, err := facts.SaveFact(ctx, fact, 0); err != nil || !applied {
		t.Fatalf("save fact: applied=%v err=%v", applied, err)
	}
	sessionIDCopy := sessionA
	messageID := messagesA[0].ID
	vector := biz.MemoryVector{
		ID: uuid.New(), UserID: owner.ID, SessionID: &sessionIDCopy, SourceType: biz.MemorySourceMessage,
		SourceID: &messageID, Content: "待删除向量", ContentHash: biz.MemoryContentHash("待删除向量"),
		Embedding: evalVector(1, 0), ProviderID: uuid.New(), Model: "embedding",
		Dimension: biz.EmbeddingDimension, Version: "v1", CreatedAt: time.Now().UTC(),
	}
	if err := vectors.UpsertVectors(ctx, owner.ID, []biz.MemoryVector{vector}); err != nil {
		t.Fatal(err)
	}
	if err := jobs.EnqueueMemory(ctx, biz.MemoryTask{
		UserID: owner.ID, SessionID: sessionA, MessageIDs: []uuid.UUID{messagesA[0].ID}, Versions: []string{"v1"},
	}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	runID := uuid.New()
	if _, err := runs.CreateRun(ctx, biz.Run{
		ID: runID, SessionID: sessionA, UserID: owner.ID, Mode: biz.AgentModeReact, Status: biz.RunQueued,
		ConfigSnapshot: map[string]any{}, ModelPolicySnapshot: map[string]any{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := jobs.EnqueueRun(ctx, owner.ID, runID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := adminDB.Exec(`INSERT INTO external_conversation_bindings
		(id, user_id, provider, tenant_key, chat_id, session_id) VALUES (?, ?, 'feishu', ?, ?, ?)`,
		uuid.New(), owner.ID, "tenant-"+suffix, "chat-"+suffix, sessionA).Error; err != nil {
		t.Fatal(err)
	}

	executor := NewDeletionExecutor(appDB, hot)
	if err := executor.DeleteSession(ctx, owner.ID, sessionA); err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name  string
		query string
		args  []any
	}{
		{"sessions", "SELECT count(*) FROM sessions WHERE id = ? AND user_id = ?", []any{sessionA, owner.ID}},
		{"messages", "SELECT count(*) FROM messages WHERE session_id = ?", []any{sessionA}},
		{"summaries", "SELECT count(*) FROM session_summaries WHERE session_id = ?", []any{sessionA}},
		{"facts", "SELECT count(*) FROM memory_facts WHERE session_id = ?", []any{sessionA}},
		{"vectors", "SELECT count(*) FROM memory_vectors WHERE session_id = ?", []any{sessionA}},
		{"runs", "SELECT count(*) FROM agent_runs WHERE session_id = ?", []any{sessionA}},
		{"bindings", "SELECT count(*) FROM external_conversation_bindings WHERE session_id = ?", []any{sessionA}},
		{"jobs", "SELECT count(*) FROM jobs WHERE user_id = ? AND (payload->>'session_id' = ? OR payload->>'run_id' = ?)",
			[]any{owner.ID, sessionA.String(), runID.String()}},
	}
	for _, check := range checks {
		if count := countDeletionRows(t, ctx, adminDB, check.query, check.args...); count != 0 {
			t.Fatalf("%s remains after deletion: %d", check.name, count)
		}
	}
	if _, hit, err := hot.Load(ctx, owner.ID, sessionA); err != nil || hit {
		t.Fatalf("hot cache remains after deletion: hit=%v err=%v", hit, err)
	}

	// 会话 B：删除记忆后保留会话与消息。
	sessionB := createMemoryIntegrationSession(t, ctx, sessions, owner.ID, "deletion-b-"+suffix)
	messagesB := appendMemoryIntegrationMessages(t, ctx, sessions, owner.ID, sessionB, 2)
	if err := hot.Save(ctx, owner.ID, sessionB, messagesB); err != nil {
		t.Fatal(err)
	}
	secondFirstID, secondFirstAt := messagesB[0].ID, messagesB[0].CreatedAt
	summaryB := biz.SessionSummary{
		ID: uuid.New(), SessionID: sessionB, UserID: owner.ID, Version: 1, SummaryText: "待删除记忆摘要",
		SourceMessageIDs: []uuid.UUID{secondFirstID}, SourceEndMessageID: &secondFirstID, SourceEndAt: &secondFirstAt, CoveredMessageCount: 1,
	}
	if _, applied, err := summaries.SaveSummary(ctx, summaryB, 0, testOutboxEvent(owner.ID, "memory.summary.created")); err != nil || !applied {
		t.Fatalf("save second summary: applied=%v err=%v", applied, err)
	}
	factB := biz.MemoryFact{
		ID: uuid.New(), UserID: owner.ID, SessionID: &sessionB, Key: "preference.timezone",
		Value: json.RawMessage(`"Asia/Shanghai"`), Confidence: 0.9, SourceKind: biz.FactSourceUser,
		Version: 1, ValidFrom: time.Now().UTC(),
	}
	if _, applied, err := facts.SaveFact(ctx, factB, 0); err != nil || !applied {
		t.Fatalf("save second fact: applied=%v err=%v", applied, err)
	}
	messageBID := messagesB[0].ID
	sessionBCopy := sessionB
	if err := vectors.UpsertVectors(ctx, owner.ID, []biz.MemoryVector{{
		ID: uuid.New(), UserID: owner.ID, SessionID: &sessionBCopy, SourceType: biz.MemorySourceMessage,
		SourceID: &messageBID, Content: "待删除记忆向量", ContentHash: biz.MemoryContentHash("待删除记忆向量"),
		Embedding: evalVector(1, 0), ProviderID: vector.ProviderID, Model: "embedding",
		Dimension: biz.EmbeddingDimension, Version: "v1", CreatedAt: time.Now().UTC(),
	}}); err != nil {
		t.Fatal(err)
	}

	// 另一用户会话必须保持不动。
	otherSession := createMemoryIntegrationSession(t, ctx, sessions, other.ID, "deletion-other-"+suffix)
	appendMemoryIntegrationMessages(t, ctx, sessions, other.ID, otherSession, 1)

	if err := executor.DeleteMemory(ctx, owner.ID); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		"SELECT count(*) FROM memory_vectors WHERE user_id = ?",
		"SELECT count(*) FROM memory_facts WHERE user_id = ?",
		"SELECT count(*) FROM session_summaries WHERE user_id = ?",
	} {
		if count := countDeletionRows(t, ctx, adminDB, query, owner.ID); count != 0 {
			t.Fatalf("memory remains after deletion: %s = %d", query, count)
		}
	}
	if count := countDeletionRows(t, ctx, adminDB, "SELECT count(*) FROM sessions WHERE id = ?", sessionB); count != 1 {
		t.Fatalf("session was deleted during memory deletion: %d", count)
	}
	if count := countDeletionRows(t, ctx, adminDB, "SELECT count(*) FROM messages WHERE session_id = ?", sessionB); count != 2 {
		t.Fatalf("messages were deleted during memory deletion: %d", count)
	}
	if _, hit, err := hot.Load(ctx, owner.ID, sessionB); err != nil || hit {
		t.Fatalf("memory deletion did not clear hot cache: hit=%v err=%v", hit, err)
	}
	if count := countDeletionRows(t, ctx, adminDB, "SELECT count(*) FROM sessions WHERE id = ? AND user_id = ?", otherSession, other.ID); count != 1 {
		t.Fatalf("other user session was affected: %d", count)
	}
}

// TestDeletionExecutorDeletesAccount 验证账号级删除清空全部业务数据、匿名化用户行，
// 同时保留审计日志、删除请求与 Outbox，避免破坏合规链路与审计外键。
func TestDeletionExecutorDeletesAccount(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL and Redis")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	users := NewUserRepository(appDB, authDB)
	owner, err := users.CreateUser(ctx, biz.User{
		Username: "account_owner_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	other, err := users.CreateUser(ctx, biz.User{
		Username: "account_other_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adminDB.Exec("DELETE FROM users WHERE id IN (?, ?)", owner.ID, other.ID).Error })
	sessions := NewSessionRepository(appDB)
	summaries := NewSummaryRepository(appDB)
	facts := NewFactRepository(appDB)
	vectors := NewMemoryRepository(appDB)
	runs := NewRunRepository(appDB)
	jobs := NewJobRepository(appDB)
	tokens := NewRefreshTokenRepository(authDB)
	deletions := NewDeletionRepository(appDB)
	redisClient := integrationRedisClient(t)
	hot, err := NewHotMessageStore(redisClient, 10, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	// 预先写入 Agent，并让 Session/Run/Binding 引用它，验证账号删除的外键清理顺序。
	agentID := uuid.New()
	if err := adminDB.Exec(`INSERT INTO agents(id, user_id, name, mode, version, status)
		VALUES (?, ?, ?, 'react', 1, 'published')`, agentID, owner.ID, "account-agent-"+suffix).Error; err != nil {
		t.Fatal(err)
	}
	sessionID := uuid.New()
	if _, err := sessions.CreateSession(ctx, biz.Session{
		ID: sessionID, UserID: owner.ID, AgentID: &agentID, Title: "account-deletion-" + suffix,
		Source: biz.SessionSourceWeb, Status: biz.SessionActive, Version: 1,
	}, testOutboxEvent(owner.ID, "session.created")); err != nil {
		t.Fatal(err)
	}
	messages := appendMemoryIntegrationMessages(t, ctx, sessions, owner.ID, sessionID, 2)
	if err := hot.Save(ctx, owner.ID, sessionID, messages); err != nil {
		t.Fatal(err)
	}
	firstID, firstAt := messages[0].ID, messages[0].CreatedAt
	if _, applied, err := summaries.SaveSummary(ctx, biz.SessionSummary{
		ID: uuid.New(), SessionID: sessionID, UserID: owner.ID, Version: 1, SummaryText: "账号删除摘要",
		SourceMessageIDs: []uuid.UUID{firstID}, SourceEndMessageID: &firstID, SourceEndAt: &firstAt, CoveredMessageCount: 1,
	}, 0, testOutboxEvent(owner.ID, "memory.summary.created")); err != nil || !applied {
		t.Fatalf("save summary: applied=%v err=%v", applied, err)
	}
	if _, applied, err := facts.SaveFact(ctx, biz.MemoryFact{
		ID: uuid.New(), UserID: owner.ID, SessionID: &sessionID, Key: "preference.language",
		Value: json.RawMessage(`"中文"`), Confidence: 0.9, SourceKind: biz.FactSourceUser,
		Version: 1, ValidFrom: time.Now().UTC(),
	}, 0); err != nil || !applied {
		t.Fatalf("save fact: applied=%v err=%v", applied, err)
	}
	messageID, sessionCopy := firstID, sessionID
	if err := vectors.UpsertVectors(ctx, owner.ID, []biz.MemoryVector{{
		ID: uuid.New(), UserID: owner.ID, SessionID: &sessionCopy, SourceType: biz.MemorySourceMessage,
		SourceID: &messageID, Content: "账号删除向量", ContentHash: biz.MemoryContentHash("账号删除向量"),
		Embedding: evalVector(1, 0), ProviderID: uuid.New(), Model: "embedding",
		Dimension: biz.EmbeddingDimension, Version: "v1", CreatedAt: time.Now().UTC(),
	}}); err != nil {
		t.Fatal(err)
	}
	runID := uuid.New()
	agentVersion := 1
	if _, err := runs.CreateRun(ctx, biz.Run{
		ID: runID, SessionID: sessionID, UserID: owner.ID, AgentID: &agentID, AgentVersion: &agentVersion,
		Mode: biz.AgentModeReact, Status: biz.RunQueued, ConfigSnapshot: map[string]any{}, ModelPolicySnapshot: map[string]any{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := jobs.EnqueueMemory(ctx, biz.MemoryTask{
		UserID: owner.ID, SessionID: sessionID, MessageIDs: []uuid.UUID{firstID}, Versions: []string{"v1"},
	}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := adminDB.Exec(`INSERT INTO external_conversation_bindings
		(id, user_id, provider, tenant_key, chat_id, session_id, agent_id) VALUES (?, ?, 'feishu', ?, ?, ?, ?)`,
		uuid.New(), owner.ID, "tenant-"+suffix, "chat-"+suffix, sessionID, agentID).Error; err != nil {
		t.Fatal(err)
	}
	if err := tokens.CreateRefreshToken(ctx, biz.RefreshToken{
		ID: uuid.New(), UserID: owner.ID, FamilyID: uuid.New(), TokenHash: []byte(strings.Repeat("\x01", 32)),
		CredentialVersion: 1, ExpiresAt: time.Now().UTC().Add(24 * time.Hour), CreatedAt: time.Now().UTC(),
	}, testOutboxEvent(owner.ID, "auth.refresh.issued")); err != nil {
		t.Fatal(err)
	}
	if err := adminDB.Exec(`INSERT INTO user_identities(id, user_id, provider, tenant_key, external_subject)
		VALUES (?, ?, 'feishu', ?, ?)`, uuid.New(), owner.ID, "tenant-"+suffix, "ou-"+suffix).Error; err != nil {
		t.Fatal(err)
	}
	if err := adminDB.Exec(`INSERT INTO idempotency_keys(id, user_id, key, request_hash, expires_at)
		VALUES (?, ?, ?, decode(repeat('ab', 32), 'hex'), ?)`,
		uuid.New(), owner.ID, "account-"+suffix, time.Now().UTC().Add(time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	// 一次性重置令牌属于账号凭据数据，账号删除必须清理。
	if err := adminDB.Exec(`INSERT INTO password_reset_tokens(id, user_id, token_hash, expires_at)
		VALUES (?, ?, decode(repeat('cd', 32), 'hex'), ?)`,
		uuid.New(), owner.ID, time.Now().UTC().Add(15*time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	if err := adminDB.Exec(`INSERT INTO usage_counters(scope, scope_id, window_start, input_tokens, output_tokens)
		VALUES ('user', ?, ?, 10, 20)`, owner.ID, time.Now().UTC().Truncate(time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	// 飞书原始事件载荷可能包含正文，账号删除必须按发言人清理。
	if err := adminDB.Exec(`INSERT INTO feishu_event_receipts
		(id, event_id, event_type, tenant_key, payload_redacted, status, user_id)
		VALUES (?, ?, 'im.message.receive_v1', ?, '{"body":"账号事件"}', 'processed', ?)`,
		uuid.New(), "account-event-"+suffix, "tenant-"+suffix, owner.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := adminDB.Exec(`INSERT INTO feishu_event_receipts
		(id, event_id, event_type, tenant_key, payload_redacted, status, user_id)
		VALUES (?, ?, 'im.message.receive_v1', ?, '{"body":"其他用户事件"}', 'processed', ?)`,
		uuid.New(), "account-other-event-"+suffix, "tenant-"+suffix, other.ID).Error; err != nil {
		t.Fatal(err)
	}
	// 审计与删除请求必须跨账号删除保留；审计只保存假名引用。
	recorder, err := audit.NewRecorder(authDB, []byte("01234567890123456789012345678901"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.Record(ctx, audit.Event{
		ActorType: "user", ActorID: owner.ID.String(), OnBehalfUserID: owner.ID.String(),
		Action: "account.export", ResourceType: "account", ResourceID: sessionID.String(),
		Outcome: "success", Source: "integration-test",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := deletions.Create(ctx, biz.DeletionRequest{
		ID: uuid.New(), UserID: owner.ID, ResourceType: biz.DeletionResourceAccount, ResourceID: owner.ID.String(),
		RequestedSource: "integration-test", MaxAttempts: 5, AvailableAt: time.Now().UTC(), CreatedAt: time.Now().UTC(),
	}, testOutboxEvent(owner.ID, "account.deletion.requested")); err != nil {
		t.Fatal(err)
	}

	// 另一用户数据用于证明账号删除不会越界。
	otherSession := createMemoryIntegrationSession(t, ctx, sessions, other.ID, "account-other-"+suffix)
	appendMemoryIntegrationMessages(t, ctx, sessions, other.ID, otherSession, 1)

	var credentialVersion int64
	if err := adminDB.Raw("SELECT credential_version FROM users WHERE id = ?", owner.ID).Scan(&credentialVersion).Error; err != nil {
		t.Fatal(err)
	}

	executor := NewDeletionExecutor(appDB, hot)
	if err := executor.DeleteAccount(ctx, owner.ID); err != nil {
		t.Fatal(err)
	}

	// 全部用户级业务表必须清空。
	checks := []struct {
		name  string
		query string
		args  []any
	}{
		{"agents", "SELECT count(*) FROM agents WHERE user_id = ?", []any{owner.ID}},
		{"sessions", "SELECT count(*) FROM sessions WHERE user_id = ?", []any{owner.ID}},
		{"messages", "SELECT count(*) FROM messages WHERE user_id = ?", []any{owner.ID}},
		{"summaries", "SELECT count(*) FROM session_summaries WHERE user_id = ?", []any{owner.ID}},
		{"facts", "SELECT count(*) FROM memory_facts WHERE user_id = ?", []any{owner.ID}},
		{"vectors", "SELECT count(*) FROM memory_vectors WHERE user_id = ?", []any{owner.ID}},
		{"runs", "SELECT count(*) FROM agent_runs WHERE user_id = ?", []any{owner.ID}},
		{"jobs", "SELECT count(*) FROM jobs WHERE user_id = ?", []any{owner.ID}},
		{"bindings", "SELECT count(*) FROM external_conversation_bindings WHERE user_id = ?", []any{owner.ID}},
		{"refresh_tokens", "SELECT count(*) FROM refresh_tokens WHERE user_id = ?", []any{owner.ID}},
		{"user_identities", "SELECT count(*) FROM user_identities WHERE user_id = ?", []any{owner.ID}},
		{"password_reset_tokens", "SELECT count(*) FROM password_reset_tokens WHERE user_id = ?", []any{owner.ID}},
		{"feishu_event_receipts", "SELECT count(*) FROM feishu_event_receipts WHERE user_id = ?", []any{owner.ID}},
		{"idempotency_keys", "SELECT count(*) FROM idempotency_keys WHERE user_id = ?", []any{owner.ID}},
		{"usage_counters", "SELECT count(*) FROM usage_counters WHERE scope = 'user' AND scope_id = ?", []any{owner.ID}},
	}
	for _, check := range checks {
		if count := countDeletionRows(t, ctx, adminDB, check.query, check.args...); count != 0 {
			t.Fatalf("%s remains after account deletion: %d", check.name, count)
		}
	}

	// 用户行匿名化保留，凭据版本递增使全部 JWT 失效。
	var status, username, usernameNormalized, passwordHash string
	var versionAfter int64
	if err := adminDB.Raw(`SELECT status, username, username_normalized, password_hash, credential_version
		FROM users WHERE id = ?`, owner.ID).Row().
		Scan(&status, &username, &usernameNormalized, &passwordHash, &versionAfter); err != nil {
		t.Fatal(err)
	}
	anonymized := "deleted_" + strings.ReplaceAll(owner.ID.String(), "-", "")
	if status != string(biz.UserStatusDeleted) || username != anonymized || usernameNormalized != anonymized ||
		passwordHash != "" || versionAfter != credentialVersion+1 {
		t.Fatalf("user row was not anonymized: status=%q username=%q normalized=%q credential=%d->%d",
			status, username, usernameNormalized, credentialVersion, versionAfter)
	}

	// 审计、删除请求与 Outbox 必须保留，且审计仍可按资源追溯。
	if count := countDeletionRows(t, ctx, adminDB, "SELECT count(*) FROM audit_logs WHERE resource_id = ?", sessionID.String()); count != 1 {
		t.Fatalf("audit log was not preserved: %d", count)
	}
	if count := countDeletionRows(t, ctx, adminDB, "SELECT count(*) FROM deletion_requests WHERE user_id = ?", owner.ID); count != 1 {
		t.Fatalf("deletion request was not preserved: %d", count)
	}
	if count := countDeletionRows(t, ctx, adminDB, "SELECT count(*) FROM outbox_events WHERE user_id = ?", owner.ID); count < 1 {
		t.Fatalf("outbox events were not preserved: %d", count)
	}

	// 另一用户数据不受影响。
	if count := countDeletionRows(t, ctx, adminDB, "SELECT count(*) FROM sessions WHERE id = ? AND user_id = ?", otherSession, other.ID); count != 1 {
		t.Fatalf("other user session was affected: %d", count)
	}
	if count := countDeletionRows(t, ctx, adminDB, "SELECT count(*) FROM messages WHERE session_id = ?", otherSession); count != 1 {
		t.Fatalf("other user messages were affected: %d", count)
	}
	if count := countDeletionRows(t, ctx, adminDB, "SELECT count(*) FROM users WHERE id = ? AND status = 'active'", other.ID); count != 1 {
		t.Fatalf("other user account was affected: %d", count)
	}
	if count := countDeletionRows(t, ctx, adminDB, "SELECT count(*) FROM feishu_event_receipts WHERE user_id = ?", other.ID); count != 1 {
		t.Fatalf("other user feishu receipt was affected: %d", count)
	}
	if _, hit, err := hot.Load(ctx, owner.ID, sessionID); err != nil || hit {
		t.Fatalf("account deletion did not clear hot cache: hit=%v err=%v", hit, err)
	}
}
