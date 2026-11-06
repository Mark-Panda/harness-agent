package biz_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/service/mocks"
	"github.com/google/uuid"
	"go.uber.org/mock/gomock"
)

// fakeSummaryReader 是摘要来源消息读取端口的内存实现。
type fakeSummaryReader struct {
	recent []biz.Message
	after  []biz.Message
	byIDs  []biz.Message
	err    error
}

// ListRecentMessages 返回预置的最近消息。
func (f *fakeSummaryReader) ListRecentMessages(context.Context, uuid.UUID, uuid.UUID, int) ([]biz.Message, error) {
	return f.recent, f.err
}

// ListMessagesAfter 返回预置的游标之后消息。
func (f *fakeSummaryReader) ListMessagesAfter(context.Context, uuid.UUID, uuid.UUID, biz.MessagePosition, int) ([]biz.Message, error) {
	return f.after, f.err
}

// ListMessagesByIDs 返回预置的按 ID 消息。
func (f *fakeSummaryReader) ListMessagesByIDs(context.Context, uuid.UUID, uuid.UUID, []uuid.UUID) ([]biz.Message, error) {
	return f.byIDs, f.err
}

// CountMessagesThrough 返回预置计数。
func (f *fakeSummaryReader) CountMessagesThrough(context.Context, uuid.UUID, uuid.UUID, biz.MessagePosition) (int64, error) {
	return int64(len(f.byIDs)), f.err
}

// fakeSummaryRepository 是摘要版本仓储的内存实现。
type fakeSummaryRepository struct {
	active    biz.SessionSummary
	found     bool
	latest    int64
	saved     []biz.SessionSummary
	expected  []int64
	applied   bool
	saveErr   error
	invalidAt int
}

// GetActiveSummary 返回预置的生效摘要。
func (f *fakeSummaryRepository) GetActiveSummary(context.Context, uuid.UUID, uuid.UUID) (biz.SessionSummary, bool, error) {
	return f.active, f.found, nil
}

// LatestSummaryVersion 返回预置的最新版本。
func (f *fakeSummaryRepository) LatestSummaryVersion(context.Context, uuid.UUID, uuid.UUID) (int64, error) {
	return f.latest, nil
}

// SaveSummary 记录写入参数并返回预置结果。
func (f *fakeSummaryRepository) SaveSummary(_ context.Context, summary biz.SessionSummary, expected int64, _ biz.OutboxEvent) (biz.SessionSummary, bool, error) {
	f.saved = append(f.saved, summary)
	f.expected = append(f.expected, expected)
	return summary, f.applied, f.saveErr
}

// InvalidateActive 记录失效调用。
func (f *fakeSummaryRepository) InvalidateActive(context.Context, uuid.UUID, uuid.UUID, time.Time) error {
	f.invalidAt++
	return nil
}

// ListSummaries 返回已保存摘要。
func (f *fakeSummaryRepository) ListSummaries(context.Context, uuid.UUID, uuid.UUID, int) ([]biz.SessionSummary, error) {
	return f.saved, nil
}

// ListUserSummaries 返回已保存摘要，供导出测试。
func (f *fakeSummaryRepository) ListUserSummaries(context.Context, uuid.UUID, int) ([]biz.SessionSummary, error) {
	return f.saved, nil
}

// fakeSummaryQueue 是摘要任务入队端口的内存实现。
type fakeSummaryQueue struct {
	tasks []biz.SummaryTask
	err   error
}

// EnqueueSummary 记录入队任务。
func (f *fakeSummaryQueue) EnqueueSummary(_ context.Context, task biz.SummaryTask, _ time.Time) error {
	f.tasks = append(f.tasks, task)
	return f.err
}

// fakeSummaryGenerator 是摘要生成器的内存实现。
type fakeSummaryGenerator struct {
	generation biz.SummaryGeneration
	err        error
	previous   string
	messages   []biz.Message
	calls      int
	started    chan struct{}
	release    chan struct{}
}

// Summarize 记录调用参数并返回预置结果。
func (f *fakeSummaryGenerator) Summarize(_ context.Context, _, _ uuid.UUID, previous string, messages []biz.Message) (biz.SummaryGeneration, error) {
	f.calls++
	f.previous = previous
	f.messages = messages
	if f.started != nil {
		close(f.started)
	}
	if f.release != nil {
		<-f.release
	}
	return f.generation, f.err
}

// fakeSummaryJobs 是摘要 Worker 使用的任务队列内存实现。
type fakeSummaryJobs struct {
	job          biz.Job
	claimed      bool
	completed    int
	failed       int
	renewed      int
	released     int
	claimErr     error
	renewBlocks  bool
	renewStarted chan struct{}
}

