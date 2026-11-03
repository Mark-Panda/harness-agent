//go:build integration

package data

import (
	"context"
	"strings"
	"testing"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
)

// TestBusinessWriteAndOutboxCommitAtomically 验证业务写入与审计 Outbox 在同一事务提交。
// 关键断言：审计事件非法时业务消息必须回滚；审计事件合法时两者同时存在。
func TestBusinessWriteAndOutboxCommitAtomically(t *testing.T) {
	adminDB, appDB, authDB := feishuIntegrationDBs(t)
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	users := NewUserRepository(appDB, authDB)
	user, err := users.CreateUser(ctx, biz.User{
		Username: "audit_atomic_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() { _ = adminDB.Exec("DELETE FROM users WHERE id = ?", user.ID).Error })

	sessions := NewSessionRepository(appDB)
	session, err := sessions.CreateSession(ctx, biz.Session{
		ID: uuid.New(), UserID: user.ID, Title: "audit atomic", Source: biz.SessionSourceWeb,
		Status: biz.SessionActive, Version: 1,
	}, testOutboxEvent(user.ID, "test.session."+suffix))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// 审计事件故意缺少 EventType，appendOutbox 返回错误后整个事务必须回滚消息写入。
	invalidMessage := biz.Message{
		ID: uuid.New(), SessionID: session.ID, UserID: user.ID, Role: biz.MessageUser, Content: "must roll back",
	}
	if _, err := sessions.AppendMessage(ctx, invalidMessage, biz.OutboxEvent{
		UserID: user.ID, AggregateType: "session", AggregateID: session.ID.String(), DedupeKey: "atomic_invalid_" + suffix,
	}); err == nil {
		t.Fatal("invalid audit event must fail the transaction")
	}
	var messageCount int64
	if err := adminDB.Table("messages").Where("id = ?", invalidMessage.ID).Count(&messageCount).Error; err != nil {
		t.Fatalf("count rolled back message: %v", err)
	}
	if messageCount != 0 {
		t.Fatalf("message survived rollback: count=%d", messageCount)
	}

	// 合法审计事件与消息必须同时提交。
	validMessage := biz.Message{
		ID: uuid.New(), SessionID: session.ID, UserID: user.ID, Role: biz.MessageUser, Content: "commit together",
	}
	validEvent := biz.OutboxEvent{
		UserID: user.ID, EventType: "test.message", AggregateType: "message",
		AggregateID: validMessage.ID.String(), DedupeKey: "atomic_valid_" + suffix,
		Payload: map[string]any{"outcome": "success"},
	}
	if _, err := sessions.AppendMessage(ctx, validMessage, validEvent); err != nil {
		t.Fatalf("append message: %v", err)
	}
	if err := adminDB.Table("messages").Where("id = ?", validMessage.ID).Count(&messageCount).Error; err != nil {
		t.Fatalf("count committed message: %v", err)
	}
	if messageCount != 1 {
		t.Fatalf("committed message count = %d", messageCount)
	}
	var outboxCount int64
	if err := adminDB.Table("outbox_events").Where("dedupe_key = ?", validEvent.DedupeKey).Count(&outboxCount).Error; err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	if outboxCount != 1 {
		t.Fatalf("outbox count = %d", outboxCount)
	}
}
