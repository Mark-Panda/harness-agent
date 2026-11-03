package biz_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
)

// fakeMemoryIndexes 是索引与维护统计端口的测试替身。
type fakeMemoryIndexes struct {
	indexes    []biz.EmbeddingIndex
	created    []biz.EmbeddingIndex
	activated  []string
	dualWrites []bool
	vectors    map[string]int64
	eligible   int64
	unembedded []biz.Message
	err        error
}

// ListIndexes 返回预置索引列表。
func (f *fakeMemoryIndexes) ListIndexes(context.Context) ([]biz.EmbeddingIndex, error) {
	return f.indexes, f.err
}

// GetIndex 按版本返回索引。
func (f *fakeMemoryIndexes) GetIndex(_ context.Context, version string) (biz.EmbeddingIndex, error) {
	if f.err != nil {
		return biz.EmbeddingIndex{}, f.err
	}
	for _, index := range f.indexes {
		if index.Version == version {
			return index, nil
		}
	}
	return biz.EmbeddingIndex{}, biz.ErrResourceNotFound
}

// CreateIndex 记录并追加索引。
func (f *fakeMemoryIndexes) CreateIndex(_ context.Context, index biz.EmbeddingIndex) (biz.EmbeddingIndex, error) {
	if f.err != nil {
		return biz.EmbeddingIndex{}, f.err
	}
	f.created = append(f.created, index)
	f.indexes = append(f.indexes, index)
	return index, nil
}

// ActivateIndex 记录切换并更新内存状态。
func (f *fakeMemoryIndexes) ActivateIndex(_ context.Context, version string) (biz.EmbeddingIndex, error) {
	if f.err != nil {
		return biz.EmbeddingIndex{}, f.err
	}
	f.activated = append(f.activated, version)
	for position := range f.indexes {
		if f.indexes[position].Version == version {
			f.indexes[position].Status = biz.EmbeddingIndexActive
			return f.indexes[position], nil
		}
	}
	return biz.EmbeddingIndex{}, biz.ErrResourceNotFound
}

// SetDualWrite 记录双写开关。
func (f *fakeMemoryIndexes) SetDualWrite(_ context.Context, _ string, enabled bool) error {
	f.dualWrites = append(f.dualWrites, enabled)
	return f.err
}

// CountVectors 返回预置向量数。
func (f *fakeMemoryIndexes) CountVectors(_ context.Context, version string) (int64, error) {
	return f.vectors[version], f.err
}

// CountEligibleMessages 返回预置候选消息数。
func (f *fakeMemoryIndexes) CountEligibleMessages(context.Context) (int64, error) {
	return f.eligible, f.err
}

// ListUnembeddedMessages 返回预置缺失消息。
func (f *fakeMemoryIndexes) ListUnembeddedMessages(context.Context, string, int) ([]biz.Message, error) {
	return f.unembedded, f.err
}

// fakeMemoryVectors 是用户向量端口的测试替身。
type fakeMemoryVectors struct {
	matches []biz.MemoryMatch
	hashes  map[uuid.UUID]string
	upserts [][]biz.MemoryVector
	err     error
}

// UpsertVectors 记录写入向量。
func (f *fakeMemoryVectors) UpsertVectors(_ context.Context, _ uuid.UUID, vectors []biz.MemoryVector) error {
	if f.err != nil {
		return f.err
	}
	f.upserts = append(f.upserts, vectors)
	return nil
}

// SearchVectors 返回预置检索结果。
func (f *fakeMemoryVectors) SearchVectors(context.Context, uuid.UUID, []float32, string, int) ([]biz.MemoryMatch, error) {
	return f.matches, f.err
}

// ListSourceHashes 返回预置内容哈希。
func (f *fakeMemoryVectors) ListSourceHashes(context.Context, uuid.UUID, string, []uuid.UUID) (map[uuid.UUID]string, error) {
	return f.hashes, f.err
}

// DeleteVectorsForSources 记录删除。
func (f *fakeMemoryVectors) DeleteVectorsForSources(context.Context, uuid.UUID, []uuid.UUID, time.Time) error {
	return f.err
}

// ListVectors 返回空列表，供导出测试使用独立替身。
func (f *fakeMemoryVectors) ListVectors(context.Context, uuid.UUID, *uuid.UUID, int) ([]biz.MemoryVector, error) {
	return nil, f.err
}

// fakeMemoryQueue 是向量化任务入队端口的内存实现。
type fakeMemoryQueue struct {
	tasks []biz.MemoryTask
	err   error
}