// EnqueueRun 未被摘要 Worker 使用。
func (f *fakeSummaryJobs) EnqueueRun(context.Context, uuid.UUID, uuid.UUID, time.Time) error {
	return nil
}

// Claim 返回预置任务一次。
func (f *fakeSummaryJobs) Claim(context.Context, string, string, time.Duration, time.Time) (biz.Job, bool, error) {
	if f.claimErr != nil {
		return biz.Job{}, false, f.claimErr
	}
	if f.claimed {
		return biz.Job{}, false, nil
	}
	f.claimed = true
	return f.job, true, nil
}

// Renew 记录续约；renewBlocks 为真时模拟在途续约，直到上下文取消才返回。
func (f *fakeSummaryJobs) Renew(ctx context.Context, _ uuid.UUID, _ string, _ int64, _ time.Duration, _ time.Time) error {
	f.renewed++
	if f.renewBlocks {
		if f.renewStarted != nil {
			close(f.renewStarted)
		}
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

// Release 记录退回。
func (f *fakeSummaryJobs) Release(context.Context, uuid.UUID, string, int64, time.Time, time.Time) error {
	f.released++
	return nil
}

// Complete 记录完成。
func (f *fakeSummaryJobs) Complete(context.Context, uuid.UUID, string, int64) error {
	f.completed++
	return nil
}

// Fail 记录失败。
func (f *fakeSummaryJobs) Fail(context.Context, uuid.UUID, string, int64, time.Time, string, time.Time) error {
	f.failed++
	return nil
}

// summaryTestMessages 构造时间连续的消息序列。
func summaryTestMessages(count int, start time.Time) []biz.Message {
	messages := make([]biz.Message, 0, count)
	for index := 0; index < count; index++ {
		messages = append(messages, biz.Message{
			ID: uuid.New(), SessionID: uuid.New(), UserID: uuid.New(), Role: biz.MessageUser,
			Content: fmt.Sprintf("消息-%d", index), CreatedAt: start.Add(time.Duration(index) * time.Second),
		})
	}
	return messages
}

// TestSummaryPlannerEnqueuesWhenThresholdReached 验证达到阈值后按保留窗口与累计覆盖数入队。
func TestSummaryPlannerEnqueuesWhenThresholdReached(t *testing.T) {
	userID, sessionID := uuid.New(), uuid.New()
	messages := summaryTestMessages(4, time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC))
	reader := &fakeSummaryReader{recent: []biz.Message{messages[3], messages[2], messages[1], messages[0]}, after: messages[:2]}
	repository := &fakeSummaryRepository{latest: 0}
	queue := &fakeSummaryQueue{}
	planner, err := biz.NewSummaryPlanner(reader, repository, queue, biz.SummaryPlanConfig{TriggerMessages: 3, KeepRecent: 2}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := planner.MaybeEnqueue(context.Background(), userID, sessionID); err != nil {
		t.Fatal(err)
	}
	if len(queue.tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(queue.tasks))
	}
	task := queue.tasks[0]
	if task.BaseVersion != 0 || task.CoveredMessageCount != 2 || len(task.SourceMessageIDs) != 2 {
		t.Fatalf("task = %#v", task)
	}
	if task.EndMessageID != messages[1].ID || !task.EndAt.Equal(messages[1].CreatedAt) {
		t.Fatalf("end boundary = %s, want %s", task.EndMessageID, messages[1].ID)
	}
	if task.Fingerprint != biz.SummarySourceFingerprint(messages[:2]) {
		t.Fatal("fingerprint mismatch")
	}
}

// TestSummaryPlannerSkipsBelowThresholdOrCoveredBoundary 验证未达阈值与已覆盖边界时不入队。
func TestSummaryPlannerSkipsBelowThresholdOrCoveredBoundary(t *testing.T) {
	userID, sessionID := uuid.New(), uuid.New()
	messages := summaryTestMessages(2, time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC))
	reader := &fakeSummaryReader{recent: messages}
	queue := &fakeSummaryQueue{}
	planner, err := biz.NewSummaryPlanner(reader, &fakeSummaryRepository{}, queue, biz.SummaryPlanConfig{TriggerMessages: 3, KeepRecent: 2}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := planner.MaybeEnqueue(context.Background(), userID, sessionID); err != nil {
		t.Fatal(err)
	}
	if len(queue.tasks) != 0 {
		t.Fatal("below threshold must not enqueue")
	}

	// 生效摘要已覆盖到边界消息时，即使触发条件满足也不重复摘要。
	more := summaryTestMessages(2, messages[1].CreatedAt.Add(time.Second))
	reader.recent = []biz.Message{more[1], more[0], messages[1], messages[0]}
	reader.after = nil
	latestID := messages[1].ID
	latestAt := messages[1].CreatedAt
	repository := &fakeSummaryRepository{latest: 1, found: true, active: biz.SessionSummary{
		ID: uuid.New(), Version: 1, SourceEndMessageID: &latestID, SourceEndAt: &latestAt, CoveredMessageCount: 2,
	}}
	planner, err = biz.NewSummaryPlanner(reader, repository, queue, biz.SummaryPlanConfig{TriggerMessages: 3, KeepRecent: 2}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := planner.MaybeEnqueue(context.Background(), userID, sessionID); err != nil {
		t.Fatal(err)
	}
	if len(queue.tasks) != 0 {
		t.Fatal("covered boundary must not enqueue")
	}
}

// TestSummaryPlannerRejectsInvalidIdentity 验证缺失用户或会话 ID 时拒绝规划。
func TestSummaryPlannerRejectsInvalidIdentity(t *testing.T) {
	planner, err := biz.NewSummaryPlanner(&fakeSummaryReader{}, &fakeSummaryRepository{}, &fakeSummaryQueue{}, biz.SummaryPlanConfig{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := planner.MaybeEnqueue(context.Background(), uuid.Nil, uuid.New()); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("error = %v", err)
	}
	if _, err := biz.NewSummaryPlanner(nil, nil, nil, biz.SummaryPlanConfig{}, nil, nil); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("constructor error = %v", err)
	}
}

// TestSummaryWorkerStartAndClaimError 验证 Worker 循环可随上下文取消退出，并处理领取失败。
func TestSummaryWorkerStartAndClaimError(t *testing.T) {
	ctrl := gomock.NewController(t)
	job := biz.Job{ID: uuid.New(), Type: biz.SummaryJobType, Payload: map[string]any{}}
	worker, jobs, _, _, _ := newSummaryWorkerTest(t, ctrl, job)
	// 标记任务已被领取，循环只验证取消退出，不进入业务处理。
	jobs.claimed = true
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	worker.Start(ctx)

	claimJobs := &fakeSummaryJobs{claimErr: errors.New("queue unavailable")}
	sessions := mocks.NewMockSessionRepository(ctrl)
	sessions.EXPECT().GetSession(gomock.Any(), gomock.Any(), gomock.Any()).Return(biz.Session{}, nil).AnyTimes()
	worker, err := biz.NewSummaryWorker(biz.SummaryWorkerDependencies{
		Jobs: claimJobs, Sessions: sessions, Messages: &fakeSummaryReader{}, Summaries: &fakeSummaryRepository{},
		Generator: &fakeSummaryGenerator{},
	}, biz.SummaryWorkerConfig{WorkerID: "summary-test"})
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := worker.ProcessOne(context.Background()); err == nil || claimed {
		t.Fatalf("claim error: claimed=%v err=%v", claimed, err)
	}
}

// summaryJobPayload 构造摘要任务载荷。
func summaryJobPayload(task biz.SummaryTask) map[string]any {
	ids := make([]any, 0, len(task.SourceMessageIDs))
	for _, id := range task.SourceMessageIDs {
		ids = append(ids, id.String())
	}
	return map[string]any{
		"user_id": task.UserID.String(), "session_id": task.SessionID.String(),
		"base_version": task.BaseVersion, "source_message_ids": ids, "fingerprint": task.Fingerprint,
		"end_message_id": task.EndMessageID.String(), "end_at": task.EndAt.UTC().Format(time.RFC3339Nano),
		"covered_message_count": task.CoveredMessageCount,
	}
}

// newSummaryWorkerTest 构造带 Mock 会话仓储与内存摘要依赖的 Worker。
func newSummaryWorkerTest(t *testing.T, ctrl *gomock.Controller, job biz.Job) (*biz.SummaryWorker, *fakeSummaryJobs, *fakeSummaryRepository, *fakeSummaryReader, *fakeSummaryGenerator) {
	t.Helper()
	jobs := &fakeSummaryJobs{job: job}
	baseVersion := int64(0)
	switch typed := job.Payload["base_version"].(type) {
	case int64:
		baseVersion = typed
	case int:
		baseVersion = int64(typed)
	}
	repository := &fakeSummaryRepository{latest: baseVersion, applied: true}
	reader := &fakeSummaryReader{}
	generator := &fakeSummaryGenerator{generation: biz.SummaryGeneration{Text: "摘要正文"}}
	sessions := mocks.NewMockSessionRepository(ctrl)
	sessions.EXPECT().GetSession(gomock.Any(), gomock.Any(), gomock.Any()).Return(biz.Session{Status: biz.SessionActive}, nil).AnyTimes()
	worker, err := biz.NewSummaryWorker(biz.SummaryWorkerDependencies{
		Jobs: jobs, Sessions: sessions, Messages: reader, Summaries: repository, Generator: generator,
	}, biz.SummaryWorkerConfig{WorkerID: "summary-test", Heartbeat: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	return worker, jobs, repository, reader, generator
}

// TestSummaryWorkerCreatesVersion 验证 Worker 校验来源、生成摘要并写入新版本。
func TestSummaryWorkerCreatesVersion(t *testing.T) {
	ctrl := gomock.NewController(t)
	message := biz.Message{ID: uuid.New(), Role: biz.MessageUser, Content: "问题", CreatedAt: time.Date(2026, 11, 2, 1, 0, 0, 0, time.UTC)}
	task := biz.SummaryTask{
		UserID: uuid.New(), SessionID: uuid.New(), BaseVersion: 0,
		SourceMessageIDs: []uuid.UUID{message.ID}, Fingerprint: biz.SummarySourceFingerprint([]biz.Message{message}),
		EndMessageID: message.ID, EndAt: message.CreatedAt, CoveredMessageCount: 1,
	}
	job := biz.Job{ID: uuid.New(), Type: biz.SummaryJobType, Payload: summaryJobPayload(task), Attempts: 1, FencingToken: 1}
	worker, jobs, repository, reader, generator := newSummaryWorkerTest(t, ctrl, job)
	reader.byIDs = []biz.Message{message}
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if jobs.completed != 1 || jobs.failed != 0 {
		t.Fatalf("completed=%d failed=%d", jobs.completed, jobs.failed)
	}
	if generator.calls != 1 {
		t.Fatalf("generator calls = %d", generator.calls)
	}
	if len(repository.saved) != 1 || repository.expected[0] != 0 {
		t.Fatalf("saved = %#v expected = %#v", repository.saved, repository.expected)
	}
	saved := repository.saved[0]
	if saved.Version != 1 || saved.SourceEndMessageID == nil || *saved.SourceEndMessageID != message.ID {
		t.Fatalf("saved summary = %#v", saved)
	}
}

// TestSummaryWorkerIgnoresCanceledRenewAfterGeneration 验证主流程结束时的主动取消不会被误判为丢租约。
func TestSummaryWorkerIgnoresCanceledRenewAfterGeneration(t *testing.T) {
	ctrl := gomock.NewController(t)
	message := biz.Message{ID: uuid.New(), Role: biz.MessageUser, Content: "问题", CreatedAt: time.Now().UTC()}
	task := biz.SummaryTask{
		UserID: uuid.New(), SessionID: uuid.New(),
		SourceMessageIDs: []uuid.UUID{message.ID}, Fingerprint: biz.SummarySourceFingerprint([]biz.Message{message}),
		EndMessageID: message.ID, EndAt: message.CreatedAt, CoveredMessageCount: 1,
	}
	job := biz.Job{ID: uuid.New(), Type: biz.SummaryJobType, Payload: summaryJobPayload(task), Attempts: 1, FencingToken: 1}
	renewStarted, generatorStarted, generatorRelease := make(chan struct{}), make(chan struct{}), make(chan struct{})
	jobs := &fakeSummaryJobs{job: job, renewBlocks: true, renewStarted: renewStarted}
	repository := &fakeSummaryRepository{latest: 0, applied: true}
	reader := &fakeSummaryReader{byIDs: []biz.Message{message}}
	generator := &fakeSummaryGenerator{
		generation: biz.SummaryGeneration{Text: "摘要正文"}, started: generatorStarted, release: generatorRelease,
	}
	sessions := mocks.NewMockSessionRepository(ctrl)
	sessions.EXPECT().GetSession(gomock.Any(), gomock.Any(), gomock.Any()).Return(biz.Session{Status: biz.SessionActive}, nil).AnyTimes()
	worker, err := biz.NewSummaryWorker(biz.SummaryWorkerDependencies{
		Jobs: jobs, Sessions: sessions, Messages: reader, Summaries: repository, Generator: generator,
	}, biz.SummaryWorkerConfig{WorkerID: "summary-lease-test", Heartbeat: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := worker.ProcessOne(context.Background()); err != nil {
			t.Errorf("process summary: %v", err)
		}
	}()
	waitSignal := func(signal chan struct{}, name string) {
		t.Helper()
		select {
		case <-signal:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s did not start", name)
		}
	}
	waitSignal(generatorStarted, "summary generation")
	// 等待心跳进入在途续约，再让生成返回并触发主流程 cancel。
	waitSignal(renewStarted, "lease renewal")
	close(generatorRelease)
	waitSignal(done, "summary worker")
	if jobs.completed != 1 || len(repository.saved) != 1 {
		t.Fatalf("completed=%d saved=%d", jobs.completed, len(repository.saved))
	}
}

// TestSummaryWorkerSkipsStaleAndInvalidSource 验证过期版本与来源缺失时不调用模型且幂等完成。
func TestSummaryWorkerSkipsStaleAndInvalidSource(t *testing.T) {
	ctrl := gomock.NewController(t)
	message := biz.Message{ID: uuid.New(), Role: biz.MessageUser, Content: "问题", CreatedAt: time.Now().UTC()}
	task := biz.SummaryTask{
		UserID: uuid.New(), SessionID: uuid.New(), BaseVersion: 1,
		SourceMessageIDs: []uuid.UUID{message.ID}, Fingerprint: "bad",
		EndMessageID: message.ID, EndAt: message.CreatedAt, CoveredMessageCount: 2,
	}
	job := biz.Job{ID: uuid.New(), Type: biz.SummaryJobType, Payload: summaryJobPayload(task), Attempts: 1, FencingToken: 1}
	worker, jobs, repository, reader, generator := newSummaryWorkerTest(t, ctrl, job)
	// 最新版本已前进到 2，任务必须直接完成。
	repository.latest = 2
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if jobs.completed != 1 || generator.calls != 0 {
		t.Fatalf("stale task: completed=%d generation=%d", jobs.completed, generator.calls)
	}

	// 重置任务队列并验证来源指纹不匹配时同样直接完成。
	jobs.claimed = false
	repository.latest = 1
	reader.byIDs = []biz.Message{message}
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if jobs.completed != 2 || generator.calls != 0 {
		t.Fatalf("invalid source: completed=%d generation=%d", jobs.completed, generator.calls)
	}
}

// TestSummaryWorkerClassifiesErrors 验证不可重试错误完成、可重试错误退避失败。
func TestSummaryWorkerClassifiesErrors(t *testing.T) {
	ctrl := gomock.NewController(t)
	message := biz.Message{ID: uuid.New(), Role: biz.MessageUser, Content: "问题", CreatedAt: time.Now().UTC()}
	task := biz.SummaryTask{
		UserID: uuid.New(), SessionID: uuid.New(), BaseVersion: 0,
		SourceMessageIDs: []uuid.UUID{message.ID}, Fingerprint: biz.SummarySourceFingerprint([]biz.Message{message}),
		EndMessageID: message.ID, EndAt: message.CreatedAt, CoveredMessageCount: 1,
	}
	job := biz.Job{ID: uuid.New(), Type: biz.SummaryJobType, Payload: summaryJobPayload(task), Attempts: 1, FencingToken: 1}
	worker, jobs, _, reader, generator := newSummaryWorkerTest(t, ctrl, job)
	reader.byIDs = []biz.Message{message}
	generator.err = biz.ErrSummaryModelUnavailable
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if jobs.completed != 1 || jobs.failed != 0 {
		t.Fatalf("unavailable: completed=%d failed=%d", jobs.completed, jobs.failed)
	}

	jobs.claimed = false
	generator.err = errors.New("provider timeout")
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if jobs.failed != 1 || jobs.completed != 1 {
		t.Fatalf("retryable: completed=%d failed=%d", jobs.completed, jobs.failed)
	}
}

// TestSummaryWorkerRejectsInvalidPayload 验证非法载荷直接完成，避免死信堆积。
func TestSummaryWorkerRejectsInvalidPayload(t *testing.T) {
	ctrl := gomock.NewController(t)
	job := biz.Job{ID: uuid.New(), Type: biz.SummaryJobType, Payload: map[string]any{"user_id": "bad"}, Attempts: 1}
	worker, jobs, _, _, generator := newSummaryWorkerTest(t, ctrl, job)
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if jobs.completed != 1 || generator.calls != 0 {
		t.Fatalf("completed=%d generation=%d", jobs.completed, generator.calls)
	}
}
