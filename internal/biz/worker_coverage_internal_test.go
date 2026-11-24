package biz

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
	"github.com/google/uuid"
)

// TestCompactToolResults 验证历史工具结果压缩只作用于最近窗口之前且保留首尾片段。
func TestCompactToolResults(t *testing.T) {
	longContent := strings.Repeat("h", compactToolResultHeadRunes) +
		strings.Repeat("m", 500) +
		strings.Repeat("t", compactToolResultMaxRunes-compactToolResultHeadRunes)
	tests := []struct {
		name       string
		messages   []react.Message
		keepRecent int
		assert     func(t *testing.T, got []react.Message)
	}{
		{
			name:       "empty input",
			messages:   nil,
			keepRecent: 8,
			assert: func(t *testing.T, got []react.Message) {
				if len(got) != 0 {
					t.Fatalf("got %d messages, want 0", len(got))
				}
			},
		},
		{
			name: "long tool result compacted",
			messages: []react.Message{
				{Role: react.RoleUser, Content: "q"},
				{Role: react.RoleTool, Content: longContent},
			},
			keepRecent: 0,
			assert: func(t *testing.T, got []react.Message) {
				content := got[1].Content
				if !strings.Contains(content, "earlier tool result compacted") {
					t.Fatalf("missing compact marker: %q", content[:80])
				}
				if !strings.HasPrefix(content, strings.Repeat("h", compactToolResultHeadRunes)) {
					t.Fatal("head segment not preserved")
				}
				if !strings.HasSuffix(content, strings.Repeat("t", compactToolResultMaxRunes-compactToolResultHeadRunes)) {
					t.Fatal("tail segment not preserved")
				}
				wantLen := compactToolResultMaxRunes + len("\n[... earlier tool result compacted: original  chars ...]\n") + len("2100")
				if len([]rune(content)) > wantLen {
					t.Fatalf("compacted length = %d runes, want <= %d", len([]rune(content)), wantLen)
				}
			},
		},
		{
			name: "short tool result untouched",
			messages: []react.Message{
				{Role: react.RoleTool, Content: strings.Repeat("s", compactToolResultMaxRunes)},
			},
			keepRecent: 0,
			assert: func(t *testing.T, got []react.Message) {
				if got[0].Content != strings.Repeat("s", compactToolResultMaxRunes) {
					t.Fatal("short tool result was modified")
				}
			},
		},
		{
			name: "recent window untouched",
			messages: []react.Message{
				{Role: react.RoleTool, Content: longContent},
				{Role: react.RoleTool, Content: longContent},
			},
			keepRecent: 1,
			assert: func(t *testing.T, got []react.Message) {
				if !strings.Contains(got[0].Content, "compacted") {
					t.Fatal("old tool result was not compacted")
				}
				if got[1].Content != longContent {
					t.Fatal("recent tool result was compacted")
				}
			},
		},
		{
			name: "negative keepRecent compacts all",
			messages: []react.Message{
				{Role: react.RoleTool, Content: longContent},
			},
			keepRecent: -3,
			assert: func(t *testing.T, got []react.Message) {
				if !strings.Contains(got[0].Content, "compacted") {
					t.Fatal("negative keepRecent did not compact all history")
				}
			},
		},
		{
			name: "non tool roles untouched",
			messages: []react.Message{
				{Role: react.RoleUser, Content: longContent},
				{Role: react.RoleAssistant, Content: longContent},
			},
			keepRecent: 0,
			assert: func(t *testing.T, got []react.Message) {
				if got[0].Content != longContent || got[1].Content != longContent {
					t.Fatal("non-tool message was compacted")
				}
			},
		},
		{
			name: "multibyte runes compacted by rune count",
			messages: []react.Message{
				{Role: react.RoleTool, Content: strings.Repeat("汉", compactToolResultMaxRunes+100)},
			},
			keepRecent: 0,
			assert: func(t *testing.T, got []react.Message) {
				if !strings.Contains(got[0].Content, "original 1700 chars") {
					t.Fatalf("rune count not used in marker: %q", got[0].Content[compactToolResultHeadRunes*3:compactToolResultHeadRunes*3+80])
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 输入不可变：复制一份用于事后比对。
			original := append([]react.Message(nil), tt.messages...)
			got := compactToolResults(tt.messages, tt.keepRecent)
			if len(got) != len(tt.messages) {
				t.Fatalf("got %d messages, want %d", len(got), len(tt.messages))
			}
			tt.assert(t, got)
			for index := range original {
				if tt.messages[index].Content != original[index].Content {
					t.Fatalf("input message %d mutated", index)
				}
			}
		})
	}
}

// TestRunEventSinkFail 验证只记录首个落库错误并以根因取消执行上下文。
func TestRunEventSinkFail(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	sink := &runEventSink{cancel: cancel}

	sink.Fail(nil)
	if sink.Err() != nil {
		t.Fatal("nil error must be ignored")
	}
	first := errors.New("db write failed")
	sink.Fail(first)
	if !errors.Is(sink.Err(), first) {
		t.Fatalf("Err() = %v, want first error", sink.Err())
	}
	if !errors.Is(context.Cause(ctx), first) {
		t.Fatalf("cancel cause = %v, want first error", context.Cause(ctx))
	}
	sink.Fail(errors.New("second error"))
	if !errors.Is(sink.Err(), first) {
		t.Fatal("second error overwrote first root cause")
	}

	var nilSink *runEventSink
	if nilSink.Err() != nil {
		t.Fatal("nil sink must report no error")
	}
	if err := (&runEventSink{}).Err(); err != nil {
		t.Fatalf("zero sink Err() = %v", err)
	}
}

// TestRecentAttachmentTexts 验证附件文本只回注最近若干轮用户消息的附件。
func TestRecentAttachmentTexts(t *testing.T) {
	user1, user2, user3 := uuid.New(), uuid.New(), uuid.New()
	history := []Message{
		{ID: user1, Role: MessageUser},
		{ID: uuid.New(), Role: MessageAssistant},
		{ID: user2, Role: MessageUser},
		{ID: uuid.New(), Role: MessageAssistant},
		{ID: user3, Role: MessageUser},
	}
	items := []AttachmentText{
		{MessageID: user1, FileName: "old.txt", Text: "old"},
		{MessageID: user2, FileName: "mid.txt", Text: "mid"},
		{MessageID: user3, FileName: "new.txt", Text: "new"},
		{MessageID: uuid.New(), FileName: "stray.txt", Text: "stray"},
	}
	tests := []struct {
		name      string
		items     []AttachmentText
		turns     int
		wantFiles []string
	}{
		{name: "zero turns drops all", items: items, turns: 0, wantFiles: nil},
		{name: "negative turns drops all", items: items, turns: -1, wantFiles: nil},
		{name: "empty items", items: nil, turns: 3, wantFiles: nil},
		{name: "last one turn", items: items, turns: 1, wantFiles: []string{"new.txt"}},
		{name: "last two turns", items: items, turns: 2, wantFiles: []string{"mid.txt", "new.txt"}},
		{name: "all turns keeps matched only", items: items, turns: 10, wantFiles: []string{"old.txt", "mid.txt", "new.txt"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := recentAttachmentTexts(tt.items, history, tt.turns)
			if len(got) != len(tt.wantFiles) {
				t.Fatalf("got %d items, want %d: %+v", len(got), len(tt.wantFiles), got)
			}
			for index, file := range tt.wantFiles {
				if got[index].FileName != file {
					t.Fatalf("item %d = %s, want %s", index, got[index].FileName, file)
				}
			}
		})
	}
}

// TestWithParentStep 验证父步骤上下文的写入与读取约定。
func TestWithParentStep(t *testing.T) {
	ctx := context.Background()
	if same := withParentStep(ctx, uuid.Nil); same != ctx {
		t.Fatal("nil parent id must not wrap context")
	}
	if got := parentStepFromContext(ctx); got != nil {
		t.Fatalf("parent from bare context = %v, want nil", got)
	}
	parentID := uuid.New()
	withParent := withParentStep(ctx, parentID)
	got := parentStepFromContext(withParent)
	if got == nil || *got != parentID {
		t.Fatalf("parent from context = %v, want %s", got, parentID)
	}
	if got := parentStepFromContext(context.WithValue(ctx, parentStepContextKey{}, uuid.Nil)); got != nil {
		t.Fatalf("nil uuid value must read as nil, got %v", got)
	}
	//nolint:staticcheck // 显式验证 nil 上下文的防御分支。
	if got := parentStepFromContext(nil); got != nil {
		t.Fatalf("nil context parent = %v, want nil", got)
	}
}

// TestObservedAndApprovalToolDelegates 验证包装工具的 Spec 与 RiskLevel 透传内部工具定义。
func TestObservedAndApprovalToolDelegates(t *testing.T) {
	inner := &fakeRuntimeTool{name: "mcp_docs_search", risk: "medium"}

	observed := observedTool{inner: inner}
	if spec := observed.Spec(); spec.Name != "mcp_docs_search" || spec.Description == "" {
		t.Fatalf("observed Spec() = %+v", spec)
	}
	if risk := observed.RiskLevel(); risk != "medium" {
		t.Fatalf("observed RiskLevel() = %q", risk)
	}

	approval := approvalTool{inner: inner}
	if spec := approval.Spec(); spec.Name != "mcp_docs_search" || !json.Valid(spec.Parameters) {
		t.Fatalf("approval Spec() = %+v", spec)
	}
	if risk := approval.RiskLevel(); risk != "medium" {
		t.Fatalf("approval RiskLevel() = %q", risk)
	}

	defaultRisk := observedTool{inner: &fakeRuntimeTool{name: "search"}}
	if risk := defaultRisk.RiskLevel(); risk != "low" {
		t.Fatalf("default RiskLevel() = %q, want low", risk)
	}
}
