//go:build integration

package data

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// feishuIntegrationDBs 打开飞书集成测试所需的管理、应用与认证连接池。
// 管理连接用于准备测试数据与清理；应用/认证连接分别验证 RLS 作用域与身份映射路径。
func feishuIntegrationDBs(t *testing.T) (*gorm.DB, *gorm.DB, *gorm.DB) {
	t.Helper()
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	return adminDB, appDB, authDB
}

// openFeishuPool 以指定连接数打开连接池，供并发写入测试真实触发唯一约束竞争。
func openFeishuPool(t *testing.T, dsn string, maxOpen int) *gorm.DB {
	t.Helper()
	db, err := OpenPostgres(dsn, maxOpen)
	if err != nil {
		t.Fatalf("open postgres pool: %v", err)
	}
	t.Cleanup(func() {
		sqlDB, _ := db.DB()
		_ = sqlDB.Close()
	})
	return db
}

// createFeishuTestUser 通过认证身份解析创建专用飞书用户，并在测试结束时级联清理。
func createFeishuTestUser(t *testing.T, ctx context.Context, adminDB, authDB *gorm.DB, tenantKey, openID string) uuid.UUID {
	t.Helper()
	identity, err := NewFeishuIdentityRepository(authDB).Resolve(ctx, tenantKey, openID)
	if err != nil {
		t.Fatalf("resolve feishu identity: %v", err)
	}
	if identity.UserID == uuid.Nil {
		t.Fatal("resolve feishu identity returned empty user id")
	}
	t.Cleanup(func() {
		// 删除用户会级联清理身份映射、会话、消息、绑定与外发记录。
		_ = adminDB.Exec("DELETE FROM users WHERE id = ?", identity.UserID).Error
	})
	return identity.UserID
}

// createFeishuTestRun 直接插入一条 Agent Run，满足外发记录对 run_id 的外键约束。
func createFeishuTestRun(t *testing.T, adminDB *gorm.DB, userID, sessionID uuid.UUID) uuid.UUID {
	t.Helper()
	runID := uuid.New()
	err := adminDB.Exec(
		`INSERT INTO agent_runs (id, session_id, user_id, mode, status) VALUES (?, ?, ?, 'react', 'queued')`,
		runID, sessionID, userID,
	).Error
	if err != nil {
		t.Fatalf("create agent run: %v", err)
	}
	return runID
}

// TestFeishuEventRepositoryLifecycle 覆盖事件回执幂等写入、状态流转、错误截断与非法入参。
func TestFeishuEventRepositoryLifecycle(t *testing.T) {
	adminDB, appDB, _ := feishuIntegrationDBs(t)
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	eventID := "feishu_it_event_" + suffix
	repository := NewFeishuEventRepository(appDB)
	t.Cleanup(func() {
		_ = adminDB.Exec("DELETE FROM feishu_event_receipts WHERE event_id = ?", eventID).Error
	})

	record := biz.FeishuEventRecord{
		EventID: eventID, EventType: "im.message.receive_v1", TenantKey: "tenant_" + suffix,
		AppID: "app_" + suffix, Payload: map[string]any{"type": "event_callback"},
	}
	inserted, err := repository.RecordEvent(ctx, record)
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	if !inserted {
		t.Fatal("first record should be inserted")
	}
	// 同一 event_id 重复投递必须幂等返回 false，避免重复消费。
	inserted, err = repository.RecordEvent(ctx, record)
	if err != nil {
		t.Fatalf("record duplicate event: %v", err)
	}
	if inserted {
		t.Fatal("duplicate record should not be inserted")
	}
	stored, err := repository.GetEvent(ctx, eventID)
	if err != nil {
		t.Fatalf("get event: %v", err)
	}
	if stored.Status != biz.FeishuEventReceived || stored.EventType != record.EventType {
		t.Fatalf("stored event = %#v", stored)
	}
	if stored.TenantKey != record.TenantKey || stored.AppID != record.AppID {
		t.Fatalf("stored event tenant/app = %q/%q", stored.TenantKey, stored.AppID)
	}
	if stored.Payload["type"] != "event_callback" {
		t.Fatalf("stored payload = %#v", stored.Payload)
	}

	// 身份解析后回填 user_id，账号删除才能按用户清理原始事件载荷。
	userID := uuid.New()
	if err := adminDB.Exec(`INSERT INTO users(id, username, username_normalized, password_hash) VALUES (?, ?, ?, 'hash')`,
		userID, "feishu_event_user_"+suffix, "feishu_event_user_"+suffix).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() { _ = adminDB.Exec("DELETE FROM users WHERE id = ?", userID).Error })
	if err := repository.BindUser(ctx, eventID, userID); err != nil {
		t.Fatalf("bind event user: %v", err)
	}
	stored, err = repository.GetEvent(ctx, eventID)
	if err != nil || stored.UserID == nil || *stored.UserID != userID {
		t.Fatalf("bound event user = %#v err=%v", stored.UserID, err)
	}
	if err := repository.BindUser(ctx, "", userID); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("bind empty event error = %v", err)
	}

	// processing 状态必须递增 attempts，并把超长错误截断为 1024 字节。
	now := time.Now().UTC()
	if err := repository.MarkEventStatus(ctx, eventID, biz.FeishuEventProcessing, strings.Repeat("x", 1100), now); err != nil {
		t.Fatalf("mark processing: %v", err)
	}
	stored, err = repository.GetEvent(ctx, eventID)
	if err != nil {
		t.Fatalf("get processing event: %v", err)
	}
	if stored.Status != biz.FeishuEventProcessing || stored.Attempts != 1 {
		t.Fatalf("processing event = %#v", stored)
	}
	if len(stored.LastError) != 1024 {
		t.Fatalf("last error length = %d, want 1024", len(stored.LastError))
	}
	// processed 状态记录完成时间，但不改变消费次数。
	if err := repository.MarkEventStatus(ctx, eventID, biz.FeishuEventProcessed, "", now.Add(time.Second)); err != nil {
		t.Fatalf("mark processed: %v", err)
	}
	stored, err = repository.GetEvent(ctx, eventID)
	if err != nil {
		t.Fatalf("get processed event: %v", err)
	}
	if stored.Status != biz.FeishuEventProcessed || stored.Attempts != 1 {
		t.Fatalf("processed event = %#v", stored)
	}
	// failed 状态允许在预算内重试，同样不改变消费次数；再次 processing 才继续计数。
	if err := repository.MarkEventStatus(ctx, eventID, biz.FeishuEventFailed, "retry", now.Add(2*time.Second)); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	if err := repository.MarkEventStatus(ctx, eventID, biz.FeishuEventProcessing, "", now.Add(3*time.Second)); err != nil {
		t.Fatalf("mark processing again: %v", err)
	}
	stored, err = repository.GetEvent(ctx, eventID)
	if err != nil {
		t.Fatalf("get retried event: %v", err)
	}
	if stored.Attempts != 2 || stored.Status != biz.FeishuEventProcessing {
		t.Fatalf("retried event = %#v", stored)
	}

	// 缺少事件 ID 或类型属于非法配置，必须在写入前拒绝。
	if _, err := repository.RecordEvent(ctx, biz.FeishuEventRecord{EventType: "im.message.receive_v1"}); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("empty event id error = %v", err)
	}
	if _, err := repository.RecordEvent(ctx, biz.FeishuEventRecord{EventID: "missing_type_" + suffix}); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("empty event type error = %v", err)
	}
}