// EnqueueMemory 记录入队任务。
func (f *fakeMemoryQueue) EnqueueMemory(_ context.Context, task biz.MemoryTask, _ time.Time) error {
	f.tasks = append(f.tasks, task)
	return f.err
}

// fakeEmbeddingGenerator 是 embedding 生成器的测试替身。
type fakeEmbeddingGenerator struct {
	vectors    [][]float32
	inputToken int
	err        error
	calls      int
	inputs     []string
}

// Embed 记录输入并返回预置向量。
func (f *fakeEmbeddingGenerator) Embed(_ context.Context, index biz.EmbeddingIndex, inputs []string) (biz.EmbeddingResult, error) {
	f.calls++
	f.inputs = inputs
	if f.err != nil {
		return biz.EmbeddingResult{}, f.err
	}
	if f.vectors != nil {
		return biz.EmbeddingResult{Vectors: f.vectors, InputTokens: f.inputToken}, nil
	}
	vectors := make([][]float32, 0, len(inputs))
	for range inputs {
		vectors = append(vectors, makeEmbedding(index.Dimension, 0.1))
	}
	return biz.EmbeddingResult{Vectors: vectors, InputTokens: f.inputToken}, nil
}

// makeEmbedding 生成指定维度的向量。
func makeEmbedding(dimension int, value float32) []float32 {
	vector := make([]float32, dimension)
	for index := range vector {
		vector[index] = value
	}
	return vector
}

// activeMemoryIndex 构造 active 索引。
func activeMemoryIndex() biz.EmbeddingIndex {
	return biz.EmbeddingIndex{
		ID: uuid.New(), Name: "active", ProviderID: uuid.New(), Model: "embedding",
		Dimension: biz.EmbeddingDimension, Version: "v1", Status: biz.EmbeddingIndexActive, DualWrite: true,
	}
}

