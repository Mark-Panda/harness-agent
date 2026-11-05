package biz_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
)

// fakeAttachmentRepo 记录服务层写入的附件，用于脱离数据库验证校验逻辑。
type fakeAttachmentRepo struct {
	created  *biz.Attachment
	data     []byte
	text     string
	count    int64
	validate error
}

func (f *fakeAttachmentRepo) Create(_ context.Context, attachment biz.Attachment, data []byte, text string) (biz.Attachment, error) {
	f.created = &attachment
	f.data = data
	f.text = text
	return attachment, nil
}

func (f *fakeAttachmentRepo) Get(_ context.Context, _, _ uuid.UUID) (biz.Attachment, error) {
	return biz.Attachment{}, nil
}

func (f *fakeAttachmentRepo) Open(_ context.Context, _, _ uuid.UUID) (biz.Attachment, []byte, error) {
	return biz.Attachment{}, nil, nil
}

func (f *fakeAttachmentRepo) ListBySession(_ context.Context, _, _ uuid.UUID) ([]biz.Attachment, error) {
	return nil, nil
}

func (f *fakeAttachmentRepo) ListTextContexts(_ context.Context, _, _ uuid.UUID) ([]biz.AttachmentText, error) {
	return nil, nil
}

func (f *fakeAttachmentRepo) Validate(_ context.Context, _, _ uuid.UUID, _ []uuid.UUID) error {
	return f.validate
}

func (f *fakeAttachmentRepo) AttachToMessage(_ context.Context, _, _, _ uuid.UUID, _ []uuid.UUID) error {
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