// TestFeishuIdentityRepositoryResolve 覆盖身份复用、并发创建、禁用用户拒绝与非法入参。
func TestFeishuIdentityRepositoryResolve(t *testing.T) {
	adminDB, _, _ := feishuIntegrationDBs(t)
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	tenantKey := "feishu_it_tenant_" + suffix
	openID := "ou_" + suffix

	// 认证连接池提高并发上限，确保多个 goroutine 能同时发起首次身份创建。
	concurrentAuth := openFeishuPool(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"), 8)
	repository := NewFeishuIdentityRepository(concurrentAuth)
	first, err := repository.Resolve(ctx, tenantKey, openID)
	if err != nil {
		t.Fatalf("resolve identity: %v", err)
	}
	if first.UserID == uuid.Nil || !first.Created {
		t.Fatalf("first resolve = %#v", first)
	}
	t.Cleanup(func() {
		_ = adminDB.Exec("DELETE FROM users WHERE id = ?", first.UserID).Error
	})
	// 已存在的映射必须复用同一用户，不重复创建账户。
	second, err := repository.Resolve(ctx, tenantKey, openID)
	if err != nil {
		t.Fatalf("resolve existing identity: %v", err)
	}
	if second.UserID != first.UserID || second.Created {
		t.Fatalf("second resolve = %#v, want user %s", second.UserID, first.UserID)
	}

	// 并发首次投递同一 open_id 时只允许一个用户生效，其余请求回读既有映射。
	const runners = 6
	var wg sync.WaitGroup
	ids := make([]uuid.UUID, runners)
	errs := make([]error, runners)
	for i := 0; i < runners; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			identity, resolveErr := repository.Resolve(ctx, tenantKey, openID)
			ids[index], errs[index] = identity.UserID, resolveErr
		}(i)
	}
	wg.Wait()
	for i, resolveErr := range errs {
		if resolveErr != nil {
			t.Fatalf("concurrent resolve %d: %v", i, resolveErr)
		}
		if ids[i] != first.UserID {
			t.Fatalf("concurrent resolve %d user = %s, want %s", i, ids[i], first.UserID)
		}
	}

	// 不同 open_id 必须映射到不同用户，禁止按昵称等可伪造字段合并。
	other, err := repository.Resolve(ctx, tenantKey, openID+"_other")
	if err != nil {
		t.Fatalf("resolve other identity: %v", err)
	}
	if other.UserID == first.UserID {
		t.Fatal("different open_id resolved to the same user")
	}
	t.Cleanup(func() {
		_ = adminDB.Exec("DELETE FROM users WHERE id = ?", other.UserID).Error
	})

	// 用户被禁用后，身份解析必须拒绝进入系统。
	if err := adminDB.Exec("UPDATE users SET status = 'disabled' WHERE id = ?", first.UserID).Error; err != nil {
		t.Fatalf("disable user: %v", err)
	}
	if _, err := repository.Resolve(ctx, tenantKey, openID); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("disabled user resolve error = %v", err)
	}
	if _, err := repository.Resolve(ctx, "", openID); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("empty tenant resolve error = %v", err)
	}
	if _, err := repository.Resolve(ctx, tenantKey, ""); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("empty open id resolve error = %v", err)
	}
}