// TestMemoryRetrieverMMRSemantics 验证 Top-K 结果经 MMR 选择并过滤低相似度。
func TestMemoryRetrieverMMRSemantics(t *testing.T) {
	index := activeMemoryIndex()
	indexes := &fakeMemoryIndexes{indexes: []biz.EmbeddingIndex{index}}
	vectors := &fakeMemoryVectors{matches: []biz.MemoryMatch{
		{VectorID: uuid.New(), Content: "高相关一", Score: 0.95, Vector: []float32{1, 0}},
		{VectorID: uuid.New(), Content: "高相关二", Score: 0.94, Vector: []float32{0.99, 0.05}},
		{VectorID: uuid.New(), Content: "低相关", Score: 0.1, Vector: []float32{0, 1}},
	}}
	generator := &fakeEmbeddingGenerator{}
	retriever, err := biz.NewMemoryRetriever(indexes, vectors, generator, biz.MemoryRetrieverConfig{
		TopK: 30, MMRTop: 2, Lambda: 0.5, MinSimilarity: 0.2,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	matches, err := retriever.Retrieve(context.Background(), uuid.New(), uuid.New(), "查询")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 {
		t.Fatalf("matches = %#v", matches)
	}
	if generator.calls != 1 || len(generator.inputs) != 1 || generator.inputs[0] != "查询" {
		t.Fatalf("embedding calls = %d inputs=%v", generator.calls, generator.inputs)
	}
}

// TestMemoryRetrieverSkipsWithoutActiveIndex 验证无 active 索引或空查询时不调用模型。
func TestMemoryRetrieverSkipsWithoutActiveIndex(t *testing.T) {
	generator := &fakeEmbeddingGenerator{}
	retriever, err := biz.NewMemoryRetriever(&fakeMemoryIndexes{indexes: []biz.EmbeddingIndex{{
		Version: "staging", Status: biz.EmbeddingIndexStaging, Dimension: biz.EmbeddingDimension,
	}}}, &fakeMemoryVectors{}, generator, biz.MemoryRetrieverConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	matches, err := retriever.Retrieve(context.Background(), uuid.New(), uuid.New(), "查询")
	if err != nil || matches != nil || generator.calls != 0 {
		t.Fatalf("no active: matches=%#v err=%v calls=%d", matches, err, generator.calls)
	}
	if _, err := retriever.Retrieve(context.Background(), uuid.Nil, uuid.New(), "查询"); err != nil {
		t.Fatalf("nil user error = %v", err)
	}
	if _, err := biz.NewMemoryRetriever(nil, nil, nil, biz.MemoryRetrieverConfig{}, nil); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("constructor error = %v", err)
	}
}

// TestMemoryIndexerUsesDualWriteVersions 验证 active 与开启双写的 staging 都进入任务。
func TestMemoryIndexerUsesDualWriteVersions(t *testing.T) {
	userID, sessionID := uuid.New(), uuid.New()
	indexes := &fakeMemoryIndexes{indexes: []biz.EmbeddingIndex{
		activeMemoryIndex(),
		{ID: uuid.New(), Version: "v2", Status: biz.EmbeddingIndexStaging, DualWrite: true, Dimension: biz.EmbeddingDimension},
		{ID: uuid.New(), Version: "v3", Status: biz.EmbeddingIndexStaging, DualWrite: false, Dimension: biz.EmbeddingDimension},
	}}
	reader := &fakeSummaryReader{recent: []biz.Message{
		{ID: uuid.New(), Role: biz.MessageUser, Content: "问题"},
		{ID: uuid.New(), Role: biz.MessageTool, Content: "工具输出"},
		{ID: uuid.New(), Role: biz.MessageAssistant, Content: "回答"},
	}}
	queue := &fakeMemoryQueue{}
	indexer, err := biz.NewMemoryIndexer(reader, indexes, queue, biz.MemoryIndexerConfig{RecentMessages: 3}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := indexer.MaybeEnqueue(context.Background(), userID, sessionID); err != nil {
		t.Fatal(err)
	}
	if len(queue.tasks) != 1 {
		t.Fatalf("tasks = %#v", queue.tasks)
	}
	task := queue.tasks[0]
	if len(task.MessageIDs) != 2 || len(task.Versions) != 2 {
		t.Fatalf("task = %#v", task)
	}
	if task.Versions[0] != "v1" || task.Versions[1] != "v2" {
		t.Fatalf("versions = %v", task.Versions)
	}
}

// TestMemoryIndexerSkipsWithoutActiveIndex 验证没有 active 索引时不做任何事。
func TestMemoryIndexerSkipsWithoutActiveIndex(t *testing.T) {
	queue := &fakeMemoryQueue{}
	indexer, err := biz.NewMemoryIndexer(&fakeSummaryReader{}, &fakeMemoryIndexes{}, queue, biz.MemoryIndexerConfig{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := indexer.MaybeEnqueue(context.Background(), uuid.New(), uuid.New()); err != nil {
		t.Fatal(err)
	}
	if len(queue.tasks) != 0 {
		t.Fatalf("tasks = %#v", queue.tasks)
	}
	if err := indexer.MaybeEnqueue(context.Background(), uuid.Nil, uuid.New()); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("identity error = %v", err)
	}
}

// memoryJobPayload 构造向量化任务载荷。
func memoryJobPayload(task biz.MemoryTask) map[string]any {
	ids := make([]any, 0, len(task.MessageIDs))
	for _, id := range task.MessageIDs {
		ids = append(ids, id.String())
	}
	versions := make([]any, 0, len(task.Versions))
	for _, version := range task.Versions {
		versions = append(versions, version)
	}
	return map[string]any{
		"user_id": task.UserID.String(), "session_id": task.SessionID.String(),
		"message_ids": ids, "versions": versions,
	}
}

// newMemoryWorkerTest 构造带内存读取端口的向量化 Worker。
func newMemoryWorkerTest(t *testing.T, job biz.Job) (*biz.MemoryEmbedWorker, *fakeSummaryJobs, *fakeMemoryIndexes, *fakeMemoryVectors, *fakeEmbeddingGenerator, *fakeSummaryReader) {
	t.Helper()
	jobs := &fakeSummaryJobs{job: job}
	indexes := &fakeMemoryIndexes{indexes: []biz.EmbeddingIndex{activeMemoryIndex()}}
	vectors := &fakeMemoryVectors{}
	generator := &fakeEmbeddingGenerator{}
	reader := &fakeSummaryReader{}
	worker, err := biz.NewMemoryEmbedWorker(biz.MemoryEmbedWorkerDependencies{
		Jobs: jobs, Messages: reader, Indexes: indexes, Vectors: vectors,
		Generator: generator,
	}, biz.MemoryEmbedWorkerConfig{WorkerID: "memory-test", Heartbeat: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	return worker, jobs, indexes, vectors, generator, reader
}

// TestMemoryEmbedWorkerWritesVectors 验证消息读取、跳过未变更内容并写入向量。
func TestMemoryEmbedWorkerWritesVectors(t *testing.T) {
	message := biz.Message{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), Role: biz.MessageUser, Content: "问题", CreatedAt: time.Now().UTC()}
	task := biz.MemoryTask{UserID: message.UserID, SessionID: message.SessionID, MessageIDs: []uuid.UUID{message.ID}, Versions: []string{"v1"}}
	job := biz.Job{ID: uuid.New(), Type: biz.MemoryEmbedJobType, Payload: memoryJobPayload(task), Attempts: 1, FencingToken: 1}
	worker, jobs, _, vectors, generator, reader := newMemoryWorkerTest(t, job)
	reader.byIDs = []biz.Message{message}
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if jobs.completed != 1 || jobs.failed != 0 || generator.calls != 1 {
		t.Fatalf("completed=%d failed=%d calls=%d", jobs.completed, jobs.failed, generator.calls)
	}
	if len(vectors.upserts) != 1 || len(vectors.upserts[0]) != 1 {
		t.Fatalf("upserts = %#v", vectors.upserts)
	}
	if vectors.upserts[0][0].ContentHash != biz.MemoryContentHash("问题") {
		t.Fatalf("content hash = %s", vectors.upserts[0][0].ContentHash)
	}
}

// TestMemoryEmbedWorkerSkipsUnchangedAndRetired 验证内容未变与已退役索引不会重复写入。
func TestMemoryEmbedWorkerSkipsUnchangedAndRetired(t *testing.T) {
	message := biz.Message{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), Role: biz.MessageUser, Content: "问题", CreatedAt: time.Now().UTC()}
	task := biz.MemoryTask{UserID: message.UserID, SessionID: message.SessionID, MessageIDs: []uuid.UUID{message.ID}, Versions: []string{"v1", "retired"}}
	job := biz.Job{ID: uuid.New(), Type: biz.MemoryEmbedJobType, Payload: memoryJobPayload(task), Attempts: 1, FencingToken: 1}
	worker, jobs, indexes, vectors, generator, reader := newMemoryWorkerTest(t, job)
	reader.byIDs = []biz.Message{message}
	vectors.hashes = map[uuid.UUID]string{message.ID: biz.MemoryContentHash("问题")}
	indexes.indexes = append(indexes.indexes, biz.EmbeddingIndex{
		ID: uuid.New(), Version: "retired", Status: biz.EmbeddingIndexRetired, Dimension: biz.EmbeddingDimension,
	})
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if jobs.completed != 1 || generator.calls != 0 || len(vectors.upserts) != 0 {
		t.Fatalf("completed=%d calls=%d upserts=%#v", jobs.completed, generator.calls, vectors.upserts)
	}
}

// TestMemoryEmbedWorkerErrorClassification 验证模型不可用直接完成、可重试错误退避。
func TestMemoryEmbedWorkerErrorClassification(t *testing.T) {
	message := biz.Message{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), Role: biz.MessageUser, Content: "问题", CreatedAt: time.Now().UTC()}
	task := biz.MemoryTask{UserID: message.UserID, SessionID: message.SessionID, MessageIDs: []uuid.UUID{message.ID}, Versions: []string{"v1"}}
	job := biz.Job{ID: uuid.New(), Type: biz.MemoryEmbedJobType, Payload: memoryJobPayload(task), Attempts: 1, FencingToken: 1}
	worker, jobs, _, _, generator, reader := newMemoryWorkerTest(t, job)
	reader.byIDs = []biz.Message{message}
	generator.err = biz.ErrEmbeddingModelUnavailable
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if jobs.completed != 1 || jobs.failed != 0 {
		t.Fatalf("unavailable: completed=%d failed=%d", jobs.completed, jobs.failed)
	}
	jobs.claimed = false
	generator.err = errors.New("network error")
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if jobs.failed != 1 {
		t.Fatalf("retryable: failed=%d", jobs.failed)
	}
}

// TestMemoryIndexServiceMaintainAndActivate 验证首建索引、回填分组、覆盖度切换与低覆盖不切换。
func TestMemoryIndexServiceMaintainAndActivate(t *testing.T) {
	models := &fakeEmbeddingModelSource{providerID: uuid.New(), model: "embedding", dimension: biz.EmbeddingDimension}
	queue := &fakeMemoryQueue{}

	// 无索引：创建并立即激活，同时回填 active 缺失消息。
	indexes := &fakeMemoryIndexes{eligible: 2, unembedded: []biz.Message{
		{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New()},
		{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New()},
	}}
	service, err := biz.NewMemoryIndexService(indexes, models, queue, biz.MemoryIndexServiceConfig{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Maintain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Active == nil || !result.Activated || len(indexes.created) != 1 || result.BackfillEnqueued != 2 {
		t.Fatalf("maintain = %#v created=%d", result, len(indexes.created))
	}

	// staging 覆盖率达标：切换为 active。
	active := activeMemoryIndex()
	staging := biz.EmbeddingIndex{ID: uuid.New(), Version: "v2", Status: biz.EmbeddingIndexStaging, DualWrite: true, Dimension: biz.EmbeddingDimension}
	indexes = &fakeMemoryIndexes{indexes: []biz.EmbeddingIndex{active, staging}, eligible: 10, vectors: map[string]int64{"v1": 10, "v2": 9}}
	service, err = biz.NewMemoryIndexService(indexes, models, queue, biz.MemoryIndexServiceConfig{CoverageThreshold: 0.9}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err = service.Maintain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Activated || result.Active == nil || result.Active.Version != "v2" || len(indexes.activated) != 1 {
		t.Fatalf("switch = %#v activated=%v", result, indexes.activated)
	}

	// 覆盖率不足：保持 staging，继续回填。
	indexes = &fakeMemoryIndexes{indexes: []biz.EmbeddingIndex{active, staging}, eligible: 10, vectors: map[string]int64{"v1": 10, "v2": 2},
		unembedded: []biz.Message{{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New()}}}
	service, err = biz.NewMemoryIndexService(indexes, models, queue, biz.MemoryIndexServiceConfig{CoverageThreshold: 0.9}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err = service.Maintain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Activated || result.Coverage != 0.2 || result.BackfillEnqueued != 2 {
		t.Fatalf("low coverage = %#v", result)
	}

	if _, err := biz.NewMemoryIndexService(nil, nil, nil, biz.MemoryIndexServiceConfig{}, nil, nil); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("constructor error = %v", err)
	}
}

// TestMemoryEmbedWorkerStartStops 验证向量化 Worker 循环随取消退出。
func TestMemoryEmbedWorkerStartStops(t *testing.T) {
	job := biz.Job{ID: uuid.New(), Type: biz.MemoryEmbedJobType, Payload: map[string]any{}}
	worker, jobs, _, _, _, _ := newMemoryWorkerTest(t, job)
	jobs.claimed = true
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	worker.Start(ctx)
}

// TestMemoryMaintenanceRunOnceSuccess 验证维护成功路径会读取 active 与 staging 版本。
func TestMemoryMaintenanceRunOnceSuccess(t *testing.T) {
	indexes := &fakeMemoryIndexes{indexes: []biz.EmbeddingIndex{activeMemoryIndex()}, eligible: 0}
	service, err := biz.NewMemoryIndexService(indexes, &fakeEmbeddingModelSource{dimension: biz.EmbeddingDimension}, &fakeMemoryQueue{}, biz.MemoryIndexServiceConfig{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	maintenance, err := biz.NewMemoryMaintenance(service, biz.MemoryMaintenanceConfig{Enabled: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	maintenance.RunOnce(context.Background())
}

// fakeEmbeddingModelSource 是 embedding 模型来源替身。
type fakeEmbeddingModelSource struct {
	providerID uuid.UUID
	model      string
	dimension  int
	err        error
}

// GetEnabledEmbeddingModel 返回预置模型。
func (f *fakeEmbeddingModelSource) GetEnabledEmbeddingModel(context.Context) (uuid.UUID, string, int, error) {
	return f.providerID, f.model, f.dimension, f.err
}

// TestMemoryIndexServiceModelUnavailable 验证没有 embedding 模型时维护返回明确错误。
func TestMemoryIndexServiceModelUnavailable(t *testing.T) {
	models := &fakeEmbeddingModelSource{err: errors.New("no model")}
	service, err := biz.NewMemoryIndexService(&fakeMemoryIndexes{}, models, &fakeMemoryQueue{}, biz.MemoryIndexServiceConfig{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Maintain(context.Background()); err == nil || !strings.Contains(err.Error(), "no model") {
		t.Fatalf("maintain error = %v", err)
	}
}

// TestMemoryMaintenanceRunOnce 验证维护循环只记录错误不中断。
func TestMemoryMaintenanceRunOnce(t *testing.T) {
	service, err := biz.NewMemoryIndexService(&fakeMemoryIndexes{err: errors.New("db down")}, &fakeEmbeddingModelSource{}, &fakeMemoryQueue{}, biz.MemoryIndexServiceConfig{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	maintenance, err := biz.NewMemoryMaintenance(service, biz.MemoryMaintenanceConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	maintenance.RunOnce(context.Background())
	if _, err := biz.NewMemoryMaintenance(nil, biz.MemoryMaintenanceConfig{}, nil); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("constructor error = %v", err)
	}
}
