package biz

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Mark-Panda/harness-agent/pkg/agent/model"
	"github.com/google/uuid"
)

// EmbeddingModelSource 返回当前可用的平台级 embedding 模型。
type EmbeddingModelSource interface {
	// GetEnabledEmbeddingModel 返回已启用供应商下的 embedding 模型；不存在时返回未找到。
	GetEnabledEmbeddingModel(ctx context.Context) (providerID uuid.UUID, modelName string, dimension int, err error)
}

// MemoryTask 是异步向量化任务载荷。
type MemoryTask struct {
	// UserID 任务归属用户。
	UserID uuid.UUID
	// SessionID 来源会话。
	SessionID uuid.UUID
	// MessageIDs 待向量化消息 ID，按时间顺序排列。
	MessageIDs []uuid.UUID
	// Versions 目标索引版本；双写时同时包含 active 与 staging。
	Versions []string
}

// MemoryJobQueue 是向量化任务入队端口。
type MemoryJobQueue interface {
	// EnqueueMemory 幂等入队向量化任务。
	EnqueueMemory(ctx context.Context, task MemoryTask, availableAt time.Time) error
}

// MemoryTrigger 是向量化触发端口，供消息写入与 Run 完成路径尽力而为调用。
type MemoryTrigger interface {
	// MaybeEnqueue 为最近消息幂等入队向量化任务。
	MaybeEnqueue(ctx context.Context, userID, sessionID uuid.UUID) error
}

// MemoryContentHash 计算记忆来源内容哈希。
func MemoryContentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// MemoryIndexerConfig 是向量化触发配置。
type MemoryIndexerConfig struct {
	// RecentMessages 每次触发最多检查的最近消息数，默认 32。
	RecentMessages int
}

// normalize 补齐向量化触发默认配置。
func (c MemoryIndexerConfig) normalize() MemoryIndexerConfig {
	if c.RecentMessages <= 0 {
		c.RecentMessages = 32
	}
	if c.RecentMessages > 64 {
		// 单任务消息上限与 Worker 校验保持一致，超过部分由后续触发或维护回填补足。
		c.RecentMessages = 64
	}
	return c
}

// MemoryIndexer 根据当前索引版本为最近消息入队向量化任务。
type MemoryIndexer struct {
	messages SummaryMessageReader
	indexes  EmbeddingIndexRepository
	queue    MemoryJobQueue
	config   MemoryIndexerConfig
	clock    func() time.Time
	logger   *slog.Logger
}