// TestFeishuConversationEnsureIdempotent 覆盖话题绑定创建、并发唯一冲突回读与系统作用域查找。
func TestFeishuConversationEnsureIdempotent(t *testing.T) {
	adminDB, _, authDB := feishuIntegrationDBs(t)
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	tenantKey := "feishu_it_tenant_" + suffix
	userID := createFeishuTestUser(t, ctx, adminDB, authDB, tenantKey, "ou_conv_"+suffix)

	// 并发连接池保证多个事务同时写入同一话题，触发唯一索引竞争。
	appPool := openFeishuPool(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"), 8)
	repository := NewFeishuConversationRepository(appPool)
	chatID := "chat_" + suffix
	threadID := "thread_" + suffix

	const runners = 4
	var wg sync.WaitGroup
	bindings := make([]biz.FeishuConversationBinding, runners)
	sessions := make([]biz.Session, runners)
	errs := make([]error, runners)
	for i := 0; i < runners; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			sessionID := uuid.New()
			binding := biz.FeishuConversationBinding{
				ID: uuid.New(), UserID: userID, Provider: "feishu", TenantKey: tenantKey,
				ChatID: chatID, ThreadID: threadID, SessionID: sessionID, Status: biz.FeishuBindingActive,
			}
			session := biz.Session{
				ID: sessionID, UserID: userID, Title: "feishu topic", Source: biz.SessionSourceFeishu,
				MemoryPolicy: biz.SessionMemoryGroup, Status: biz.SessionActive, Version: 1,
			}
			bindings[index], sessions[index], errs[index] = repository.EnsureConversation(ctx, binding, session, testOutboxEvent(userID, "test.feishu.binding."+uuid.NewString()))
		}(i)
	}
	wg.Wait()
	for i, ensureErr := range errs {
		if ensureErr != nil {
			t.Fatalf("concurrent ensure %d: %v", i, ensureErr)
		}
	}
	// 所有并发调用必须收敛到同一话题与同一会话，禁止生成群级重复会话。
	winner := bindings[0]
	for i, binding := range bindings {
		if binding.ID != winner.ID || binding.SessionID != winner.SessionID {
			t.Fatalf("ensure %d binding = %#v, winner = %#v", i, binding, winner)
		}
		if sessions[i].ID != winner.SessionID || sessions[i].UserID != userID {
			t.Fatalf("ensure %d session = %#v", i, sessions[i])
		}
		// 群聊话题必须持久化为 group 记忆策略，避免升级后回退为私有记忆。
		if sessions[i].MemoryPolicy != biz.SessionMemoryGroup {
			t.Fatalf("ensure %d memory policy = %q", i, sessions[i].MemoryPolicy)
		}
	}

	// 系统作用域按 tenant/chat/thread 查找绑定，供群内不同成员复用同一话题。
	found, ok, err := repository.FindConversation(ctx, tenantKey, chatID, threadID, "")
	if err != nil {
		t.Fatalf("find conversation: %v", err)
	}
	if !ok || found.ID != winner.ID || found.SessionID != winner.SessionID {
		t.Fatalf("found conversation = %#v, ok=%t", found, ok)
	}
	// 不存在的话题必须返回 false，而不是伪造空绑定。
	if _, ok, err := repository.FindConversation(ctx, tenantKey, chatID+"_missing", threadID, ""); err != nil || ok {
		t.Fatalf("missing conversation ok=%t err=%v", ok, err)
	}
	// 群聊话题会话必须被识别为共享会话，禁止后续开启私有记忆。
	if shared, err := repository.IsSharedSession(ctx, userID, winner.SessionID); err != nil || !shared {
		t.Fatalf("group session shared=%t err=%v", shared, err)
	}
	// 未知会话不得被误判为共享会话。
	if shared, err := repository.IsSharedSession(ctx, userID, uuid.New()); err != nil || shared {
		t.Fatalf("unknown session shared=%t err=%v", shared, err)
	}
}

// TestSessionMemoryPolicyPersistence 覆盖默认私有策略、群聊策略更新与版本冲突。
func TestSessionMemoryPolicyPersistence(t *testing.T) {
	adminDB, appDB, authDB := feishuIntegrationDBs(t)
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	tenantKey := "feishu_it_tenant_" + suffix
	userID := createFeishuTestUser(t, ctx, adminDB, authDB, tenantKey, "ou_policy_"+suffix)
	repository := NewSessionRepository(appDB)

	session := biz.Session{
		ID: uuid.New(), UserID: userID, Title: "记忆策略", Source: biz.SessionSourceWeb,
		Status: biz.SessionActive, Version: 1,
	}
	created, err := repository.CreateSession(ctx, session, testOutboxEvent(userID, "test.session.created"))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	// 空策略必须落库为 private，保持与升级前一致的默认行为。
	if created.MemoryPolicy != biz.SessionMemoryPrivate {
		t.Fatalf("default memory policy = %q", created.MemoryPolicy)
	}
	// 普通 Web 会话没有群聊绑定，不得被识别为共享会话。
	if shared, err := NewFeishuConversationRepository(appDB).IsSharedSession(ctx, userID, created.ID); err != nil || shared {
		t.Fatalf("web session shared=%t err=%v", shared, err)
	}

	created.MemoryPolicy = biz.SessionMemoryDisabled
	created.Version++
	updated, err := repository.UpdateSessionMemoryPolicy(ctx, created, testOutboxEvent(userID, "test.session.memory_policy_updated"))
	if err != nil {
		t.Fatalf("update memory policy: %v", err)
	}
	if updated.MemoryPolicy != biz.SessionMemoryDisabled || updated.Version != 2 {
		t.Fatalf("updated session = %#v", updated)
	}

	// 使用旧版本号再次更新必须失败，避免并发写覆盖。
	stale := updated
	stale.Version = 2
	stale.MemoryPolicy = biz.SessionMemoryGroup
	if _, err := repository.UpdateSessionMemoryPolicy(ctx, stale, testOutboxEvent(userID, "test.session.memory_policy_stale")); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("stale policy update error = %v", err)
	}
}

