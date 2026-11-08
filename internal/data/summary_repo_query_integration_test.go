//go:build integration

package data

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
)

// TestSummaryRepositoryQueryPaths 验证版本号查询、显式失效、按用户导出与消息游标读取。
func TestSummaryRepositoryQueryPaths(t *testing.T) {
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

	// 空会话的版本号为 0；参数校验拒绝空身份。
	if version, err := summaries.LatestSummaryVersion(ctx, userID, sessionID); err != nil || version != 0 {
		t.Fatalf("empty LatestSummaryVersion = %d, %v", version, err)
	}
	if _, err := sessions.ListRecentMessages(ctx, uuid.Nil, sessionID, 10); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("ListRecentMessages nil user error = %v", err)
	}
	if _, err := sessions.CountMessagesThrough(ctx, userID, sessionID, biz.MessagePosition{}); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("CountMessagesThrough zero boundary error = %v", err)
	}

	// 消息游标查询：正序翻页覆盖全部消息且不重复。
	count, err := sessions.CountMessagesThrough(ctx, userID, sessionID, biz.MessagePosition{CreatedAt: messages[1].CreatedAt, ID: messages[1].ID})
	if err != nil || count != 2 {
		t.Fatalf("CountMessagesThrough = %d, %v", count, err)
	}
	firstPage, err := sessions.ListMessagesAfter(ctx, userID, sessionID, biz.MessagePosition{}, 2)
	if err != nil || len(firstPage) != 2 || firstPage[0].ID != messages[0].ID {
		t.Fatalf("first page = %#v, %v", firstPage, err)
	}
	secondPage, err := sessions.ListMessagesAfter(ctx, userID, sessionID, biz.MessagePosition{CreatedAt: firstPage[1].CreatedAt, ID: firstPage[1].ID}, 10)
	if err != nil || len(secondPage) != 1 || secondPage[0].ID != messages[2].ID {
		t.Fatalf("second page = %#v, %v", secondPage, err)
	}
	recent, err := sessions.ListRecentMessages(ctx, userID, sessionID, 2)
	if err != nil || len(recent) != 2 || recent[0].ID != messages[2].ID {
		t.Fatalf("ListRecentMessages = %#v, %v", recent, err)
	}
	byIDs, err := sessions.ListMessagesByIDs(ctx, userID, sessionID, []uuid.UUID{messages[2].ID, messages[0].ID, uuid.New()})
	if err != nil || len(byIDs) != 2 || byIDs[0].ID != messages[0].ID || byIDs[1].ID != messages[2].ID {
		t.Fatalf("ListMessagesByIDs = %#v, %v", byIDs, err)
	}
	if _, err := sessions.ListMessagesByIDs(ctx, userID, sessionID, nil); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("ListMessagesByIDs empty error = %v", err)
	}

	// 写入两版摘要后：LatestSummaryVersion 含失效版本，InvalidateActive 使 active 消失。
	first := biz.SessionSummary{
		ID: uuid.New(), SessionID: sessionID, UserID: userID, Version: 1, SummaryText: "v1",
		SourceMessageIDs: []uuid.UUID{messages[0].ID}, SourceFingerprint: biz.SummarySourceFingerprint(messages[:1]),
		SourceStartMessageID: &messages[0].ID, SourceEndMessageID: &messages[0].ID, SourceEndAt: &messages[0].CreatedAt,
		CoveredMessageCount: 1, Status: biz.SummaryActive,
	}
	if _, applied, err := summaries.SaveSummary(ctx, first, 0, testOutboxEvent(userID, "memory.summary.created")); err != nil || !applied {
		t.Fatalf("save v1: applied=%v err=%v", applied, err)
	}
	if version, err := summaries.LatestSummaryVersion(ctx, userID, sessionID); err != nil || version != 1 {
		t.Fatalf("LatestSummaryVersion = %d, %v", version, err)
	}
	if err := summaries.InvalidateActive(ctx, userID, sessionID, time.Now().UTC()); err != nil {
		t.Fatalf("InvalidateActive() = %v", err)
	}
	if _, found, err := summaries.GetActiveSummary(ctx, userID, sessionID); err != nil || found {
		t.Fatalf("active after invalidate: found=%v err=%v", found, err)
	}
	if version, err := summaries.LatestSummaryVersion(ctx, userID, sessionID); err != nil || version != 1 {
		t.Fatalf("LatestSummaryVersion after invalidate = %d, %v", version, err)
	}
	if err := summaries.InvalidateActive(ctx, uuid.Nil, sessionID, time.Now().UTC()); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("InvalidateActive nil user error = %v", err)
	}

	exported, err := summaries.ListUserSummaries(ctx, userID, 10)
	if err != nil || len(exported) != 1 || exported[0].Status != biz.SummaryInvalidated {
		t.Fatalf("ListUserSummaries = %#v, %v", exported, err)
	}

	// 跨用户隔离：B 用户读不到 A 用户的摘要与消息。
	otherID, otherSessionID, _ := summaryIntegrationSetup(t, ctx, sessions, users)
	defer func() { _ = adminDB.Exec("DELETE FROM users WHERE id = ?", otherID).Error }()
	if exported, err := summaries.ListUserSummaries(ctx, otherID, 10); err != nil || len(exported) != 0 {
		t.Fatalf("cross-user ListUserSummaries = %#v, %v", exported, err)
	}
	if listed, err := summaries.ListSummaries(ctx, otherID, sessionID, 10); err != nil || len(listed) != 0 {
		t.Fatalf("cross-user ListSummaries = %#v, %v", listed, err)
	}
	if version, err := summaries.LatestSummaryVersion(ctx, otherID, sessionID); err != nil || version != 0 {
		t.Fatalf("cross-user LatestSummaryVersion = %d, %v", version, err)
	}
	if recent, err := sessions.ListRecentMessages(ctx, otherID, sessionID, 10); err != nil || len(recent) != 0 {
		t.Fatalf("cross-user ListRecentMessages = %#v, %v", recent, err)
	}
	if count, err := sessions.CountMessagesThrough(ctx, otherID, sessionID, biz.MessagePosition{CreatedAt: messages[2].CreatedAt, ID: messages[2].ID}); err != nil || count != 0 {
		t.Fatalf("cross-user CountMessagesThrough = %d, %v", count, err)
	}
	if byIDs, err := sessions.ListMessagesByIDs(ctx, otherID, otherSessionID, []uuid.UUID{messages[0].ID}); err != nil || len(byIDs) != 0 {
		t.Fatalf("cross-user ListMessagesByIDs = %#v, %v", byIDs, err)
	}
}
