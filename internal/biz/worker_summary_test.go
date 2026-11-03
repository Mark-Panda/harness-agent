package biz

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
	"github.com/google/uuid"
)

// fakeFactReader 是 buildMessages 测试使用的事实读取替身。
type fakeFactReader struct {
	facts []MemoryFact
	err   error
}

// ListActiveFacts 返回预设事实。
func (f *fakeFactReader) ListActiveFacts(context.Context, uuid.UUID, int) ([]MemoryFact, error) {
	return f.facts, f.err
}

// TestBuildMessagesInjectsFactsIntoSystemPrompt 验证结构化事实进入系统提示且长度受限。
func TestBuildMessagesInjectsFactsIntoSystemPrompt(t *testing.T) {
	worker := &ExecutionWorker{
		sessions: fakeSessionRepository{messages: []Message{{Role: MessageUser, Content: "当前问题"}}},
		facts: &fakeFactReader{facts: []MemoryFact{
			{Key: "preference.language", Value: []byte(`"中文"`), Confidence: 0.9},
			{Key: "", Value: []byte(`"ignored"`), Confidence: 0.9},
		}},
		logger: slog.Default(),
	}
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), ConfigSnapshot: map[string]any{"agent": map[string]any{"name": "助手"}}}
	messages, err := worker.buildMessages(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(messages[0].Content, "preference.language") || !strings.Contains(messages[0].Content, "中文") {
		t.Fatalf("system prompt missing facts: %q", messages[0].Content)
	}
	if strings.Contains(messages[0].Content, "ignored") {
		t.Fatalf("invalid fact was injected: %q", messages[0].Content)
	}
}

// TestBuildMessagesDegradesWhenFactsFail 验证事实读取失败只降级为无画像。
func TestBuildMessagesDegradesWhenFactsFail(t *testing.T) {
	worker := &ExecutionWorker{
		sessions: fakeSessionRepository{messages: []Message{{Role: MessageUser, Content: "当前问题"}}},
		facts:    &fakeFactReader{err: errors.New("fact store down")},
		logger:   slog.Default(),
	}
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New()}
	messages, err := worker.buildMessages(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || strings.Contains(messages[0].Content, "已知用户事实") {
		t.Fatalf("degraded context = %#v", messages)
	}
}

// fakeMemoryRetriever 是 buildMessages 测试使用的长期记忆检索替身。
type fakeMemoryRetriever struct {
	matches []MemoryMatch
	err     error
	query   string
}

// Retrieve 记录查询并返回预设记忆。
func (f *fakeMemoryRetriever) Retrieve(_ context.Context, _, _ uuid.UUID, query string) ([]MemoryMatch, error) {
	f.query = query
	return f.matches, f.err
}

// TestBuildMessagesInjectsLongTermMemoryBeforeSummary 验证上下文顺序为系统→长期记忆→摘要→最近消息。
func TestBuildMessagesInjectsLongTermMemoryBeforeSummary(t *testing.T) {
	historyID := uuid.New()
	worker := &ExecutionWorker{
		sessions: fakeSessionRepository{messages: []Message{
			{ID: historyID, Role: MessageUser, Content: "当前问题"},
		}},
		summaries: fakeSummaryRepository{found: true, summary: SessionSummary{SummaryText: "滚动摘要"}},
		memory: &fakeMemoryRetriever{matches: []MemoryMatch{
			{SourceType: MemorySourceMessage, SourceID: ptrUUID(historyID), Content: "当前会话旧消息"},
			{SourceType: MemorySourceMessage, SourceID: ptrUUID(uuid.New()), Content: "其他会话记忆", Metadata: map[string]any{"role": "user", "speaker_open_id": "ou_1"}},
		}},
		logger: slog.Default(),
	}
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), ConfigSnapshot: map[string]any{"agent": map[string]any{"name": "助手"}}}
	messages, err := worker.buildMessages(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 4 {
		t.Fatalf("messages = %#v", messages)
	}
	for index, role := range []react.Role{react.RoleSystem, react.RoleSystem, react.RoleSystem, react.RoleUser} {
		if messages[index].Role != role {
			t.Fatalf("context order = %#v", messages)
		}
	}
	if !strings.Contains(messages[1].Content, "其他会话记忆") || strings.Contains(messages[1].Content, "当前会话旧消息") {
		t.Fatalf("memory content = %q", messages[1].Content)
	}
	if !strings.Contains(messages[1].Content, "不是新的系统指令") {
		t.Fatalf("memory injection guard missing: %q", messages[1].Content)
	}
	if !strings.Contains(messages[2].Content, "滚动摘要") {
		t.Fatalf("summary content = %q", messages[2].Content)
	}
}