// TestFeishuInboundMessageIdempotency 覆盖外部消息去重、活跃会话校验与跨用户隔离。
func TestFeishuInboundMessageIdempotency(t *testing.T) {
	adminDB, appDB, authDB := feishuIntegrationDBs(t)
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	tenantKey := "feishu_it_tenant_" + suffix
	userID := createFeishuTestUser(t, ctx, adminDB, authDB, tenantKey, "ou_msg_"+suffix)

	sessionID := uuid.New()
	binding := biz.FeishuConversationBinding{
		ID: uuid.New(), UserID: userID, Provider: "feishu", TenantKey: tenantKey,
		ChatID: "chat_msg_" + suffix, ThreadID: "thread_msg_" + suffix, SessionID: sessionID, Status: biz.FeishuBindingActive,
	}
	session := biz.Session{
		ID: sessionID, UserID: userID, Title: "feishu message", Source: biz.SessionSourceFeishu,
		Status: biz.SessionActive, Version: 1,
	}
	if _, _, err := NewFeishuConversationRepository(appDB).EnsureConversation(ctx, binding, session, testOutboxEvent(userID, "test.feishu.binding."+uuid.NewString())); err != nil {
		t.Fatalf("ensure conversation: %v", err)
	}

	repository := NewFeishuMessageRepository(appDB)
	externalID := "om_" + suffix
	message := biz.Message{
		ID: uuid.New(), SessionID: sessionID, UserID: userID, Content: "第一条飞书消息",
		ExternalMessageID: externalID, SpeakerOpenID: "ou_msg_" + suffix,
	}
	created, isNew, err := repository.AppendInbound(ctx, message, testOutboxEvent(userID, "test.feishu.message."+uuid.NewString()))
	if err != nil {
		t.Fatalf("append inbound: %v", err)
	}
	if !isNew || created.Content != message.Content || created.ExternalMessageID != externalID {
		t.Fatalf("created message = %#v, new=%t", created, isNew)
	}
	// 飞书重推同一 external_message_id 时返回既有消息，不重复写入。
	duplicate := message
	duplicate.ID = uuid.New()
	duplicate.Content = "重复投递"
	existing, isNew, err := repository.AppendInbound(ctx, duplicate, testOutboxEvent(userID, "test.feishu.message."+uuid.NewString()))
	if err != nil {
		t.Fatalf("append duplicate inbound: %v", err)
	}
	if isNew || existing.ID != created.ID || existing.Content != message.Content {
		t.Fatalf("duplicate message = %#v, new=%t", existing, isNew)
	}
	// 缺少外部消息 ID 无法幂等，必须在写入前拒绝。
	if _, _, err := repository.AppendInbound(ctx, biz.Message{SessionID: sessionID, UserID: userID, Content: "missing id"}, testOutboxEvent(userID, "test.feishu.message."+uuid.NewString())); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("missing external id error = %v", err)
	}
	// 跨用户写入必须因会话归属不匹配而失败。
	otherUserID := uuid.New()
	if _, _, err := repository.AppendInbound(ctx, biz.Message{ID: uuid.New(), SessionID: sessionID, UserID: otherUserID, Content: "steal", ExternalMessageID: "om_steal_" + suffix}, testOutboxEvent(otherUserID, "test.feishu.message."+uuid.NewString())); !errors.Is(err, biz.ErrResourceNotFound) {
		t.Fatalf("cross-user append error = %v", err)
	}
	// 会话归档后禁止继续追加消息。
	if err := adminDB.Exec("UPDATE sessions SET status = 'archived' WHERE id = ?", sessionID).Error; err != nil {
		t.Fatalf("archive session: %v", err)
	}
	if _, _, err := repository.AppendInbound(ctx, biz.Message{ID: uuid.New(), SessionID: sessionID, UserID: userID, Content: "late", ExternalMessageID: "om_late_" + suffix}, testOutboxEvent(userID, "test.feishu.message."+uuid.NewString())); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("archived session append error = %v", err)
	}
}

// TestFeishuInboundOutOfOrderAndConcurrentSpeakers 覆盖乱序事件与同话题多发言人的并发写入。
// 关键断言：消息按飞书发生时间落库、发言人标识保留、会话 last_message_at 单调取最大值。
func TestFeishuInboundOutOfOrderAndConcurrentSpeakers(t *testing.T) {
	adminDB, appDB, authDB := feishuIntegrationDBs(t)
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	tenantKey := "feishu_it_tenant_" + suffix
	userID := createFeishuTestUser(t, ctx, adminDB, authDB, tenantKey, "ou_order_"+suffix)

	sessionID := uuid.New()
	binding := biz.FeishuConversationBinding{
		ID: uuid.New(), UserID: userID, Provider: "feishu", TenantKey: tenantKey,
		ChatID: "chat_order_" + suffix, ThreadID: "thread_order_" + suffix, SessionID: sessionID, Status: biz.FeishuBindingActive,
	}
	session := biz.Session{
		ID: sessionID, UserID: userID, Title: "feishu order", Source: biz.SessionSourceFeishu,
		MemoryPolicy: biz.SessionMemoryGroup, Status: biz.SessionActive, Version: 1,
	}
	if _, _, err := NewFeishuConversationRepository(appDB).EnsureConversation(ctx, binding, session, testOutboxEvent(userID, "test.feishu.binding."+uuid.NewString())); err != nil {
		t.Fatalf("ensure conversation: %v", err)
	}

	repository := NewFeishuMessageRepository(appDB)
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	// 故意让发生时间与写入顺序不一致：第二条先于第一条写入，模拟事件乱序到达。
	inbound := []biz.Message{
		{ID: uuid.New(), SessionID: sessionID, UserID: userID, Content: "第一条", ExternalMessageID: "om_order_1_" + suffix, SpeakerOpenID: "ou_a_" + suffix, CreatedAt: base},
		{ID: uuid.New(), SessionID: sessionID, UserID: userID, Content: "第三条", ExternalMessageID: "om_order_3_" + suffix, SpeakerOpenID: "ou_b_" + suffix, CreatedAt: base.Add(2 * time.Second)},
		{ID: uuid.New(), SessionID: sessionID, UserID: userID, Content: "第二条", ExternalMessageID: "om_order_2_" + suffix, SpeakerOpenID: "ou_c_" + suffix, CreatedAt: base.Add(time.Second)},
	}
	var wg sync.WaitGroup
	results := make([]error, len(inbound))
	for index := range inbound {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			created, isNew, err := repository.AppendInbound(ctx, inbound[index], testOutboxEvent(userID, "test.feishu.message."+uuid.NewString()))
			_ = created
			if err == nil && !isNew {
				err = errors.New("concurrent inbound message must be newly created")
			}
			results[index] = err
		}(index)
	}
	wg.Wait()
	for index, err := range results {
		if err != nil {
			t.Fatalf("append %s: %v", inbound[index].ExternalMessageID, err)
		}
	}

	// 每条消息必须保留飞书发生时间与发言人标识，不能退化为并发写入时的本地时间。
	for _, message := range inbound {
		var storedAt time.Time
		if err := adminDB.Table("messages").Where("external_message_id = ?", message.ExternalMessageID).Pluck("created_at", &storedAt).Error; err != nil {
			t.Fatalf("read %s created_at: %v", message.ExternalMessageID, err)
		}
		if !storedAt.Equal(message.CreatedAt) {
			t.Fatalf("%s created_at = %v, want %v", message.ExternalMessageID, storedAt, message.CreatedAt)
		}
		var speaker string
		if err := adminDB.Table("messages").Where("external_message_id = ?", message.ExternalMessageID).Pluck("speaker_open_id", &speaker).Error; err != nil {
			t.Fatalf("read %s speaker: %v", message.ExternalMessageID, err)
		}
		if speaker != message.SpeakerOpenID {
			t.Fatalf("%s speaker = %q, want %q", message.ExternalMessageID, speaker, message.SpeakerOpenID)
		}
	}

	// 旧事件晚到时不得把会话最近消息时间回退到更早时间。
	var lastMessageAt time.Time
	if err := adminDB.Table("sessions").Where("id = ?", sessionID).Pluck("last_message_at", &lastMessageAt).Error; err != nil {
		t.Fatalf("read session last_message_at: %v", err)
	}
	if !lastMessageAt.Equal(base.Add(2 * time.Second)) {
		t.Fatalf("last_message_at = %v, want %v", lastMessageAt, base.Add(2*time.Second))
	}
}

