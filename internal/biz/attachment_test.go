package biz_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
)

// fakeAttachmentRepo 记录服务层写入的附件，用于脱离数据库验证校验逻辑。
type fakeAttachmentRepo struct {
	created      *biz.Attachment
	attachment   biz.Attachment
	data         []byte
	text         string
	listed       []biz.Attachment
	count        int64
	validate     error
	validatedIDs []uuid.UUID
	attachedIDs  []uuid.UUID
	deletedID    uuid.UUID
	deleteCalls  int
}

func (f *fakeAttachmentRepo) Create(_ context.Context, attachment biz.Attachment, data []byte, text string) (biz.Attachment, error) {
	f.created = &attachment
	f.data = data
	f.text = text
	return attachment, nil
}

func (f *fakeAttachmentRepo) Get(_ context.Context, _, _ uuid.UUID) (biz.Attachment, error) {
	return f.attachment, nil
}

func (f *fakeAttachmentRepo) Open(_ context.Context, _, _ uuid.UUID) (biz.Attachment, []byte, error) {
	return f.attachment, f.data, nil
}

func (f *fakeAttachmentRepo) ListBySession(_ context.Context, _, _ uuid.UUID) ([]biz.Attachment, error) {
	return f.listed, nil
}

func (f *fakeAttachmentRepo) ListTextContexts(_ context.Context, _, _ uuid.UUID) ([]biz.AttachmentText, error) {
	return nil, nil
}

func (f *fakeAttachmentRepo) Validate(_ context.Context, _, _ uuid.UUID, ids []uuid.UUID) error {
	f.validatedIDs = append([]uuid.UUID(nil), ids...)
	return f.validate
}

func (f *fakeAttachmentRepo) AttachToMessage(_ context.Context, _, _, _ uuid.UUID, ids []uuid.UUID) error {
	f.attachedIDs = append([]uuid.UUID(nil), ids...)
	return nil
}

func (f *fakeAttachmentRepo) DeleteUnbound(_ context.Context, _, attachmentID uuid.UUID) error {
	f.deletedID = attachmentID
	f.deleteCalls++
	return nil
}

func (f *fakeAttachmentRepo) CountBySession(_ context.Context, _, _ uuid.UUID) (int64, error) {
	return f.count, nil
}

// fakeAttachmentSessions 返回固定会话，用于上传前校验。
type fakeAttachmentSessions struct {
	session biz.Session
}

func (f fakeAttachmentSessions) GetSession(_ context.Context, _, _ uuid.UUID) (biz.Session, error) {
	return f.session, nil
}

func newAttachmentService(t *testing.T, repo *fakeAttachmentRepo, session biz.Session) *biz.AttachmentService {
	t.Helper()
	service, err := biz.NewAttachmentService(fakeAttachmentSessions{session: session}, repo)
	if err != nil {
		t.Fatalf("new attachment service: %v", err)
	}
	return service
}

func TestAttachmentUploadExtractsTextAndSanitizesName(t *testing.T) {
	userID, sessionID := uuid.New(), uuid.New()
	repo := &fakeAttachmentRepo{}
	service := newAttachmentService(t, repo, biz.Session{ID: sessionID, UserID: userID, Status: biz.SessionActive})
	attachment, err := service.Upload(context.Background(), biz.Principal{UserID: userID}, sessionID,
		"../../notes.md", "text/markdown", []byte("# 需求\n第一行\r\n第二行"))
	if err != nil {
		t.Fatalf("upload failed: %v", err)
	}
	if attachment.FileName != "notes.md" {
		t.Fatalf("unexpected file name: %s", attachment.FileName)
	}
	if !attachment.TextExtracted || attachment.TextChars == 0 {
		t.Fatalf("text should be extracted: %+v", attachment)
	}
	if !strings.Contains(repo.text, "第一行\n第二行") {
		t.Fatalf("CRLF should be normalized, got %q", repo.text)
	}
	if int64(len(repo.data)) != attachment.SizeBytes {
		t.Fatalf("stored bytes mismatch: %d vs %d", len(repo.data), attachment.SizeBytes)
	}
}

func TestAttachmentUploadRejectsUnsupportedAndOversizedFiles(t *testing.T) {
	userID, sessionID := uuid.New(), uuid.New()
	repo := &fakeAttachmentRepo{}
	service := newAttachmentService(t, repo, biz.Session{ID: sessionID, UserID: userID, Status: biz.SessionActive})
	if _, err := service.Upload(context.Background(), biz.Principal{UserID: userID}, sessionID, "empty.txt", "text/plain", nil); err == nil {
		t.Fatal("empty file should be rejected")
	}
	oversized := make([]byte, biz.MaxAttachmentBytes+1)
	if _, err := service.Upload(context.Background(), biz.Principal{UserID: userID}, sessionID, "big.bin", "application/octet-stream", oversized); err == nil {
		t.Fatal("oversized file should be rejected")
	}
	repo.count = biz.MaxAttachmentsPerSession
	if _, err := service.Upload(context.Background(), biz.Principal{UserID: userID}, sessionID, "a.txt", "text/plain", []byte("a")); err == nil {
		t.Fatal("quota exhaustion should be rejected")
	}
}