// NewMemoryIndexer 构造向量化触发器。
func NewMemoryIndexer(messages SummaryMessageReader, indexes EmbeddingIndexRepository, queue MemoryJobQueue,
	config MemoryIndexerConfig, logger *slog.Logger, clock func() time.Time) (*MemoryIndexer, error) {
	if messages == nil || indexes == nil || queue == nil {
		return nil, invalid("memory indexer requires messages, indexes and queue")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &MemoryIndexer{messages: messages, indexes: indexes, queue: queue, config: config.normalize(), clock: clock, logger: logger}, nil
}

// MaybeEnqueue 为最近的可向量化消息入队任务；没有 active 索引时不做任何事。
func (i *MemoryIndexer) MaybeEnqueue(ctx context.Context, userID, sessionID uuid.UUID) error {
	if userID == uuid.Nil || sessionID == uuid.Nil {
		return invalid("memory indexing requires user and session ids")
	}
	versions, err := writeVersions(ctx, i.indexes)
	if err != nil {
		return err
	}
	if len(versions) == 0 {
		return nil
	}
	recent, err := i.messages.ListRecentMessages(ctx, userID, sessionID, i.config.RecentMessages)
	if err != nil {
		return err
	}
	ids := make([]uuid.UUID, 0, len(recent))
	for _, message := range recent {
		if !embeddableRole(message.Role) || strings.TrimSpace(message.Content) == "" {
			continue
		}
		ids = append(ids, message.ID)
	}
	if len(ids) == 0 {
		return nil
	}
	task := MemoryTask{UserID: userID, SessionID: sessionID, MessageIDs: ids, Versions: versions}
	if err := i.queue.EnqueueMemory(ctx, task, i.clock()); err != nil {
		return fmt.Errorf("enqueue memory task: %w", err)
	}
	i.logger.Info("memory embedding task enqueued", "session_id", sessionID, "messages", len(ids), "versions", len(versions))
	return nil
}

// writeVersions 返回当前需要写入的索引版本：active；存在 staging 时按双写开关追加。
func writeVersions(ctx context.Context, indexes EmbeddingIndexRepository) ([]string, error) {
	list, err := indexes.ListIndexes(ctx)
	if err != nil {
		return nil, err
	}
	var active *EmbeddingIndex
	staging := make([]EmbeddingIndex, 0, 1)
	for position := range list {
		switch list[position].Status {
		case EmbeddingIndexActive:
			active = &list[position]
		case EmbeddingIndexStaging:
			staging = append(staging, list[position])
		}
	}
	if active == nil {
		return nil, nil
	}
	versions := []string{active.Version}
	for _, index := range staging {
		// 双写关闭时 staging 只由回填任务单独写入，避免新消息产生额外成本。
		if index.DualWrite {
			versions = append(versions, index.Version)
		}
	}
	return versions, nil
}

// embeddableRole 判断消息是否值得进入长期记忆。
// 只索引用户输入：助手回复由模型生成，缺少新的用户意图与事实，
// 入库后会在语义检索中大量召回同质内容，反而稀释真正有价值的用户记忆。
func embeddableRole(role MessageRole) bool {
	return role == MessageUser
}

// LLMEmbeddingGenerator 使用索引声明的供应商与模型生成向量。
type LLMEmbeddingGenerator struct {
	providers ProviderCredentialRepository
	opener    CredentialOpener
	timeout   time.Duration
}

// NewLLMEmbeddingGenerator 构造 embedding 生成器。
func NewLLMEmbeddingGenerator(providers ProviderCredentialRepository, opener CredentialOpener, timeout time.Duration) (*LLMEmbeddingGenerator, error) {
	if providers == nil || opener == nil {
		return nil, invalid("embedding generator requires provider credentials and opener")
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &LLMEmbeddingGenerator{providers: providers, opener: opener, timeout: timeout}, nil
}

// Embed 调用索引绑定的 embedding 模型并校验维度。
func (g *LLMEmbeddingGenerator) Embed(ctx context.Context, index EmbeddingIndex, inputs []string) (EmbeddingResult, error) {
	if len(inputs) == 0 {
		return EmbeddingResult{}, fmt.Errorf("%w: embedding inputs are empty", ErrInvalidConfig)
	}
	if index.Dimension != EmbeddingDimension {
		return EmbeddingResult{}, fmt.Errorf("%w: unsupported embedding dimension %d", ErrInvalidConfig, index.Dimension)
	}
	provider, credential, err := g.providers.GetProviderCredential(ctx, index.ProviderID)
	if err != nil {
		return EmbeddingResult{}, fmt.Errorf("%w: read provider credential: %v", ErrEmbeddingModelUnavailable, err)
	}
	if provider.Status != ProviderEnabled {
		return EmbeddingResult{}, fmt.Errorf("%w: provider is disabled", ErrEmbeddingModelUnavailable)
	}
	plaintext, err := g.opener.Open(credential.KeyVersion, credential.Encrypted)
	if err != nil || len(plaintext) == 0 {
		return EmbeddingResult{}, fmt.Errorf("%w: decrypt provider credential", ErrEmbeddingModelUnavailable)
	}
	client, err := model.NewClient(provider.BaseURL, string(plaintext), g.timeout)
	if err != nil {
		return EmbeddingResult{}, fmt.Errorf("%w: %v", ErrEmbeddingModelUnavailable, err)
	}
	response, err := client.Embed(ctx, model.EmbeddingRequest{Model: index.Model, Inputs: inputs, Dimensions: index.Dimension})
	if err != nil {
		// 认证、参数与维度错误重试无意义，其余错误交给任务退避。
		if errors.Is(err, model.ErrAuthentication) || errors.Is(err, model.ErrPermanent) || errors.Is(err, model.ErrCapability) {
			return EmbeddingResult{}, fmt.Errorf("%w: %v", ErrEmbeddingModelUnavailable, err)
		}
		return EmbeddingResult{}, err
	}
	for _, vector := range response.Embeddings {
		if len(vector) != index.Dimension {
			return EmbeddingResult{}, fmt.Errorf("%w: provider returned dimension %d", ErrEmbeddingModelUnavailable, len(vector))
		}
	}
	return EmbeddingResult{Vectors: response.Embeddings, InputTokens: response.InputTokens}, nil
}

// MemoryEmbedWorkerConfig 是向量化 Worker 配置。
type MemoryEmbedWorkerConfig struct {
	// WorkerID 实例标识。
	WorkerID string
	// JobType 认领的任务类型，默认 memory.embed。
	JobType string
	// Concurrency 单实例并发任务数。
	Concurrency int
	// PollInterval 无任务时的轮询间隔。
	PollInterval time.Duration
	// Lease 任务租约时长。
	Lease time.Duration
	// Heartbeat 租约续约间隔。
	Heartbeat time.Duration
	// BatchSize 单次 embedding 请求的最大文本数，默认 16。
	BatchSize int
}

// normalize 补齐向量化 Worker 默认配置。
func (c MemoryEmbedWorkerConfig) normalize() MemoryEmbedWorkerConfig {
	if c.JobType == "" {
		c.JobType = MemoryEmbedJobType
	}
	if c.Concurrency <= 0 {
		c.Concurrency = 1
	}
	if c.PollInterval <= 0 {
		c.PollInterval = time.Second
	}
	if c.Lease <= 0 {
		c.Lease = 60 * time.Second
	}
	if c.Heartbeat <= 0 || c.Heartbeat >= c.Lease {
		c.Heartbeat = c.Lease / 3
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 16
	}
	if c.BatchSize > 64 {
		c.BatchSize = 64
	}
	return c
}

// MemoryEmbedWorkerDependencies 聚合向量化 Worker 依赖。
type MemoryEmbedWorkerDependencies struct {
	// Jobs 持久化任务队列。
	Jobs JobQueue
	// Messages 消息读取端口。
	Messages SummaryMessageReader
	// Indexes 索引版本仓储。
	Indexes EmbeddingIndexRepository
	// Vectors 用户向量仓储。
	Vectors MemoryVectorRepository
	// Generator embedding 生成器。
	Generator EmbeddingGenerator
	// Usage 后台向量化用量记录器，可为空。
	Usage UsageRecorder
	// Logger 结构化日志。
	Logger *slog.Logger
	// Clock 当前时间函数。
	Clock func() time.Time
}

// MemoryEmbedWorker 认领向量化任务并幂等写入向量。
type MemoryEmbedWorker struct {
	jobs      JobQueue
	messages  SummaryMessageReader
	indexes   EmbeddingIndexRepository
	vectors   MemoryVectorRepository
	generator EmbeddingGenerator
	usage     UsageRecorder
	config    MemoryEmbedWorkerConfig
	clock     func() time.Time
	logger    *slog.Logger
}

// NewMemoryEmbedWorker 构造向量化 Worker。
func NewMemoryEmbedWorker(dependencies MemoryEmbedWorkerDependencies, config MemoryEmbedWorkerConfig) (*MemoryEmbedWorker, error) {
	if dependencies.Jobs == nil || dependencies.Messages == nil || dependencies.Indexes == nil ||
		dependencies.Vectors == nil || dependencies.Generator == nil {
		return nil, invalid("memory embed worker requires jobs, messages, indexes, vectors and generator")
	}
	if strings.TrimSpace(config.WorkerID) == "" {
		return nil, invalid("memory embed worker id is required")
	}
	logger := dependencies.Logger
	if logger == nil {
		logger = slog.Default()
	}
	clock := dependencies.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &MemoryEmbedWorker{
		jobs: dependencies.Jobs, messages: dependencies.Messages, indexes: dependencies.Indexes,
		vectors: dependencies.Vectors, generator: dependencies.Generator, usage: dependencies.Usage,
		config: config.normalize(), clock: clock, logger: logger,
	}, nil
}

// Start 启动配置数量的向量化协程。
func (w *MemoryEmbedWorker) Start(ctx context.Context) {
	var workers sync.WaitGroup
	for index := 0; index < w.config.Concurrency; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			w.loop(ctx)
		}()
	}
	workers.Wait()
}