// TestFeishuOutboundIdempotencyAndClaim 覆盖外发去重、领取、延后回退与完成流转。
func TestFeishuOutboundIdempotencyAndClaim(t *testing.T) {
	adminDB, appDB, authDB := feishuIntegrationDBs(t)
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	tenantKey := "feishu_it_tenant_" + suffix
	userID := createFeishuTestUser(t, ctx, adminDB, authDB, tenantKey, "ou_out_"+suffix)

	sessionID := uuid.New()
	binding := biz.FeishuConversationBinding{
		ID: uuid.New(), UserID: userID, Provider: "feishu", TenantKey: tenantKey,
		ChatID: "chat_out_" + suffix, ThreadID: "thread_out_" + suffix, SessionID: sessionID, Status: biz.FeishuBindingActive,
	}
	session := biz.Session{ID: sessionID, UserID: userID, Title: "feishu outbound", Source: biz.SessionSourceFeishu, Status: biz.SessionActive, Version: 1}
	if _, _, err := NewFeishuConversationRepository(appDB).EnsureConversation(ctx, binding, session, testOutboxEvent(userID, "test.feishu.binding."+uuid.NewString())); err != nil {
		t.Fatalf("ensure conversation: %v", err)
	}
	runID := createFeishuTestRun(t, adminDB, userID, sessionID)

	// 隔离历史残余记录，确保本次领取只会命中下面创建的两条外发记录。
	if err := adminDB.Exec("UPDATE feishu_outbound_messages SET status = 'retrying', available_at = now() + interval '1 day' WHERE status IN ('pending', 'retrying')").Error; err != nil {
		t.Fatalf("stale pending rows: %v", err)
	}
	if err := adminDB.Exec("UPDATE feishu_outbound_messages SET status = 'retrying', available_at = now() + interval '1 day', lease_expires_at = NULL WHERE status = 'sending'").Error; err != nil {
		t.Fatalf("stale sending rows: %v", err)
	}

	repository := NewFeishuOutboundRepository(appDB)
	firstID := uuid.New()
	secondID := uuid.New()
	for _, outbound := range []biz.FeishuOutbound{
		{ID: firstID, UserID: userID, SessionID: sessionID, RunID: runID, ChatID: binding.ChatID, ThreadID: binding.ThreadID, Kind: "card", DedupeKey: "feishu_it_out_" + suffix + "_1"},
		{ID: secondID, UserID: userID, SessionID: sessionID, RunID: runID, ChatID: binding.ChatID, ThreadID: binding.ThreadID, Kind: "text", DedupeKey: "feishu_it_out_" + suffix + "_2"},
	} {
		if err := repository.CreateOutbound(ctx, outbound); err != nil {
			t.Fatalf("create outbound: %v", err)
		}
		// 同一 dedupe_key 重复创建必须幂等成功，不产生第二条记录。
		if err := repository.CreateOutbound(ctx, outbound); err != nil {
			t.Fatalf("duplicate outbound: %v", err)
		}
	}
	var count int64
	if err := adminDB.Table("feishu_outbound_messages").Where("dedupe_key LIKE ?", "feishu_it_out_"+suffix+"%").Count(&count).Error; err != nil {
		t.Fatalf("count outbound: %v", err)
	}
	if count != 2 {
		t.Fatalf("outbound rows = %d, want 2", count)
	}
	if err := repository.CreateOutbound(ctx, biz.FeishuOutbound{UserID: userID, ChatID: binding.ChatID, RunID: runID}); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("invalid outbound error = %v", err)
	}
	if _, _, err := repository.ClaimOutbound(ctx, "", time.Minute, time.Now().UTC()); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("invalid claim error = %v", err)
	}

	now := time.Now().UTC()
	claimed, ok, err := repository.ClaimOutbound(ctx, "worker-it", time.Minute, now)
	if err != nil || !ok {
		t.Fatalf("claim first outbound ok=%t err=%v", ok, err)
	}
	if claimed.ID != firstID && claimed.ID != secondID {
		t.Fatalf("claimed unexpected outbound %s", claimed.ID)
	}
	if claimed.Status != "sending" || claimed.Attempts != 1 {
		t.Fatalf("claimed outbound = %#v", claimed)
	}
	second, ok, err := repository.ClaimOutbound(ctx, "worker-it", time.Minute, now)
	if err != nil || !ok {
		t.Fatalf("claim second outbound ok=%t err=%v", ok, err)
	}
	if second.ID == claimed.ID || second.Attempts != 1 {
		t.Fatalf("second claimed = %#v", second)
	}
	// 没有到期记录时领取返回 false。
	if _, ok, err := repository.ClaimOutbound(ctx, "worker-it", time.Minute, now); err != nil || ok {
		t.Fatalf("empty claim ok=%t err=%v", ok, err)
	}

	// 等待 Run 期间延后检查必须回退领取计数，避免轮询耗尽失败预算。
	retryAt := now.Add(time.Minute)
	if err := repository.DeferOutbound(ctx, claimed.ID, retryAt, now); err != nil {
		t.Fatalf("defer outbound: %v", err)
	}
	deferred := readFeishuOutbound(t, adminDB, claimed.ID)
	if deferred.Status != "pending" || deferred.Attempts != 0 {
		t.Fatalf("deferred outbound = %#v", deferred)
	}
	if _, ok, err := repository.ClaimOutbound(ctx, "worker-it", time.Minute, retryAt); err != nil || !ok {
		t.Fatalf("re-claim deferred ok=%t err=%v", ok, err)
	}
	// 保存飞书消息 ID 后标记完成。
	if err := repository.UpdateOutboundMessageID(ctx, claimed.ID, "om_reply_"+suffix, retryAt); err != nil {
		t.Fatalf("update outbound message id: %v", err)
	}
	// 卡片成功更新必须持久化更新时间与内容指纹，供跨副本节流与去重。
	fingerprint := "fp_" + suffix
	if err := repository.MarkOutboundCardUpdated(ctx, claimed.ID, fingerprint, retryAt); err != nil {
		t.Fatalf("mark outbound card updated: %v", err)
	}
	if marked := readFeishuOutbound(t, adminDB, claimed.ID); marked.CardFingerprint != fingerprint || marked.CardUpdatedAt == nil {
		t.Fatalf("marked outbound = %#v", marked)
	}
	if err := repository.MarkOutboundCardUpdated(ctx, claimed.ID, "", retryAt); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("empty fingerprint error = %v", err)
	}
	if err := repository.CompleteOutbound(ctx, claimed.ID, retryAt); err != nil {
		t.Fatalf("complete outbound: %v", err)
	}
	completed := readFeishuOutbound(t, adminDB, claimed.ID)
	if completed.Status != "completed" || completed.MessageID != "om_reply_"+suffix || completed.CompletedAt == nil {
		t.Fatalf("completed outbound = %#v", completed)
	}
}

