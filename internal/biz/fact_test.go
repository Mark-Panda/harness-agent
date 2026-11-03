package biz_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
)

// fakeFactRepository 是结构化事实仓储的内存实现。
type fakeFactRepository struct {
	active  map[string]biz.MemoryFact
	saved   []biz.MemoryFact
	applied bool
	err     error
	deleted []string
}

// ListActiveFacts 返回内存中的生效事实。
func (f *fakeFactRepository) ListActiveFacts(context.Context, uuid.UUID, int) ([]biz.MemoryFact, error) {
	facts := make([]biz.MemoryFact, 0, len(f.active))
	for _, fact := range f.active {
		facts = append(facts, fact)
	}
	return facts, f.err
}

// GetActiveFact 按 key 返回生效事实。
func (f *fakeFactRepository) GetActiveFact(_ context.Context, _ uuid.UUID, key string) (biz.MemoryFact, bool, error) {
	if f.err != nil {
		return biz.MemoryFact{}, false, f.err
	}
	fact, exists := f.active[key]
	return fact, exists, nil
}

// SaveFact 记录写入并更新内存状态。
func (f *fakeFactRepository) SaveFact(_ context.Context, fact biz.MemoryFact, _ int64) (biz.MemoryFact, bool, error) {
	if f.err != nil {
		return biz.MemoryFact{}, false, f.err
	}
	f.saved = append(f.saved, fact)
	if f.active == nil {
		f.active = map[string]biz.MemoryFact{}
	}
	f.active[fact.Key] = fact
	return fact, f.applied, nil
}

// ListFactVersions 返回已保存版本。
func (f *fakeFactRepository) ListFactVersions(context.Context, uuid.UUID, string, int) ([]biz.MemoryFact, error) {
	return f.saved, nil
}

// ListFacts 返回已保存事实，供导出测试。
func (f *fakeFactRepository) ListFacts(context.Context, uuid.UUID, int) ([]biz.MemoryFact, error) {
	return f.saved, nil
}

// DeleteFact 记录删除。
func (f *fakeFactRepository) DeleteFact(_ context.Context, _ uuid.UUID, key string, _ time.Time) error {
	f.deleted = append(f.deleted, key)
	delete(f.active, key)
	return f.err
}

// factCandidate 构造事实候选。
func factCandidate(key, value string, confidence float64, correction bool) biz.FactCandidate {
	return biz.FactCandidate{
		Key: key, Value: json.RawMessage(value), Confidence: confidence, Correction: correction,
		SourceKind: biz.FactSourceUser,
	}
}

// TestFactServiceConflictPolicy 验证低置信度与显式纠正保护规则。
func TestFactServiceConflictPolicy(t *testing.T) {
	repo := &fakeFactRepository{applied: true, active: map[string]biz.MemoryFact{}}
	service, err := biz.NewFactService(repo, biz.FactPolicy{MinConfidence: 0.6, CorrectionGuard: 0.9}, nil)
	if err != nil {
		t.Fatal(err)
	}
	userID, sessionID := uuid.New(), uuid.New()

	// 低置信度候选直接跳过。
	result, err := service.Apply(context.Background(), userID, sessionID, []biz.FactCandidate{
		factCandidate("preference.language", `"中文"`, 0.4, false),
	})
	if err != nil || result.Applied != 0 || result.Skipped != 1 {
		t.Fatalf("low confidence result = %#v err=%v", result, err)
	}

	if _, err := service.Apply(context.Background(), userID, sessionID, []biz.FactCandidate{
		factCandidate("preference.language", `"中文"`, 0.9, false),
	}); err != nil {
		t.Fatal(err)
	}
	// 相同值不产生新版本。
	result, err = service.Apply(context.Background(), userID, sessionID, []biz.FactCandidate{
		factCandidate("preference.language", `"中文"`, 0.95, false),
	})
	if err != nil || result.Skipped != 1 {
		t.Fatalf("same value result = %#v err=%v", result, err)
	}
	// 低置信度候选不得覆盖已有高置信度事实。
	result, err = service.Apply(context.Background(), userID, sessionID, []biz.FactCandidate{
		factCandidate("preference.language", `"英文"`, 0.7, false),
	})
	if err != nil || result.Skipped != 1 {
		t.Fatalf("lower confidence result = %#v err=%v", result, err)
	}
	// 高置信度候选可以更新。
	result, err = service.Apply(context.Background(), userID, sessionID, []biz.FactCandidate{
		factCandidate("preference.language", `"英文"`, 0.98, false),
	})
	if err != nil || result.Applied != 1 || len(repo.saved) != 2 {
		t.Fatalf("higher confidence result = %#v saved=%d err=%v", result, len(repo.saved), err)
	}

	// 用户显式纠正后，低置信度非纠正候选不得静默覆盖。
	repo.active["preference.language"] = biz.MemoryFact{
		Key: "preference.language", Value: json.RawMessage(`"英文"`), Confidence: 0.98,
		Corrected: true, Version: 2, ValidFrom: time.Now().UTC(),
	}
	result, err = service.Apply(context.Background(), userID, sessionID, []biz.FactCandidate{
		factCandidate("preference.language", `"中文"`, 0.7, false),
	})
	if err != nil || result.Skipped != 1 {
		t.Fatalf("correction guard result = %#v err=%v", result, err)
	}
	// 新的显式纠正允许覆盖。
	result, err = service.Apply(context.Background(), userID, sessionID, []biz.FactCandidate{
		factCandidate("preference.language", `"中文"`, 0.7, true),
	})
	if err != nil || result.Applied != 1 {
		t.Fatalf("explicit correction result = %#v err=%v", result, err)
	}
}