// loop 持续认领任务直到上下文取消。
func (w *MemoryEmbedWorker) loop(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if _, err := w.ProcessOne(ctx); err != nil && ctx.Err() == nil {
			w.logger.Warn("claim memory embed job failed", "error", err)
		}
		timer.Reset(w.config.PollInterval)
	}
}

// ProcessOne 认领并处理至多一个向量化任务。
func (w *MemoryEmbedWorker) ProcessOne(ctx context.Context) (bool, error) {
	job, claimed, err := w.jobs.Claim(ctx, w.config.WorkerID, w.config.JobType, w.config.Lease, w.clock())
	if err != nil {
		return false, err
	}
	if !claimed {
		return false, nil
	}
	w.handle(ctx, job)
	return true, nil
}

// handle 解析任务、读取来源消息并按索引版本批量写入向量。
func (w *MemoryEmbedWorker) handle(ctx context.Context, job Job) {
	task, err := decodeMemoryTask(job)
	if err != nil {
		w.logger.Error("invalid memory embed job payload", "job_id", job.ID, "error", err)
		_ = w.jobs.Complete(context.WithoutCancel(ctx), job.ID, w.config.WorkerID, job.FencingToken)
		return
	}
	if len(task.MessageIDs) > 64 {
		// 单任务消息数受限，避免超长请求与租约占用；超限任务直接终止并由后续维护任务补足。
		w.logger.Warn("memory embed job exceeds message limit", "job_id", job.ID, "messages", len(task.MessageIDs))
		_ = w.jobs.Complete(context.WithoutCancel(ctx), job.ID, w.config.WorkerID, job.FencingToken)
		return
	}
	messages, err := w.messages.ListMessagesByIDs(ctx, task.UserID, task.SessionID, task.MessageIDs)
	if err != nil {
		w.failJob(ctx, job, err)
		return
	}
	eligible := make([]Message, 0, len(messages))
	for _, message := range messages {
		if embeddableRole(message.Role) && strings.TrimSpace(message.Content) != "" {
			eligible = append(eligible, message)
		}
	}
	if len(eligible) == 0 {
		_ = w.jobs.Complete(context.WithoutCancel(ctx), job.ID, w.config.WorkerID, job.FencingToken)
		return
	}
	for _, version := range task.Versions {
		index, err := w.indexes.GetIndex(ctx, version)
		if err != nil {
			// 索引已删除时跳过；其它错误交给任务重试。
			if errors.Is(err, ErrResourceNotFound) {
				continue
			}
			w.failJob(ctx, job, err)
			return
		}
		if index.Status == EmbeddingIndexRetired {
			continue
		}
		if err := w.embedMessages(ctx, task.UserID, task.SessionID, index, eligible); err != nil {
			if errors.Is(err, ErrEmbeddingModelUnavailable) {
				w.logger.Warn("embedding model unavailable; completing job", "job_id", job.ID, "version", version, "error", err)
				_ = w.jobs.Complete(context.WithoutCancel(ctx), job.ID, w.config.WorkerID, job.FencingToken)
				return
			}
			w.failJob(ctx, job, err)
			return
		}
	}
	_ = w.jobs.Complete(context.WithoutCancel(ctx), job.ID, w.config.WorkerID, job.FencingToken)
}