// TestFeishuOutboundFailureAndLeaseRecovery 覆盖租约过期恢复、失败退避、错误截断与死信。
func TestFeishuOutboundFailureAndLeaseRecovery(t *testing.T) {
	adminDB, appDB, authDB := feishuIntegrationDBs(t)
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	tenantKey := "feishu_it_tenant_" + suffix
	userID := createFeishuTestUser(t, ctx, adminDB, authDB, tenantKey, "ou_fail_"+suffix)

	sessionID := uuid.New()
	binding := biz.FeishuConversationBinding{
		ID: uuid.New(), UserID: userID, Provider: "feishu", TenantKey: tenantKey,
		ChatID: "chat_fail_" + suffix, ThreadID: "thread_fail_" + suffix, SessionID: sessionID, Status: biz.FeishuBindingActive,
	}
	session := biz.Session{ID: sessionID, UserID: userID, Title: "feishu failure", Source: biz.SessionSourceFeishu, Status: biz.SessionActive, Version: 1}
	if _, _, err := NewFeishuConversationRepository(appDB).EnsureConversation(ctx, binding, session, testOutboxEvent(userID, "test.feishu.binding."+uuid.NewString())); err != nil {
		t.Fatalf("ensure conversation: %v", err)
	}
	runID := createFeishuTestRun(t, adminDB, userID, sessionID)

	if err := adminDB.Exec("UPDATE feishu_outbound_messages SET status = 'retrying', available_at = now() + interval '1 day' WHERE status IN ('pending', 'retrying')").Error; err != nil {
		t.Fatalf("stale pending rows: %v", err)
	}
	if err := adminDB.Exec("UPDATE feishu_outbound_messages SET status = 'retrying', available_at = now() + interval '1 day', lease_expires_at = NULL WHERE status = 'sending'").Error; err != nil {
		t.Fatalf("stale sending rows: %v", err)
	}

	repository := NewFeishuOutboundRepository(appDB)
	leaseID := uuid.New()
	deadID := uuid.New()
	for _, outbound := range []biz.FeishuOutbound{
		{ID: leaseID, UserID: userID, SessionID: sessionID, RunID: runID, ChatID: binding.ChatID, ThreadID: binding.ThreadID, Kind: "card", DedupeKey: "feishu_it_fail_" + suffix + "_lease"},
		{ID: deadID, UserID: userID, SessionID: sessionID, RunID: runID, ChatID: binding.ChatID, ThreadID: binding.ThreadID, Kind: "text", DedupeKey: "feishu_it_fail_" + suffix + "_dead"},
	} {
		if err := repository.CreateOutbound(ctx, outbound); err != nil {
			t.Fatalf("create outbound: %v", err)
		}
	}
	// 死信记录的预算设置为 1，领取后一次失败即进入 dead。
	if err := adminDB.Exec("UPDATE feishu_outbound_messages SET max_attempts = 1 WHERE id = ?", deadID).Error; err != nil {
		t.Fatalf("shrink dead budget: %v", err)
	}

	now := time.Now().UTC()
	claimed, ok, err := repository.ClaimOutbound(ctx, "worker-it", 10*time.Second, now)
	if err != nil || !ok {
		t.Fatalf("claim lease outbound ok=%t err=%v", ok, err)
	}
	if claimed.ID != leaseID && claimed.ID != deadID {
		t.Fatalf("claimed unexpected outbound %s", claimed.ID)
	}
	// 领取一条后立即把租约置为过期，模拟 Worker 崩溃后的恢复路径。
	if err := adminDB.Exec("UPDATE feishu_outbound_messages SET lease_expires_at = now() - interval '1 second' WHERE id = ?", claimed.ID).Error; err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	recovered, ok, err := repository.ClaimOutbound(ctx, "worker-recover", 10*time.Second, now)
	if err != nil || !ok {
		t.Fatalf("claim expired lease ok=%t err=%v", ok, err)
	}
	if recovered.ID != claimed.ID || recovered.Attempts != claimed.Attempts+1 {
		t.Fatalf("recovered outbound = %#v, first = %#v", recovered, claimed)
	}
	// 未超过预算的失败进入退避重试，并且错误文本截断为 1024 字节。
	if err := repository.FailOutbound(ctx, claimed.ID, strings.Repeat("y", 1100), now.Add(time.Minute), now); err != nil {
		t.Fatalf("fail outbound: %v", err)
	}
	retried := readFeishuOutbound(t, adminDB, claimed.ID)
	if retried.Status != "retrying" || len(retried.LastError) != 1024 || retried.CompletedAt != nil {
		t.Fatalf("retried outbound = %#v", retried)
	}

	// 目标死信记录领取后失败一次即超过预算，必须进入 dead 并记录完成时间。
	deadClaim, ok, err := repository.ClaimOutbound(ctx, "worker-it", 10*time.Second, now)
	if err != nil || !ok || deadClaim.ID != deadID {
		t.Fatalf("claim dead outbound = %#v, ok=%t err=%v", deadClaim, ok, err)
	}
	if deadClaim.Attempts != 1 || deadClaim.MaxAttempts != 1 {
		t.Fatalf("dead candidate = %#v", deadClaim)
	}
	if err := repository.FailOutbound(ctx, deadID, "permanent failure", now, now); err != nil {
		t.Fatalf("fail dead outbound: %v", err)
	}
	dead := readFeishuOutbound(t, adminDB, deadID)
	if dead.Status != "dead" || dead.CompletedAt == nil || dead.LastError != "permanent failure" {
		t.Fatalf("dead outbound = %#v", dead)
	}
}

