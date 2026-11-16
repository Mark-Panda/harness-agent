package biz

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
	"github.com/google/uuid"
)

// pagedChildSessionRepo 只实现 ListMessages，用游标模拟 PostgreSQL 的倒序分页。
type pagedChildSessionRepo struct {
	SessionRepository
	pages   map[string]Page[Message]
	cursors []string
}

// ListMessages 按游标返回预置分页。
func (r *pagedChildSessionRepo) ListMessages(_ context.Context, _, _ uuid.UUID, _ int, cursor string) (Page[Message], error) {
	r.cursors = append(r.cursors, cursor)
	page, ok := r.pages[cursor]
	if !ok {
		return Page[Message]{}, fmt.Errorf("unexpected cursor %q", cursor)
	}
	return page, nil
}

// TestLoadChildHistoryCountsAllMessagesBeyondContextWindow 验证历史窗口截断时仍统计全部消息用于消息 ID 基底。
func TestLoadChildHistoryCountsAllMessagesBeyondContextWindow(t *testing.T) {
	const total = supervisorChildHistoryLimit + 5
	messages := make([]Message, total)
	for index := range messages {
		role := MessageAssistant
		if index%2 == 1 {
			role = MessageTool
		}
		messages[index] = Message{ID: uuid.New(), Role: role, Content: fmt.Sprintf("m-%d", index)}
	}
	// 保留窗口从 index 5 开始，确保首条是 assistant，不会触发孤立工具消息裁剪。
	messages[5].Role = MessageAssistant

	const pageSize = 200
	pages := map[string]Page[Message]{}
	cursor := ""
	for start := total; start > 0; start -= pageSize {
		begin := start - pageSize
		if begin < 0 {
			begin = 0
		}
		items := make([]Message, 0, start-begin)
		for index := start - 1; index >= begin; index-- {
			items = append(items, messages[index])
		}
		next := ""
		if begin > 0 {
			next = fmt.Sprintf("cursor-%d", begin)
		}
		pages[cursor] = Page[Message]{Items: items, NextCursor: next}
		cursor = next
	}

	repo := &pagedChildSessionRepo{pages: pages}
	worker := &ExecutionWorker{sessions: repo}
	history, persisted, err := worker.loadChildHistory(context.Background(), Run{UserID: uuid.New()}, SupervisorTask{ChildSessionID: uuid.New()})
	if err != nil {
		t.Fatal(err)
	}
	if persisted != total {
		t.Fatalf("persisted = %d, want %d", persisted, total)
	}
	if len(history) != supervisorChildHistoryLimit {
		t.Fatalf("history length = %d, want %d", len(history), supervisorChildHistoryLimit)
	}
	if history[0].Content != fmt.Sprintf("m-%d", total-supervisorChildHistoryLimit) {
		t.Fatalf("history head = %q", history[0].Content)
	}
	if history[len(history)-1].Content != fmt.Sprintf("m-%d", total-1) {
		t.Fatalf("history tail = %q", history[len(history)-1].Content)
	}
}

// TestTrimChildHistoryDropsOldestAssistantToolBlocks 验证超预算时按完整工具调用块丢弃最旧历史。
func TestTrimChildHistoryDropsOldestAssistantToolBlocks(t *testing.T) {
	long := strings.Repeat("x", 4000)
	history := []react.Message{
		{Role: react.RoleAssistant, Content: long, ToolCalls: []react.ToolCall{{ID: "old", Name: "lookup"}}},
		{Role: react.RoleTool, ToolCallID: "old", Content: long},
		{Role: react.RoleAssistant, Content: "recent"},
		{Role: react.RoleTool, ToolCallID: "new", Content: "ok"},
	}
	trimmed := trimChildHistory(history, 100)
	if len(trimmed) != 2 || trimmed[0].Role != react.RoleAssistant || trimmed[0].Content != "recent" {
		t.Fatalf("trimmed = %#v", trimmed)
	}
	if got := trimChildHistory(history, 0); len(got) != len(history) {
		t.Fatalf("zero budget must keep history: %d", len(got))
	}
}

// TestSanitizeChildHistoryDropsOrphanTools 验证回放窗口起点不会出现无对应 assistant 的工具消息。
func TestSanitizeChildHistoryDropsOrphanTools(t *testing.T) {
	history := []react.Message{
		{Role: react.RoleTool, ToolCallID: "orphan-1"},
		{Role: react.RoleTool, ToolCallID: "orphan-2"},
		{Role: react.RoleAssistant, Content: "start"},
		{Role: react.RoleTool, ToolCallID: "call"},
	}
	sanitized := sanitizeChildHistory(history)
	if len(sanitized) != 2 || sanitized[0].Content != "start" || sanitized[1].ToolCallID != "call" {
		t.Fatalf("sanitized = %#v", sanitized)
	}
	if sanitizeChildHistory([]react.Message{{Role: react.RoleTool}}) != nil {
		t.Fatal("all-orphan history must be dropped")
	}
}