// embedMessages 跳过未变更内容后分批生成向量并写入。
func (w *MemoryEmbedWorker) embedMessages(ctx context.Context, userID, sessionID uuid.UUID, index EmbeddingIndex, messages []Message) error {
	ids := make([]uuid.UUID, 0, len(messages))
	for _, message := range messages {
		ids = append(ids, message.ID)
	}
	hashes, err := w.vectors.ListSourceHashes(ctx, userID, index.Version, ids)
	if err != nil {
		return err
	}
	pending := make([]Message, 0, len(messages))
	for _, message := range messages {
		if hashes[message.ID] == MemoryContentHash(message.Content) {
			continue
		}
		pending = append(pending, message)
	}
	if len(pending) == 0 {
		return nil
	}
	now := w.clock()
	for start := 0; start < len(pending); start += w.config.BatchSize {
		end := start + w.config.BatchSize
		if end > len(pending) {
			end = len(pending)
		}
		batch := pending[start:end]
		inputs := make([]string, 0, len(batch))
		for _, message := range batch {
			inputs = append(inputs, message.Content)
		}
		result, err := w.generator.Embed(ctx, index, inputs)
		if err != nil {
			return err
		}
		if w.usage != nil && result.InputTokens > 0 {
			providerID := index.ProviderID
			if err := w.usage.RecordUsage(ctx, userID, nil, &providerID, int64(result.InputTokens), 0, 0, now); err != nil {
				w.logger.Warn("record embedding usage failed", "session_id", sessionID, "error", err)
			}
		}
		if len(result.Vectors) != len(batch) {
			return fmt.Errorf("%w: embedding count mismatch", ErrInvalidConfig)
		}
		vectors := make([]MemoryVector, 0, len(batch))
		for position, message := range batch {
			if err := validateVectorDimension(result.Vectors[position], index.Dimension); err != nil {
				return err
			}
			sessionIDCopy := sessionID
			messageID := message.ID
			vectors = append(vectors, MemoryVector{
				ID: uuid.New(), UserID: userID, SessionID: &sessionIDCopy,
				SourceType: MemorySourceMessage, SourceID: &messageID, Content: message.Content,
				ContentHash: MemoryContentHash(message.Content), Embedding: result.Vectors[position],
				Metadata: map[string]any{
					"role": string(message.Role), "speaker_open_id": message.SpeakerOpenID,
					"created_at": message.CreatedAt.UTC().Format(time.RFC3339Nano),
				},
				ProviderID: index.ProviderID, Model: index.Model, Dimension: index.Dimension,
				Version: index.Version, CreatedAt: now,
			})
		}
		if err := w.vectors.UpsertVectors(ctx, userID, vectors); err != nil {
			return err
		}
	}
	w.logger.Info("memory vectors written", "session_id", sessionID, "version", index.Version, "messages", len(pending))
	return nil
}