// TestFeishuLatestAssistantMessageIsolation 覆盖最近助手消息读取与跨用户隔离。
func TestFeishuLatestAssistantMessageIsolation(t *testing.T) {
	adminDB, appDB, authDB := feishuIntegrationDBs(t)
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	tenantKey := "feishu_it_tenant_" + suffix
	userID := createFeishuTestUser(t, ctx, adminDB, authDB, tenantKey, "ou_latest_"+suffix)

	sessionID := uuid.New()
	binding := biz.FeishuConversationBinding{
		ID: uuid.New(), UserID: userID, Provider: "feishu", TenantKey: tenantKey,
		ChatID: "chat_latest_" + suffix, ThreadID: "thread_latest_" + suffix, SessionID: sessionID, Status: biz.FeishuBindingActive,
	}
	session := biz.Session{ID: sessionID, UserID: userID, Title: "feishu latest", Source: biz.SessionSourceFeishu, Status: biz.SessionActive, Version: 1}
	if _, _, err := NewFeishuConversationRepository(appDB).EnsureConversation(ctx, binding, session, testOutboxEvent(userID, "test.feishu.binding."+uuid.NewString())); err != nil {
		t.Fatalf("ensure conversation: %v", err)
	}

	sessionRepository := NewSessionRepository(appDB)
	for _, content := range []string{"较早的回复", "最新的回复"} {
		if _, err := sessionRepository.AppendMessage(ctx, biz.Message{
			ID: uuid.New(), SessionID: sessionID, UserID: userID, Role: biz.MessageAssistant, Content: content,
		}, testOutboxEvent(userID, "test.feishu.assistant."+uuid.NewString())); err != nil {
			t.Fatalf("append assistant message: %v", err)
		}
		// 确保两条消息的 created_at 可区分，避免同一时间戳导致排序不确定。
		time.Sleep(5 * time.Millisecond)
	}

	latest, err := NewFeishuOutboundRepository(appDB).LatestAssistantMessage(ctx, userID, sessionID)
	if err != nil {
		t.Fatalf("latest assistant message: %v", err)
	}
	if latest.Role != biz.MessageAssistant || latest.Content != "最新的回复" {
		t.Fatalf("latest assistant message = %#v", latest)
	}
	// 其他用户读取同一会话必须返回不存在，禁止跨用户召回回复内容。
	if _, err := NewFeishuOutboundRepository(appDB).LatestAssistantMessage(ctx, uuid.New(), sessionID); !errors.Is(err, biz.ErrResourceNotFound) {
		t.Fatalf("cross-user latest message error = %v", err)
	}
}