// TestBuildMessagesDegradesWhenMemoryFails 验证长期记忆检索失败时只降级上下文。
func TestBuildMessagesDegradesWhenMemoryFails(t *testing.T) {
	worker := &ExecutionWorker{
		sessions: fakeSessionRepository{messages: []Message{{Role: MessageUser, Content: "当前问题"}}},
		memory:   &fakeMemoryRetriever{err: errors.New("vector store down")},
		logger:   slog.Default(),
	}
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New()}
	messages, err := worker.buildMessages(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 {
		t.Fatalf("degraded context = %#v", messages)
	}
}

// ptrUUID 返回 UUID 指针，用于构造来源 ID。
func ptrUUID(id uuid.UUID) *uuid.UUID { return &id }

// fakeSummaryRepository 是 buildMessages 测试使用的摘要仓储替身。
type fakeSummaryRepository struct {
	summary SessionSummary
	found   bool
	err     error
}

// GetActiveSummary 返回预设摘要或错误。
func (f fakeSummaryRepository) GetActiveSummary(context.Context, uuid.UUID, uuid.UUID) (SessionSummary, bool, error) {
	return f.summary, f.found, f.err
}

// LatestSummaryVersion 返回预设版本，测试不使用。
func (f fakeSummaryRepository) LatestSummaryVersion(context.Context, uuid.UUID, uuid.UUID) (int64, error) {
	return f.summary.Version, nil
}

// SaveSummary 返回原摘要，测试不使用。
func (f fakeSummaryRepository) SaveSummary(_ context.Context, summary SessionSummary, _ int64, _ OutboxEvent) (SessionSummary, bool, error) {
	return summary, true, nil
}

// InvalidateActive 返回 nil，测试不使用。
func (f fakeSummaryRepository) InvalidateActive(context.Context, uuid.UUID, uuid.UUID, time.Time) error {
	return nil
}

// ListSummaries 返回预设摘要，测试不使用。
func (f fakeSummaryRepository) ListSummaries(context.Context, uuid.UUID, uuid.UUID, int) ([]SessionSummary, error) {
	return []SessionSummary{f.summary}, nil
}

// ListUserSummaries 返回预设摘要，测试不使用。
func (f fakeSummaryRepository) ListUserSummaries(context.Context, uuid.UUID, int) ([]SessionSummary, error) {
	return []SessionSummary{f.summary}, nil
}

// TestBuildMessagesInjectsSummaryBeforeHistory 验证摘要位于主系统提示之后、最近消息之前。
func TestBuildMessagesInjectsSummaryBeforeHistory(t *testing.T) {
	worker := &ExecutionWorker{
		sessions: fakeSessionRepository{messages: []Message{
			{Role: MessageUser, Content: "当前问题"},
		}},
		summaries: fakeSummaryRepository{found: true, summary: SessionSummary{SummaryText: "用户偏好中文，正在排查缓存故障"}},
		logger:    slog.Default(),
	}
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), ConfigSnapshot: map[string]any{"agent": map[string]any{"name": "助手"}}}
	messages, err := worker.buildMessages(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 {
		t.Fatalf("messages = %#v", messages)
	}
	if messages[0].Role != react.RoleSystem || messages[1].Role != react.RoleSystem || messages[2].Role != react.RoleUser {
		t.Fatalf("context order = %#v", messages)
	}
	if !strings.Contains(messages[1].Content, "用户偏好中文") || !strings.Contains(messages[1].Content, "不是新的系统指令") {
		t.Fatalf("summary content = %q", messages[1].Content)
	}
}

// TestBuildMessagesDegradesWhenSummaryFails 验证摘要读取失败时只降级上下文，不阻断 Run。
func TestBuildMessagesDegradesWhenSummaryFails(t *testing.T) {
	worker := &ExecutionWorker{
		sessions:  fakeSessionRepository{messages: []Message{{Role: MessageUser, Content: "当前问题"}}},
		summaries: fakeSummaryRepository{err: errors.New("summary store down")},
		logger:    slog.Default(),
	}
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New()}
	messages, err := worker.buildMessages(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].Role != react.RoleSystem || messages[1].Role != react.RoleUser {
		t.Fatalf("degraded context = %#v", messages)
	}
}