// TestFactServiceValidationAndDeletion 验证非法值被拒绝、超过上限被截断与删除语义。
func TestFactServiceValidationAndDeletion(t *testing.T) {
	repo := &fakeFactRepository{applied: true, active: map[string]biz.MemoryFact{}}
	service, err := biz.NewFactService(repo, biz.FactPolicy{MinConfidence: 0.6}, nil)
	if err != nil {
		t.Fatal(err)
	}
	userID := uuid.New()
	candidates := []biz.FactCandidate{
		{Key: "", Value: json.RawMessage(`"x"`), Confidence: 0.9},
		{Key: "bad", Value: json.RawMessage(`not-json`), Confidence: 0.9},
	}
	result, err := service.Apply(context.Background(), userID, uuid.New(), candidates)
	if err != nil || result.Skipped != 2 {
		t.Fatalf("invalid result = %#v err=%v", result, err)
	}
	if err := service.Delete(context.Background(), userID, "preference.language"); err != nil || len(repo.deleted) != 1 {
		t.Fatalf("delete err=%v deleted=%v", err, repo.deleted)
	}
	if err := service.Delete(context.Background(), uuid.Nil, "x"); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("delete identity error = %v", err)
	}
	if _, err := biz.NewFactService(nil, biz.FactPolicy{}, nil); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("constructor error = %v", err)
	}

	// 单用户事实数达到上限后拒绝新键，已存在的键仍可更新。
	limitedRepo := &fakeFactRepository{applied: true, active: map[string]biz.MemoryFact{
		"preference.language": {Key: "preference.language", Value: json.RawMessage(`"中文"`), Confidence: 0.9, Version: 1},
	}}
	limited, err := biz.NewFactService(limitedRepo, biz.FactPolicy{MinConfidence: 0.6, MaxFactsPerUser: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err = limited.Apply(context.Background(), userID, uuid.New(), []biz.FactCandidate{
		factCandidate("profile.timezone", `"Asia/Shanghai"`, 0.9, false),
	})
	if err != nil || result.Skipped != 1 {
		t.Fatalf("limit result = %#v err=%v", result, err)
	}
}

// fakeFactExtractor 是事实提取器的测试替身。
type fakeFactExtractor struct {
	candidates []biz.FactCandidate
	err        error
	calls      int
}

// Extract 记录调用并返回预置候选。
func (f *fakeFactExtractor) Extract(context.Context, uuid.UUID, uuid.UUID, []biz.Message) ([]biz.FactCandidate, error) {
	f.calls++
	return f.candidates, f.err
}

// factJobPayload 构造事实任务载荷。
func factJobPayload(task biz.FactTask) map[string]any {
	ids := make([]any, 0, len(task.MessageIDs))
	for _, id := range task.MessageIDs {
		ids = append(ids, id.String())
	}
	return map[string]any{
		"user_id": task.UserID.String(), "session_id": task.SessionID.String(),
		"message_ids": ids, "fingerprint": task.Fingerprint,
	}
}

// factJobPayloadWithOwner 在基础载荷上追加群聊发言人事实归属字段。
func factJobPayloadWithOwner(task biz.FactTask) map[string]any {
	payload := factJobPayload(task)
	if task.FactOwnerID != uuid.Nil {
		payload["fact_owner_id"] = task.FactOwnerID.String()
	}
	if task.SpeakerOpenID != "" {
		payload["speaker_open_id"] = task.SpeakerOpenID
	}
	return payload
}

// TestFactExtractionWorkerSuccess 验证来源校验、提取与写入链路。
func TestFactExtractionWorkerSuccess(t *testing.T) {
	message := biz.Message{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), Role: biz.MessageUser, Content: "我偏好中文", CreatedAt: time.Now().UTC()}
	task := biz.FactTask{
		UserID: message.UserID, SessionID: message.SessionID, MessageIDs: []uuid.UUID{message.ID},
		Fingerprint: biz.SummarySourceFingerprint([]biz.Message{message}),
	}
	job := biz.Job{ID: uuid.New(), Type: biz.FactExtractJobType, Payload: factJobPayload(task), Attempts: 1, FencingToken: 1}
	jobs := &fakeSummaryJobs{job: job}
	reader := &fakeSummaryReader{byIDs: []biz.Message{message}}
	repo := &fakeFactRepository{applied: true, active: map[string]biz.MemoryFact{}}
	factService, err := biz.NewFactService(repo, biz.FactPolicy{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	extractor := &fakeFactExtractor{candidates: []biz.FactCandidate{factCandidate("preference.language", `"中文"`, 0.9, false)}}
	worker, err := biz.NewFactExtractionWorker(biz.FactExtractionWorkerDependencies{
		Jobs: jobs, Messages: reader, Facts: factService, Extractor: extractor,
	}, biz.FactExtractionWorkerConfig{WorkerID: "fact-test", Heartbeat: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if jobs.completed != 1 || extractor.calls != 1 || len(repo.saved) != 1 {
		t.Fatalf("completed=%d calls=%d saved=%d", jobs.completed, extractor.calls, len(repo.saved))
	}
	if !repo.saved[0].Corrected && repo.saved[0].SourceKind != biz.FactSourceUser {
		t.Fatalf("saved fact = %#v", repo.saved[0])
	}
}

// TestFactExtractionWorkerSkipsStaleSource 验证来源变化与不可重试错误直接完成。
func TestFactExtractionWorkerSkipsStaleSource(t *testing.T) {
	message := biz.Message{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), Role: biz.MessageUser, Content: "我偏好中文", CreatedAt: time.Now().UTC()}
	task := biz.FactTask{UserID: message.UserID, SessionID: message.SessionID, MessageIDs: []uuid.UUID{message.ID}, Fingerprint: strings.Repeat("0", 64)}
	job := biz.Job{ID: uuid.New(), Type: biz.FactExtractJobType, Payload: factJobPayload(task), Attempts: 1, FencingToken: 1}
	jobs := &fakeSummaryJobs{job: job}
	reader := &fakeSummaryReader{byIDs: []biz.Message{message}}
	repo := &fakeFactRepository{applied: true}
	factService, err := biz.NewFactService(repo, biz.FactPolicy{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	extractor := &fakeFactExtractor{}
	worker, err := biz.NewFactExtractionWorker(biz.FactExtractionWorkerDependencies{
		Jobs: jobs, Messages: reader, Facts: factService, Extractor: extractor,
	}, biz.FactExtractionWorkerConfig{WorkerID: "fact-test", Heartbeat: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if jobs.completed != 1 || extractor.calls != 0 {
		t.Fatalf("stale completed=%d calls=%d", jobs.completed, extractor.calls)
	}

	// 不可重试模型错误直接完成。
	jobs.claimed = false
	task.Fingerprint = biz.SummarySourceFingerprint([]biz.Message{message})
	job.Payload = factJobPayload(task)
	jobs.job = job
	extractor.err = biz.ErrFactExtractionUnavailable
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if jobs.completed != 2 || jobs.failed != 0 {
		t.Fatalf("unavailable completed=%d failed=%d", jobs.completed, jobs.failed)
	}

	// 可重试错误进入退避。
	jobs.claimed = false
	extractor.err = errors.New("network error")
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if jobs.failed != 1 {
		t.Fatalf("retryable failed=%d", jobs.failed)
	}
}

// fakeFactQueue 是事实任务入队端口的内存实现。
type fakeFactQueue struct {
	tasks []biz.FactTask
}

// EnqueueFacts 记录入队任务。
func (f *fakeFactQueue) EnqueueFacts(_ context.Context, task biz.FactTask, _ time.Time) error {
	f.tasks = append(f.tasks, task)
	return nil
}

// TestFactIndexerThresholdAndPayload 验证消息不足不触发、足够时按时间顺序入队。
func TestFactIndexerThresholdAndPayload(t *testing.T) {
	userID, sessionID := uuid.New(), uuid.New()
	messages := summaryTestMessages(4, time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC))
	for index := range messages {
		if index%2 == 1 {
			messages[index].Role = biz.MessageAssistant
		}
	}
	reader := &fakeSummaryReader{recent: []biz.Message{messages[3], messages[2], messages[1], messages[0]}}
	queue := &fakeFactQueue{}
	indexer, err := biz.NewFactIndexer(reader, queue, biz.FactIndexerConfig{RecentMessages: 4, MinMessages: 4}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := indexer.MaybeEnqueue(context.Background(), userID, sessionID); err != nil {
		t.Fatal(err)
	}
	if len(queue.tasks) != 1 || len(queue.tasks[0].MessageIDs) != 4 {
		t.Fatalf("tasks = %#v", queue.tasks)
	}
	if queue.tasks[0].MessageIDs[0] != messages[0].ID {
		t.Fatalf("message order was not restored: %#v", queue.tasks[0].MessageIDs)
	}

	// 消息不足时不触发。
	queue.tasks = nil
	reader.recent = messages[:2]
	indexer, err = biz.NewFactIndexer(reader, queue, biz.FactIndexerConfig{RecentMessages: 4, MinMessages: 4}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := indexer.MaybeEnqueue(context.Background(), userID, sessionID); err != nil {
		t.Fatal(err)
	}
	if len(queue.tasks) != 0 {
		t.Fatalf("below threshold tasks = %#v", queue.tasks)
	}
	if err := indexer.MaybeEnqueue(context.Background(), uuid.Nil, sessionID); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("identity error = %v", err)
	}
}

// TestFactIndexerSpeakerFiltering 验证群聊只提取发言人消息、恢复正序并传递事实归属。
func TestFactIndexerSpeakerFiltering(t *testing.T) {
	ownerID, speakerID, sessionID := uuid.New(), uuid.New(), uuid.New()
	base := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	messages := []biz.Message{
		{ID: uuid.New(), UserID: ownerID, SessionID: sessionID, Role: biz.MessageUser, SpeakerOpenID: "ou_a", Content: "我喜欢咖啡", CreatedAt: base},
		{ID: uuid.New(), UserID: ownerID, SessionID: sessionID, Role: biz.MessageAssistant, Content: "收到", CreatedAt: base.Add(time.Minute)},
		{ID: uuid.New(), UserID: ownerID, SessionID: sessionID, Role: biz.MessageUser, SpeakerOpenID: "ou_b", Content: "他喜欢茶", CreatedAt: base.Add(2 * time.Minute)},
		{ID: uuid.New(), UserID: ownerID, SessionID: sessionID, Role: biz.MessageUser, SpeakerOpenID: "ou_a", Content: "预算 500", CreatedAt: base.Add(3 * time.Minute)},
	}
	// ListRecentMessages 按时间倒序返回，Indexer 必须恢复正序并对齐 Worker 的读取顺序。
	reader := &fakeSummaryReader{recent: []biz.Message{messages[3], messages[2], messages[1], messages[0]}}
	queue := &fakeFactQueue{}
	indexer, err := biz.NewFactIndexer(reader, queue, biz.FactIndexerConfig{RecentMessages: 4, MinMessages: 2}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := indexer.MaybeEnqueueSpeaker(context.Background(), ownerID, sessionID, speakerID, "ou_a"); err != nil {
		t.Fatal(err)
	}
	if len(queue.tasks) != 1 {
		t.Fatalf("speaker tasks = %#v", queue.tasks)
	}
	task := queue.tasks[0]
	if task.UserID != ownerID || task.FactOwnerID != speakerID || task.SpeakerOpenID != "ou_a" || task.SessionID != sessionID {
		t.Fatalf("speaker task identity = %#v", task)
	}
	selected := []biz.Message{messages[0], messages[3]}
	if len(task.MessageIDs) != len(selected) || task.MessageIDs[0] != selected[0].ID || task.MessageIDs[1] != selected[1].ID {
		t.Fatalf("speaker message ids = %#v", task.MessageIDs)
	}
	if task.Fingerprint != biz.SummarySourceFingerprint(selected) {
		t.Fatalf("speaker fingerprint mismatch: %q", task.Fingerprint)
	}
	// 非法入参直接拒绝，不得入队。
	if err := indexer.MaybeEnqueueSpeaker(context.Background(), ownerID, sessionID, uuid.Nil, "ou_a"); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("nil fact owner error = %v", err)
	}
	if err := indexer.MaybeEnqueueSpeaker(context.Background(), ownerID, sessionID, speakerID, "  "); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("blank speaker error = %v", err)
	}
}

// TestFactExtractionWorkerWritesSpeakerOwnership 验证群聊事实写入发言人私有记忆并保留发言人过滤键。
func TestFactExtractionWorkerWritesSpeakerOwnership(t *testing.T) {
	ownerID, speakerID, sessionID := uuid.New(), uuid.New(), uuid.New()
	message := biz.Message{
		ID: uuid.New(), UserID: ownerID, SessionID: sessionID, Role: biz.MessageUser,
		SpeakerOpenID: "ou_a", Content: "我喜欢咖啡", CreatedAt: time.Now().UTC(),
	}
	task := biz.FactTask{
		UserID: ownerID, FactOwnerID: speakerID, SpeakerOpenID: "ou_a", SessionID: sessionID,
		MessageIDs: []uuid.UUID{message.ID}, Fingerprint: biz.SummarySourceFingerprint([]biz.Message{message}),
	}
	job := biz.Job{ID: uuid.New(), Type: biz.FactExtractJobType, Payload: factJobPayloadWithOwner(task), Attempts: 1, FencingToken: 1}
	jobs := &fakeSummaryJobs{job: job}
	reader := &fakeSummaryReader{byIDs: []biz.Message{message}}
	repo := &fakeFactRepository{applied: true, active: map[string]biz.MemoryFact{}}
	factService, err := biz.NewFactService(repo, biz.FactPolicy{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	extractor := &fakeFactExtractor{candidates: []biz.FactCandidate{factCandidate("preference.drink", `"咖啡"`, 0.9, false)}}
	worker, err := biz.NewFactExtractionWorker(biz.FactExtractionWorkerDependencies{
		Jobs: jobs, Messages: reader, Facts: factService, Extractor: extractor,
	}, biz.FactExtractionWorkerConfig{WorkerID: "fact-test", Heartbeat: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if jobs.completed != 1 || len(repo.saved) != 1 {
		t.Fatalf("completed=%d saved=%d", jobs.completed, len(repo.saved))
	}
	// 事实归属必须切换到发言人本人，而非会话所有者。
	if repo.saved[0].UserID != speakerID {
		t.Fatalf("fact owner = %s, want %s", repo.saved[0].UserID, speakerID)
	}
	// 非法 fact_owner_id 视为不可解析任务，直接完成而不是写错用户。
	jobs.claimed = false
	job.Payload["fact_owner_id"] = "not-a-uuid"
	jobs.job = job
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if jobs.completed != 2 || jobs.failed != 0 || len(repo.saved) != 1 {
		t.Fatalf("invalid owner completed=%d failed=%d saved=%d", jobs.completed, jobs.failed, len(repo.saved))
	}
}