// TestFeishuConversationLifecycleAndPromotion 覆盖关闭、恢复、话题升级与解绑后重建。
// 关键场景：主时间线首条 @ 以根消息 ID 作为过渡话题键，飞书创建真实话题后升级绑定。
func TestFeishuConversationLifecycleAndPromotion(t *testing.T) {
	adminDB, appDB, authDB := feishuIntegrationDBs(t)
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	tenantKey := "feishu_it_tenant_" + suffix
	userID := createFeishuTestUser(t, ctx, adminDB, authDB, tenantKey, "ou_life_"+suffix)

	repository := NewFeishuConversationRepository(appDB)
	chatID := "chat_life_" + suffix
	rootMessageID := "om_root_" + suffix
	// 过渡别名：话题尚未创建时以根消息 ID 占位。
	legacyThreadID := rootMessageID
	realThreadID := "omt_life_" + suffix

	sessionID := uuid.New()
	binding := biz.FeishuConversationBinding{
		ID: uuid.New(), UserID: userID, Provider: "feishu", TenantKey: tenantKey,
		ChatID: chatID, ThreadID: legacyThreadID, RootMessageID: rootMessageID,
		SessionID: sessionID, Status: biz.FeishuBindingActive,
	}
	session := biz.Session{
		ID: sessionID, UserID: userID, Title: "feishu lifecycle", Source: biz.SessionSourceFeishu,
		MemoryPolicy: biz.SessionMemoryGroup, Status: biz.SessionActive, Version: 1,
	}
	if _, _, err := repository.EnsureConversation(ctx, binding, session, testOutboxEvent(userID, "test.feishu.lifecycle."+uuid.NewString())); err != nil {
		t.Fatalf("ensure conversation: %v", err)
	}

	// 关闭后绑定仍可被查到，Worker 依据状态忽略自动回复；话题键保持占用。
	released, err := repository.ReleaseConversation(ctx, userID, sessionID, biz.FeishuBindingClosed, testOutboxEvent(userID, "test.feishu.closed."+uuid.NewString()))
	if err != nil || released != 1 {
		t.Fatalf("close released=%d err=%v", released, err)
	}
	closed, found, err := repository.FindConversation(ctx, tenantKey, chatID, legacyThreadID, "")
	if err != nil || !found || closed.Status != biz.FeishuBindingClosed {
		t.Fatalf("closed binding found=%t binding=%#v err=%v", found, closed, err)
	}

	// 恢复后回到活跃状态，重复恢复幂等返回 0。
	restored, err := repository.RestoreConversation(ctx, userID, sessionID, testOutboxEvent(userID, "test.feishu.restored."+uuid.NewString()))
	if err != nil || restored != 1 {
		t.Fatalf("restore affected=%d err=%v", restored, err)
	}
	active, found, err := repository.FindConversation(ctx, tenantKey, chatID, legacyThreadID, "")
	if err != nil || !found || active.Status != biz.FeishuBindingActive {
		t.Fatalf("restored binding found=%t binding=%#v err=%v", found, active, err)
	}
	if restored, err := repository.RestoreConversation(ctx, userID, sessionID, testOutboxEvent(userID, "test.feishu.restored2."+uuid.NewString())); err != nil || restored != 0 {
		t.Fatalf("idempotent restore affected=%d err=%v", restored, err)
	}

	// 话题升级后按真实话题 ID 命中，旧过渡别名不再命中。
	promoted, err := repository.PromoteConversationThread(ctx, userID, sessionID, legacyThreadID, realThreadID)
	if err != nil || promoted != 1 {
		t.Fatalf("promote affected=%d err=%v", promoted, err)
	}
	threaded, found, err := repository.FindConversation(ctx, tenantKey, chatID, realThreadID, "")
	if err != nil || !found || threaded.SessionID != sessionID {
		t.Fatalf("promoted thread found=%t binding=%#v err=%v", found, threaded, err)
	}
	if _, found, err := repository.FindConversation(ctx, tenantKey, chatID, legacyThreadID, ""); err != nil || found {
		t.Fatalf("legacy thread still matched found=%t err=%v", found, err)
	}

	// 解绑后查找跳过该记录，同一话题允许重新建立全新会话。
	unbound, err := repository.ReleaseConversation(ctx, userID, sessionID, biz.FeishuBindingUnbound, testOutboxEvent(userID, "test.feishu.unbound."+uuid.NewString()))
	if err != nil || unbound != 1 {
		t.Fatalf("unbind released=%d err=%v", unbound, err)
	}
	if _, found, err := repository.FindConversation(ctx, tenantKey, chatID, realThreadID, ""); err != nil || found {
		t.Fatalf("unbound binding still matched found=%t err=%v", found, err)
	}
	newSessionID := uuid.New()
	newBinding := biz.FeishuConversationBinding{
		ID: uuid.New(), UserID: userID, Provider: "feishu", TenantKey: tenantKey,
		ChatID: chatID, ThreadID: realThreadID, SessionID: newSessionID, Status: biz.FeishuBindingActive,
	}
	newSession := biz.Session{
		ID: newSessionID, UserID: userID, Title: "feishu rebind", Source: biz.SessionSourceFeishu,
		MemoryPolicy: biz.SessionMemoryGroup, Status: biz.SessionActive, Version: 1,
	}
	if _, _, err := repository.EnsureConversation(ctx, newBinding, newSession, testOutboxEvent(userID, "test.feishu.rebind."+uuid.NewString())); err != nil {
		t.Fatalf("rebind conversation: %v", err)
	}
	rebound, found, err := repository.FindConversation(ctx, tenantKey, chatID, realThreadID, "")
	if err != nil || !found || rebound.SessionID != newSessionID {
		t.Fatalf("rebound conversation found=%t binding=%#v err=%v", found, rebound, err)
	}
}

// readFeishuOutbound 读取外发记录当前状态，用于断言异步状态流转结果。
func readFeishuOutbound(t *testing.T, adminDB *gorm.DB, id uuid.UUID) feishuOutboundModel {
	t.Helper()
	var record feishuOutboundModel
	if err := adminDB.Table("feishu_outbound_messages").Where("id = ?", id).First(&record).Error; err != nil {
		t.Fatalf("read outbound %s: %v", id, err)
	}
	return record
}