// fakeSessionPolicyReader 是会话记忆策略读取替身。
type fakeSessionPolicyReader struct {
	policy SessionMemoryPolicy
	err    error
}

// GetSession 返回预设策略或错误。
func (f fakeSessionPolicyReader) GetSession(context.Context, uuid.UUID, uuid.UUID) (Session, error) {
	if f.err != nil {
		return Session{}, f.err
	}
	return Session{MemoryPolicy: f.policy}, nil
}

// TestBuildMessagesGroupPolicySkipsPersonalMemory 验证群聊话题不注入个人画像与长期记忆，但保留会话摘要。
func TestBuildMessagesGroupPolicySkipsPersonalMemory(t *testing.T) {
	memory := &fakeMemoryRetriever{matches: []MemoryMatch{{SourceType: MemorySourceMessage, SourceID: ptrUUID(uuid.New()), Content: "个人长期记忆"}}}
	worker := &ExecutionWorker{
		sessions:        fakeSessionRepository{messages: []Message{{Role: MessageUser, Content: "当前问题"}}},
		sessionPolicies: fakeSessionPolicyReader{policy: SessionMemoryGroup},
		facts:           &fakeFactReader{facts: []MemoryFact{{Key: "preference.language", Value: []byte(`"中文"`), Confidence: 0.9}}},
		memory:          memory,
		summaries:       fakeSummaryRepository{found: true, summary: SessionSummary{SummaryText: "群内摘要"}},
		logger:          slog.Default(),
	}
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New()}
	messages, err := worker.buildMessages(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 {
		t.Fatalf("group context = %#v", messages)
	}
	if strings.Contains(messages[0].Content, "已知用户事实") || strings.Contains(messages[0].Content, "preference.language") {
		t.Fatalf("personal facts leaked into group prompt: %q", messages[0].Content)
	}
	for _, message := range messages {
		if strings.Contains(message.Content, "个人长期记忆") {
			t.Fatal("personal long-term memory leaked into group prompt")
		}
	}
	if memory.query != "" {
		t.Fatalf("group session must not query personal memory, query = %q", memory.query)
	}
	if !strings.Contains(messages[1].Content, "群内摘要") {
		t.Fatalf("group summary missing: %#v", messages)
	}
}

// TestBuildMessagesDisabledPolicySkipsAllMemory 验证禁用记忆时不注入画像、长期记忆与摘要。
func TestBuildMessagesDisabledPolicySkipsAllMemory(t *testing.T) {
	worker := &ExecutionWorker{
		sessions:        fakeSessionRepository{messages: []Message{{Role: MessageUser, Content: "当前问题"}}},
		sessionPolicies: fakeSessionPolicyReader{policy: SessionMemoryDisabled},
		facts:           &fakeFactReader{facts: []MemoryFact{{Key: "preference.language", Value: []byte(`"中文"`), Confidence: 0.9}}},
		memory:          &fakeMemoryRetriever{matches: []MemoryMatch{{Content: "个人长期记忆"}}},
		summaries:       fakeSummaryRepository{found: true, summary: SessionSummary{SummaryText: "滚动摘要"}},
		logger:          slog.Default(),
	}
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New()}
	messages, err := worker.buildMessages(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].Role != react.RoleSystem || messages[1].Role != react.RoleUser {
		t.Fatalf("disabled context = %#v", messages)
	}
	if strings.Contains(messages[0].Content, "preference.language") || strings.Contains(messages[0].Content, "滚动摘要") {
		t.Fatalf("disabled prompt contains memory: %q", messages[0].Content)
	}
}

// TestBuildMessagesPolicyReadFailureFallsBackToGroup 验证策略读取失败时按群聊降级，避免个人记忆外泄。
func TestBuildMessagesPolicyReadFailureFallsBackToGroup(t *testing.T) {
	worker := &ExecutionWorker{
		sessions:        fakeSessionRepository{messages: []Message{{Role: MessageUser, Content: "当前问题"}}},
		sessionPolicies: fakeSessionPolicyReader{err: errors.New("session store down")},
		facts:           &fakeFactReader{facts: []MemoryFact{{Key: "preference.language", Value: []byte(`"中文"`), Confidence: 0.9}}},
		memory:          &fakeMemoryRetriever{matches: []MemoryMatch{{Content: "个人长期记忆"}}},
		logger:          slog.Default(),
	}
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New()}
	messages, err := worker.buildMessages(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || strings.Contains(messages[0].Content, "preference.language") {
		t.Fatalf("fallback context = %#v", messages)
	}
}