func TestAttachmentUploadRejectsInactiveSessionAndBinaryExtraction(t *testing.T) {
	userID, sessionID := uuid.New(), uuid.New()
	repo := &fakeAttachmentRepo{}
	service := newAttachmentService(t, repo, biz.Session{ID: sessionID, UserID: userID, Status: biz.SessionArchived})
	if _, err := service.Upload(context.Background(), biz.Principal{UserID: userID}, sessionID, "a.txt", "text/plain", []byte("a")); err == nil {
		t.Fatal("inactive session should reject uploads")
	}
	service = newAttachmentService(t, repo, biz.Session{ID: sessionID, UserID: userID, Status: biz.SessionActive})
	attachment, err := service.Upload(context.Background(), biz.Principal{UserID: userID}, sessionID, "report.bin", "", []byte{0x00, 0x01})
	if err != nil {
		t.Fatalf("binary upload failed: %v", err)
	}
	if attachment.TextExtracted {
		t.Fatal("binary file should not be extracted")
	}
}

func TestAttachmentServiceQueriesAndMessageBinding(t *testing.T) {
	userID, sessionID, messageID := uuid.New(), uuid.New(), uuid.New()
	attachmentID := uuid.New()
	repo := &fakeAttachmentRepo{
		attachment: biz.Attachment{ID: attachmentID, UserID: userID, SessionID: sessionID},
		data:       []byte("content"),
	}
	repo.listed = []biz.Attachment{repo.attachment}
	service := newAttachmentService(t, repo, biz.Session{ID: sessionID, UserID: userID, Status: biz.SessionActive})
	principal := biz.Principal{UserID: userID}

	got, err := service.Get(context.Background(), principal, attachmentID)
	if err != nil || got.ID != attachmentID {
		t.Fatalf("Get() = %#v, %v", got, err)
	}
	opened, data, err := service.Open(context.Background(), principal, attachmentID)
	if err != nil || opened.ID != attachmentID || string(data) != "content" {
		t.Fatalf("Open() = %#v, %q, %v", opened, data, err)
	}
	listed, err := service.ListBySession(context.Background(), principal, sessionID)
	if err != nil || len(listed) != 1 || listed[0].ID != attachmentID {
		t.Fatalf("ListBySession() = %#v, %v", listed, err)
	}

	ids := []uuid.UUID{attachmentID, uuid.Nil, attachmentID}
	if err := service.ValidateForMessage(context.Background(), principal, sessionID, ids); err != nil {
		t.Fatalf("ValidateForMessage() error = %v", err)
	}
	if len(repo.validatedIDs) != 1 || repo.validatedIDs[0] != attachmentID {
		t.Fatalf("attachment IDs were not deduplicated: %#v", repo.validatedIDs)
	}
	if err := service.ValidateForMessage(context.Background(), principal, sessionID, nil); err != nil {
		t.Fatalf("empty validation error = %v", err)
	}
	if err := service.AttachToMessage(context.Background(), principal, sessionID, messageID, ids); err != nil {
		t.Fatalf("AttachToMessage() error = %v", err)
	}
	if len(repo.attachedIDs) != 1 || repo.attachedIDs[0] != attachmentID {
		t.Fatalf("attached IDs were not deduplicated: %#v", repo.attachedIDs)
	}

	tooMany := make([]uuid.UUID, biz.MaxAttachmentsPerMessage+1)
	for index := range tooMany {
		tooMany[index] = uuid.New()
	}
	if err := service.ValidateForMessage(context.Background(), principal, sessionID, tooMany); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("attachment limit error = %v", err)
	}
	if err := service.AttachToMessage(context.Background(), principal, sessionID, messageID, tooMany); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("attach limit error = %v", err)
	}
}

func TestAttachmentServiceDeleteUnboundAndAuthorization(t *testing.T) {
	userID, sessionID, attachmentID := uuid.New(), uuid.New(), uuid.New()
	repo := &fakeAttachmentRepo{attachment: biz.Attachment{ID: attachmentID, UserID: userID, SessionID: sessionID}}
	service := newAttachmentService(t, repo, biz.Session{ID: sessionID, UserID: userID, Status: biz.SessionActive})
	principal := biz.Principal{UserID: userID}
	if err := service.DeleteUnbound(context.Background(), principal, attachmentID); err != nil {
		t.Fatalf("DeleteUnbound() error = %v", err)
	}
	if repo.deleteCalls != 1 || repo.deletedID != attachmentID {
		t.Fatalf("unexpected deletion: calls=%d id=%s", repo.deleteCalls, repo.deletedID)
	}
	repo.attachment.MessageID = &sessionID
	if err := service.DeleteUnbound(context.Background(), principal, attachmentID); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("bound attachment deletion error = %v", err)
	}
	if repo.deleteCalls != 1 {
		t.Fatal("bound attachment must not reach the repository deleter")
	}

	anonymous := biz.Principal{}
	if _, err := service.Get(context.Background(), anonymous, attachmentID); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("anonymous Get error = %v", err)
	}
	if _, _, err := service.Open(context.Background(), anonymous, attachmentID); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("anonymous Open error = %v", err)
	}
	if _, err := service.ListBySession(context.Background(), anonymous, sessionID); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("anonymous List error = %v", err)
	}
	if err := service.ValidateForMessage(context.Background(), anonymous, sessionID, nil); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("anonymous Validate error = %v", err)
	}
	if err := service.AttachToMessage(context.Background(), anonymous, sessionID, uuid.New(), nil); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("anonymous Attach error = %v", err)
	}
	if err := service.DeleteUnbound(context.Background(), anonymous, attachmentID); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("anonymous Delete error = %v", err)
	}
}