// failJob 记录可重试失败并退避。
func (w *MemoryEmbedWorker) failJob(ctx context.Context, job Job, cause error) {
	if err := w.jobs.Fail(context.WithoutCancel(ctx), job.ID, w.config.WorkerID, job.FencingToken,
		w.clock().Add(backoffDelay(job.Attempts)), sanitizeExecutionError(cause), w.clock()); err != nil {
		w.logger.Warn("fail memory embed job failed", "job_id", job.ID, "error", err)
	}
}

// decodeMemoryTask 解析向量化任务载荷。
func decodeMemoryTask(job Job) (MemoryTask, error) {
	userID, err := payloadUUID(job.Payload, "user_id")
	if err != nil {
		return MemoryTask{}, err
	}
	sessionID, err := payloadUUID(job.Payload, "session_id")
	if err != nil {
		return MemoryTask{}, err
	}
	rawIDs, ok := job.Payload["message_ids"].([]any)
	if !ok || len(rawIDs) == 0 {
		return MemoryTask{}, fmt.Errorf("message_ids is invalid")
	}
	ids := make([]uuid.UUID, 0, len(rawIDs))
	for _, raw := range rawIDs {
		text, ok := raw.(string)
		if !ok {
			return MemoryTask{}, fmt.Errorf("message_ids contains a non-string value")
		}
		id, err := uuid.Parse(text)
		if err != nil || id == uuid.Nil {
			return MemoryTask{}, fmt.Errorf("message_ids contains an invalid id")
		}
		ids = append(ids, id)
	}
	rawVersions, ok := job.Payload["versions"].([]any)
	if !ok || len(rawVersions) == 0 {
		return MemoryTask{}, fmt.Errorf("versions is invalid")
	}
	versions := make([]string, 0, len(rawVersions))
	for _, raw := range rawVersions {
		text, ok := raw.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return MemoryTask{}, fmt.Errorf("versions contains an invalid value")
		}
		versions = append(versions, text)
	}
	return MemoryTask{UserID: userID, SessionID: sessionID, MessageIDs: ids, Versions: versions}, nil
}

