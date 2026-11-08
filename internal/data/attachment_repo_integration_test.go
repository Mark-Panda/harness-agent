//go:build integration

package data

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
)

// TestAttachmentRepositoryLifecycle 验证附件创建/读取/绑定/删除与用户隔离。
func TestAttachmentRepositoryLifecycle(t *testing.T) {
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
		Username: "attach_owner_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	other, err := users.CreateUser(ctx, biz.User{
		Username: "attach_other_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adminDB.Exec("DELETE FROM users WHERE id IN (?, ?)", owner.ID, other.ID).Error }()

	sessions := NewSessionRepository(appDB)
	sessionID := createMemoryIntegrationSession(t, ctx, sessions, owner.ID, "attach-"+suffix)
	repo := NewAttachmentRepository(appDB)

	if _, err := repo.Create(ctx, biz.Attachment{ID: uuid.Nil, UserID: owner.ID, SessionID: sessionID}, nil, ""); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("create without identity error = %v", err)
	}

	attachmentID := uuid.New()
	created, err := repo.Create(ctx, biz.Attachment{
		ID: attachmentID, UserID: owner.ID, SessionID: sessionID, FileName: "notes.txt", MimeType: "text/plain",
		SizeBytes: 5, SHA256: "sha", TextExtracted: true, TextChars: 5,
	}, []byte("hello"), "hello")
	if err != nil || created.ID != attachmentID || created.MessageID != nil {
		t.Fatalf("Create() = %#v, %v", created, err)
	}

	got, err := repo.Get(ctx, owner.ID, attachmentID)
	if err != nil || got.FileName != "notes.txt" || !got.TextExtracted {
		t.Fatalf("Get() = %#v, %v", got, err)
	}
	_, data, err := repo.Open(ctx, owner.ID, attachmentID)
	if err != nil || string(data) != "hello" {
		t.Fatalf("Open() data = %q, %v", data, err)
	}

	// 跨用户隔离：B 用户读不到 A 用户的附件。
	if _, err := repo.Get(ctx, other.ID, attachmentID); err == nil {
		t.Fatal("other user read owner's attachment metadata")
	}
	if _, _, err := repo.Open(ctx, other.ID, attachmentID); err == nil {
		t.Fatal("other user opened owner's attachment content")
	}
	if listed, err := repo.ListBySession(ctx, other.ID, sessionID); err != nil || len(listed) != 0 {
		t.Fatalf("cross-user ListBySession = %#v, %v", listed, err)
	}
	if count, err := repo.CountBySession(ctx, other.ID, sessionID); err != nil || count != 0 {
		t.Fatalf("cross-user CountBySession = %d, %v", count, err)
	}

	if count, err := repo.CountBySession(ctx, owner.ID, sessionID); err != nil || count != 1 {
		t.Fatalf("CountBySession = %d, %v", count, err)
	}
	if err := repo.Validate(ctx, owner.ID, sessionID, []uuid.UUID{attachmentID}); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if err := repo.Validate(ctx, owner.ID, sessionID, []uuid.UUID{attachmentID, uuid.New()}); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("Validate() missing id error = %v", err)
	}

	// 绑定到消息：先落一条真实消息满足外键。
	message, err := sessions.AppendMessage(ctx, biz.Message{
		ID: uuid.New(), SessionID: sessionID, UserID: owner.ID, Role: biz.MessageUser, Content: "带附件",
	}, testOutboxEvent(owner.ID, "message.created"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.AttachToMessage(ctx, owner.ID, sessionID, message.ID, []uuid.UUID{attachmentID}); err != nil {
		t.Fatalf("AttachToMessage() = %v", err)
	}
	bound, err := repo.Get(ctx, owner.ID, attachmentID)
	if err != nil || bound.MessageID == nil || *bound.MessageID != message.ID {
		t.Fatalf("bound attachment = %#v, %v", bound, err)
	}
	// 已绑定附件不可重复绑定、不可校验为未绑定、不可按未绑定删除。
	if err := repo.AttachToMessage(ctx, owner.ID, sessionID, message.ID, []uuid.UUID{attachmentID}); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("re-attach error = %v", err)
	}
	if err := repo.Validate(ctx, owner.ID, sessionID, []uuid.UUID{attachmentID}); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("validate bound error = %v", err)
	}
	if err := repo.DeleteUnbound(ctx, owner.ID, attachmentID); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("delete bound error = %v", err)
	}

	texts, err := repo.ListTextContexts(ctx, owner.ID, sessionID)
	if err != nil || len(texts) != 1 || texts[0].Text != "hello" || texts[0].MessageID != message.ID {
		t.Fatalf("ListTextContexts() = %#v, %v", texts, err)
	}
	if texts, err := repo.ListTextContexts(ctx, other.ID, sessionID); err != nil || len(texts) != 0 {
		t.Fatalf("cross-user ListTextContexts = %#v, %v", texts, err)
	}

	// 未绑定附件可删除，删除后读取失败。
	pendingID := uuid.New()
	if _, err := repo.Create(ctx, biz.Attachment{
		ID: pendingID, UserID: owner.ID, SessionID: sessionID, FileName: "pending.bin", MimeType: "application/octet-stream",
		SizeBytes: 1,
	}, []byte("x"), ""); err != nil {
		t.Fatal(err)
	}
	if err := repo.DeleteUnbound(ctx, owner.ID, pendingID); err != nil {
		t.Fatalf("DeleteUnbound() = %v", err)
	}
	if _, err := repo.Get(ctx, owner.ID, pendingID); err == nil {
		t.Fatal("deleted attachment still readable")
	}
	var contentRows int64
	if err := adminDB.Raw("SELECT COUNT(*) FROM attachment_contents WHERE attachment_id = ?", pendingID).Scan(&contentRows).Error; err != nil || contentRows != 0 {
		t.Fatalf("orphan attachment content rows = %d, %v", contentRows, err)
	}
}
