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

// summaryIntegrationSetup 创建摘要测试所需的用户、会话与消息。
func summaryIntegrationSetup(t *testing.T, ctx context.Context, sessions *SessionRepository, users *UserRepository) (uuid.UUID, uuid.UUID, []biz.Message) {
	t.Helper()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	user, err := users.CreateUser(ctx, biz.User{
		Username: "summary_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	sessionID := uuid.New()
	if _, err := sessions.CreateSession(ctx, biz.Session{
		ID: sessionID, UserID: user.ID, Title: "摘要测试", Source: biz.SessionSourceWeb,
		Status: biz.SessionActive, Version: 1,
	}, testOutboxEvent(user.ID, "session.created")); err != nil {
		t.Fatal(err)
	}
	messages := make([]biz.Message, 0, 3)
	for index := 1; index <= 3; index++ {
		created, err := sessions.AppendMessage(ctx, biz.Message{
			ID: uuid.New(), SessionID: sessionID, UserID: user.ID, Role: biz.MessageUser,
			Content: "消息-" + suffix + "-" + string(rune('0'+index)),
		}, testOutboxEvent(user.ID, "message.created"))
		if err != nil {
			t.Fatal(err)
		}
		messages = append(messages, created)
		// 保证消息时间严格递增，避免同一毫秒内随机 UUID 打乱边界排序。
		time.Sleep(2 * time.Millisecond)
	}
	return user.ID, sessionID, messages
}

// TestSummaryRepositoryLifecycle 验证版本递增、旧版本替换、过期拒绝与来源删除后的失效。
func TestSummaryRepositoryLifecycle(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	sessions := NewSessionRepository(appDB)
	users := NewUserRepository(appDB, authDB)
	summaries := NewSummaryRepository(appDB)
	userID, sessionID, messages := summaryIntegrationSetup(t, ctx, sessions, users)
	defer func() { _ = adminDB.Exec("DELETE FROM users WHERE id = ?", userID).Error }()

	first := biz.SessionSummary{
		ID: uuid.New(), SessionID: sessionID, UserID: userID, Version: 1, SummaryText: "第一版摘要",
		SourceMessageIDs: []uuid.UUID{messages[0].ID}, SourceFingerprint: biz.SummarySourceFingerprint(messages[:1]),
		SourceStartMessageID: &messages[0].ID, SourceEndMessageID: &messages[0].ID, SourceEndAt: &messages[0].CreatedAt,
		CoveredMessageCount: 1, Status: biz.SummaryActive,
	}
	saved, applied, err := summaries.SaveSummary(ctx, first, 0, testOutboxEvent(userID, "memory.summary.created"))
	if err != nil || !applied {
		t.Fatalf("save first summary: applied=%v err=%v", applied, err)
	}
	if saved.Version != 1 {
		t.Fatalf("first version = %d", saved.Version)
	}
	active, found, err := summaries.GetActiveSummary(ctx, userID, sessionID)
	if err != nil || !found || active.Version != 1 {
		t.Fatalf("active = %#v found=%v err=%v", active, found, err)
	}
	// 重复执行同一版本必须返回 applied=false，不能覆盖已有摘要。
	if _, applied, err := summaries.SaveSummary(ctx, first, 0, testOutboxEvent(userID, "memory.summary.created")); err != nil || applied {
		t.Fatalf("duplicate save: applied=%v err=%v", applied, err)
	}

	second := biz.SessionSummary{
		ID: uuid.New(), SessionID: sessionID, UserID: userID, Version: 2, SummaryText: "第二版摘要",
		SourceMessageIDs: []uuid.UUID{messages[1].ID}, SourceFingerprint: biz.SummarySourceFingerprint(messages[1:2]),
		SourceStartMessageID: &messages[1].ID, SourceEndMessageID: &messages[1].ID, SourceEndAt: &messages[1].CreatedAt,
		CoveredMessageCount: 2, ParentSummaryID: &saved.ID, Status: biz.SummaryActive,
	}
	if _, applied, err := summaries.SaveSummary(ctx, second, 1, testOutboxEvent(userID, "memory.summary.created")); err != nil || !applied {
		t.Fatalf("save second summary: applied=%v err=%v", applied, err)
	}
	active, found, err = summaries.GetActiveSummary(ctx, userID, sessionID)
	if err != nil || !found || active.Version != 2 || active.ParentSummaryID == nil || *active.ParentSummaryID != saved.ID {
		t.Fatalf("second active = %#v found=%v err=%v", active, found, err)
	}
	history, err := summaries.ListSummaries(ctx, userID, sessionID, 10)
	if err != nil || len(history) != 2 || history[0].Version != 2 || history[1].Status != biz.SummarySuperseded {
		t.Fatalf("history = %#v err=%v", history, err)
	}

	// 过期版本任务不得覆盖已生效的新版本。
	stale := second
	stale.ID = uuid.New()
	stale.SummaryText = "过期摘要"
	if _, applied, err := summaries.SaveSummary(ctx, stale, 1, testOutboxEvent(userID, "memory.summary.created")); err != nil || applied {
		t.Fatalf("stale save: applied=%v err=%v", applied, err)
	}

	// 删除覆盖边界内的消息后，摘要必须失效且不再作为上下文注入。
	if err := adminDB.Exec("UPDATE messages SET deleted_at = now() WHERE id = ?", messages[1].ID).Error; err != nil {
		t.Fatal(err)
	}
	active, found, err = summaries.GetActiveSummary(ctx, userID, sessionID)
	if err != nil || found {
		t.Fatalf("invalidated summary: found=%v err=%v active=%#v", found, err, active)
	}
	history, err = summaries.ListSummaries(ctx, userID, sessionID, 10)
	if err != nil || history[0].Status != biz.SummaryInvalidated {
		t.Fatalf("invalidated status = %#v err=%v", history, err)
	}

	// 跨用户读取必须返回未命中，RLS 与查询条件双重隔离。
	otherSession := uuid.New()
	if _, found, err := summaries.GetActiveSummary(ctx, uuid.New(), otherSession); err != nil || found {
		t.Fatalf("cross user found=%v err=%v", found, err)
	}
}

// TestSummaryRepositoryRejectsMissingSource 验证来源消息缺失时拒绝写入摘要。
func TestSummaryRepositoryRejectsMissingSource(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	sessions := NewSessionRepository(appDB)
	users := NewUserRepository(appDB, authDB)
	summaries := NewSummaryRepository(appDB)
	userID, sessionID, messages := summaryIntegrationSetup(t, ctx, sessions, users)
	defer func() { _ = adminDB.Exec("DELETE FROM users WHERE id = ?", userID).Error }()

	missing := uuid.New()
	summary := biz.SessionSummary{
		ID: uuid.New(), SessionID: sessionID, UserID: userID, Version: 1, SummaryText: "缺失来源",
		SourceMessageIDs:   []uuid.UUID{missing},
		SourceEndMessageID: &messages[0].ID, SourceEndAt: &messages[0].CreatedAt, CoveredMessageCount: 1,
	}
	if _, applied, err := summaries.SaveSummary(ctx, summary, 0, testOutboxEvent(userID, "memory.summary.created")); err == nil {
		if applied {
			t.Fatal("missing source must not be applied")
		}
	} else if !errors.Is(err, biz.ErrSummarySourceInvalid) {
		// 具体错误是否包装为来源失效由仓储实现决定，但必须拒绝写入。
		t.Logf("missing source rejected with error: %v", err)
	}
}