// MemoryIndexServiceConfig 是索引维护配置。
type MemoryIndexServiceConfig struct {
	// CoverageThreshold staging 切换到 active 所需的最低覆盖比例，默认 0.95。
	CoverageThreshold float64
	// BackfillBatch 单次回填扫描的最大消息数，默认 200。
	BackfillBatch int
}

// normalize 补齐索引维护默认配置。
func (c MemoryIndexServiceConfig) normalize() MemoryIndexServiceConfig {
	if c.CoverageThreshold <= 0 || c.CoverageThreshold > 1 {
		c.CoverageThreshold = 0.95
	}
	if c.BackfillBatch <= 0 {
		c.BackfillBatch = 200
	}
	return c
}

// MaintainResult 是一次索引维护结果。
type MaintainResult struct {
	// Active 当前生效索引，可为空。
	Active *EmbeddingIndex
	// Staging 正在回填的索引，可为空。
	Staging *EmbeddingIndex
	// Coverage staging 覆盖率，0 到 1。
	Coverage float64
	// Activated 本次是否完成切换。
	Activated bool
	// BackfillEnqueued 本次入队的回填任务数。
	BackfillEnqueued int
}

// MemoryIndexService 维护索引版本：自动建索引、回填、覆盖度校验与切换。
type MemoryIndexService struct {
	indexes EmbeddingIndexRepository
	models  EmbeddingModelSource
	queue   MemoryJobQueue
	config  MemoryIndexServiceConfig
	clock   func() time.Time
	logger  *slog.Logger
}

