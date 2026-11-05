package biz

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestFormatAttachmentContextEnforcesBudget(t *testing.T) {
	messageID := uuid.New()
	longText := strings.Repeat("字", MaxInjectedCharsPerAttachment+10)
	result := formatAttachmentContext([]AttachmentText{
		{MessageID: messageID, FileName: "a.md", Text: "短文本"},
		{MessageID: messageID, FileName: "b.md", Text: longText},
	})
	block := result[messageID]
	if !strings.Contains(block, "短文本") || !strings.Contains(block, "b.md") {
		t.Fatalf("expected both attachments in context, got %q", block)
	}
	if !strings.Contains(block, "内容已截断") {
		t.Fatal("oversized attachment text should be truncated")
	}
	if count := len([]rune(block)); count > MaxInjectedCharsTotal+200 {
		t.Fatalf("context exceeds total budget: %d", count)
	}
}

func TestFormatAttachmentContextSkipsEmptyText(t *testing.T) {
	messageID := uuid.New()
	if result := formatAttachmentContext([]AttachmentText{{MessageID: messageID, FileName: "bin", Text: "  "}}); len(result) != 0 {
		t.Fatalf("empty text should be skipped, got %v", result)
	}
}
