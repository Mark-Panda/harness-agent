package biz

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

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

// requeueRunRepository captures the worker fence and deadline for a paused Run.
type requeueRunRepository struct {
	RunRepository
	calls    int
	fence    RunFence
	deadline *time.Time
	now      time.Time
	err      error
}

func (r *requeueRunRepository) RequeuePausedRun(_ context.Context, _, _ uuid.UUID, fence RunFence, deadline *time.Time, now time.Time) error {
	r.calls++
	r.fence, r.deadline, r.now = fence, deadline, now
	return r.err
}

// TestRequeuePausedRun 验证暂停 Run 退回 queued 并清空租约、顺延截止时间。
func TestRequeuePausedRun(t *testing.T) {
	now := time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)
	deadline := now.Add(-time.Minute)
	updatedAt := now.Add(-2 * time.Minute)
	run := Run{
		ID: uuid.New(), UserID: uuid.New(), Status: RunRunning,
		DeadlineAt: &deadline, UpdatedAt: updatedAt,
	}

	repo := &requeueRunRepository{}
	worker := &ExecutionWorker{
		runs:   repo,
		clock:  func() time.Time { return now },
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	fence := RunFence{Owner: "worker-a", Token: 1}
	if err := worker.requeuePausedRun(context.Background(), run, fence); err != nil {
		t.Fatal(err)
	}

	if repo.calls != 1 || repo.fence != fence {
		t.Fatalf("transition = %d calls with fence %+v", repo.calls, repo.fence)
	}
	// 暂停耗时 2 分钟必须补回截止时间，避免恢复后被误判超时。
	if repo.deadline == nil || !repo.deadline.Equal(deadline.Add(2*time.Minute)) {
		t.Fatalf("deadline_at = %v, want %v", repo.deadline, deadline.Add(2*time.Minute))
	}
	if !repo.now.Equal(now) {
		t.Fatalf("updated_at = %v, want %v", repo.now, now)
	}

	// 无截止时间：不得写入 deadline_at 字段。
	repoNoDeadline := &requeueRunRepository{}
	workerNoDeadline := &ExecutionWorker{
		runs: repoNoDeadline, clock: func() time.Time { return now },
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := workerNoDeadline.requeuePausedRun(context.Background(), Run{ID: uuid.New(), UserID: uuid.New(), Status: RunRunning}, fence); err != nil {
		t.Fatal(err)
	}
	if repoNoDeadline.deadline != nil {
		t.Fatal("deadline_at written for run without deadline")
	}

	// 冲突与仓储错误都只记录日志，不得 panic。
	for _, cause := range []error{ErrResourceConflict, errors.New("db down")} {
		repoErr := &requeueRunRepository{err: cause}
		workerErr := &ExecutionWorker{
			runs: repoErr, clock: func() time.Time { return now },
			logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		if err := workerErr.requeuePausedRun(context.Background(), run, fence); !errors.Is(err, cause) || repoErr.calls != 1 {
			t.Fatalf("transition calls = %d, error = %v, want %v", repoErr.calls, err, cause)
		}
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