// NewMemoryIndexService 构造索引维护服务。
func NewMemoryIndexService(indexes EmbeddingIndexRepository, models EmbeddingModelSource, queue MemoryJobQueue,
	config MemoryIndexServiceConfig, logger *slog.Logger, clock func() time.Time) (*MemoryIndexService, error) {
	if indexes == nil || models == nil || queue == nil {
		return nil, invalid("memory index service requires indexes, model source and queue")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &MemoryIndexService{indexes: indexes, models: models, queue: queue, config: config.normalize(), clock: clock, logger: logger}, nil
}

// Maintain 执行一次索引维护：确保 active 索引存在，校验 staging 覆盖率，必要时切换并回填。
func (s *MemoryIndexService) Maintain(ctx context.Context) (MaintainResult, error) {
	list, err := s.indexes.ListIndexes(ctx)
	if err != nil {
		return MaintainResult{}, err
	}
	var active, staging *EmbeddingIndex
	for position := range list {
		switch list[position].Status {
		case EmbeddingIndexActive:
			active = &list[position]
		case EmbeddingIndexStaging:
			if staging == nil || list[position].CreatedAt.Before(staging.CreatedAt) {
				staging = &list[position]
			}
		}
	}
	if active == nil {
		created, err := s.ensureIndex(ctx)
		if err != nil {
			return MaintainResult{}, err
		}
		// 首个索引立即生效，历史消息随后由回填补齐；没有 active 时检索本就没有结果。
		activated, err := s.indexes.ActivateIndex(ctx, created.Version)
		if err != nil {
			return MaintainResult{}, err
		}
		active = &activated
		result := MaintainResult{Active: active, Activated: true}
		enqueued, err := s.backfill(ctx, active, nil)
		if err != nil {
			return result, err
		}
		result.BackfillEnqueued = enqueued
		return result, nil
	}
	result := MaintainResult{Active: active, Staging: staging}
	// active 与目录中启用的 embedding 模型不一致时自动创建 staging 索引，实现不停机重建。
	if staging == nil {
		providerID, modelName, dimension, err := s.models.GetEnabledEmbeddingModel(ctx)
		if err == nil && (providerID != active.ProviderID || modelName != active.Model || dimension != active.Dimension) {
			staged, err := s.createIndex(ctx, providerID, modelName, dimension)
			if err != nil {
				return result, err
			}
			staging = &staged
			result.Staging = staging
		} else if err != nil && !errors.Is(err, ErrResourceNotFound) {
			return result, err
		}
	}
	eligible, err := s.indexes.CountEligibleMessages(ctx)
	if err != nil {
		return result, err
	}
	if staging != nil {
		vectors, err := s.indexes.CountVectors(ctx, staging.Version)
		if err != nil {
			return result, err
		}
		if eligible > 0 {
			result.Coverage = float64(vectors) / float64(eligible)
		}
		if eligible == 0 || result.Coverage >= s.config.CoverageThreshold {
			if err := s.indexes.SetDualWrite(ctx, staging.Version, true); err != nil {
				return result, err
			}
			activated, err := s.indexes.ActivateIndex(ctx, staging.Version)
			if err != nil {
				return result, err
			}
			result.Active = &activated
			result.Staging = nil
			result.Activated = true
			s.logger.Info("embedding index switched", "version", activated.Version, "coverage", result.Coverage)
		}
	}
	enqueued, err := s.backfill(ctx, result.Active, result.Staging)
	if err != nil {
		return result, err
	}
	result.BackfillEnqueued = enqueued
	return result, nil
}

// ensureIndex 基于启用 embedding 模型创建 staging 索引。
func (s *MemoryIndexService) ensureIndex(ctx context.Context) (EmbeddingIndex, error) {
	providerID, modelName, dimension, err := s.models.GetEnabledEmbeddingModel(ctx)
	if err != nil {
		return EmbeddingIndex{}, err
	}
	return s.createIndex(ctx, providerID, modelName, dimension)
}

// createIndex 按供应商、模型与维度创建 staging 索引。
func (s *MemoryIndexService) createIndex(ctx context.Context, providerID uuid.UUID, modelName string, dimension int) (EmbeddingIndex, error) {
	if dimension <= 0 {
		dimension = EmbeddingDimension
	}
	// 版本名包含模型与创建时间，避免重建时与历史版本冲突。
	version := fmt.Sprintf("%s-%d-%d", sanitizeVersion(modelName), dimension, s.clock().UnixNano())
	index := EmbeddingIndex{
		ID: uuid.New(), Name: version, ProviderID: providerID, Model: modelName, Dimension: dimension,
		Version: version, Status: EmbeddingIndexStaging, DualWrite: true,
		Metadata: map[string]any{"created_by": "memory-index-service"},
	}
	created, err := s.indexes.CreateIndex(ctx, index)
	if err != nil {
		return EmbeddingIndex{}, err
	}
	s.logger.Info("embedding index staged", "version", created.Version, "model", created.Model)
	return created, nil
}

// backfill 为 active 与 staging 索引扫描缺失向量并按会话入队回填任务。
func (s *MemoryIndexService) backfill(ctx context.Context, active, staging *EmbeddingIndex) (int, error) {
	versions := make([]string, 0, 2)
	if active != nil {
		versions = append(versions, active.Version)
	}
	if staging != nil {
		versions = append(versions, staging.Version)
	}
	enqueued := 0
	for _, version := range versions {
		messages, err := s.indexes.ListUnembeddedMessages(ctx, version, s.config.BackfillBatch)
		if err != nil {
			return enqueued, err
		}
		// 按用户与会话分组，保证任务载荷满足用户隔离与单任务消息上限。
		type groupKey struct {
			userID    uuid.UUID
			sessionID uuid.UUID
		}
		groups := make(map[groupKey][]uuid.UUID)
		for _, message := range messages {
			key := groupKey{userID: message.UserID, sessionID: message.SessionID}
			groups[key] = append(groups[key], message.ID)
		}
		for key, ids := range groups {
			// 按 Worker 单任务上限切分，避免超限任务被直接完成而丢失回填。
			for start := 0; start < len(ids); start += 64 {
				end := start + 64
				if end > len(ids) {
					end = len(ids)
				}
				task := MemoryTask{UserID: key.userID, SessionID: key.sessionID, MessageIDs: ids[start:end], Versions: []string{version}}
				if err := s.queue.EnqueueMemory(ctx, task, s.clock()); err != nil {
					return enqueued, err
				}
				enqueued++
			}
		}
	}
	return enqueued, nil
}

// sanitizeVersion 把模型名转换为安全的索引版本片段。
func sanitizeVersion(value string) string {
	var builder strings.Builder
	for _, char := range strings.ToLower(value) {
		switch {
		case char >= 'a' && char <= 'z', char >= '0' && char <= '9', char == '-', char == '_':
			builder.WriteRune(char)
		default:
			builder.WriteRune('-')
		}
	}
	result := strings.Trim(builder.String(), "-")
	if result == "" {
		return "embedding"
	}
	if len(result) > 48 {
		result = result[:48]
	}
	return result
}

// MemoryMaintenanceConfig 是后台维护循环配置。
type MemoryMaintenanceConfig struct {
	// Enabled 是否启用后台维护。
	Enabled bool
	// Interval 维护轮询间隔。
	Interval time.Duration
}

// MemoryMaintenance 周期性维护索引并回填缺失向量。
type MemoryMaintenance struct {
	service *MemoryIndexService
	config  MemoryMaintenanceConfig
	logger  *slog.Logger
}

// NewMemoryMaintenance 构造后台维护循环。
func NewMemoryMaintenance(service *MemoryIndexService, config MemoryMaintenanceConfig, logger *slog.Logger) (*MemoryMaintenance, error) {
	if service == nil {
		return nil, invalid("memory maintenance requires index service")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if config.Interval <= 0 {
		config.Interval = time.Minute
	}
	return &MemoryMaintenance{service: service, config: config, logger: logger}, nil
}

// Start 周期执行维护直到上下文取消。
func (m *MemoryMaintenance) Start(ctx context.Context) {
	if !m.config.Enabled {
		return
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		m.RunOnce(ctx)
		timer.Reset(m.config.Interval)
	}
}

// RunOnce 执行一次维护；错误只记录日志，避免后台循环退出。
func (m *MemoryMaintenance) RunOnce(ctx context.Context) {
	result, err := m.service.Maintain(ctx)
	if err != nil {
		m.logger.Warn("memory index maintenance failed", "error", err)
		return
	}
	m.logger.Info("memory index maintenance completed", "active", indexVersion(result.Active),
		"staging", indexVersion(result.Staging), "coverage", result.Coverage, "backfill", result.BackfillEnqueued)
}

// indexVersion 安全读取索引版本用于日志。
func indexVersion(index *EmbeddingIndex) string {
	if index == nil {
		return ""
	}
	return index.Version
}

// 编译期断言各端口实现。
var (
	_ MemoryTrigger      = (*MemoryIndexer)(nil)
	_ EmbeddingGenerator = (*LLMEmbeddingGenerator)(nil)
)
