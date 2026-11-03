package biz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Mark-Panda/harness-agent/pkg/memory"
	"github.com/google/uuid"
)

// 结构化事实错误。
var (
	// ErrFactExtractionUnavailable 当前会话没有可用于事实提取的模型或凭证，任务应终止。
	ErrFactExtractionUnavailable = errors.New("fact extraction model is unavailable")
	// ErrFactPermanent 事实提取请求被永久拒绝，重试无意义。
	ErrFactPermanent = errors.New("fact extraction failed permanently")
	// FactExtractJobType 异步事实提取任务类型。
	FactExtractJobType = "memory.fact.extract"
	// FactSourceUser 用户明确陈述。
	FactSourceUser = "user"
	// FactSourceAssistant 助手归纳。
	FactSourceAssistant = "assistant"
	// FactSourceCorrection 用户显式纠正。
	FactSourceCorrection = "correction"
)

// MemoryFact 是用户私有的结构化事实版本。
type MemoryFact struct {
	// ID 事实版本唯一标识。
	ID uuid.UUID
	// UserID 所属用户，RLS 隔离键。
	UserID uuid.UUID
	// SessionID 来源会话，可为空。
	SessionID *uuid.UUID
	// Key 点分层事实键。
	Key string
	// Value JSON 值。
	Value json.RawMessage
	// Confidence 置信度，取值 [0,1]。
	Confidence float64
	// SourceKind 来源类型：user/assistant/correction。
	SourceKind string
	// Corrected 是否用户显式纠正。
	Corrected bool
	// SourceMessageID 来源消息。
	SourceMessageID *uuid.UUID
	// Metadata 提取元数据，禁止包含凭证。
	Metadata map[string]any
	// ValidFrom 生效时间。
	ValidFrom time.Time
	// ValidTo 失效时间，空表示当前生效。
	ValidTo *time.Time
	// Version 同一事实键的版本号。
	Version int64
	// CreatedAt 创建时间。
	CreatedAt time.Time
	// UpdatedAt 更新时间。
	UpdatedAt time.Time
}

// FactRepository 定义结构化事实持久化契约，所有方法强制携带 userID。
type FactRepository interface {
	// ListActiveFacts 按更新时间倒序列出当前生效事实。
	ListActiveFacts(ctx context.Context, userID uuid.UUID, limit int) ([]MemoryFact, error)
	// GetActiveFact 读取指定键的生效事实。
	GetActiveFact(ctx context.Context, userID uuid.UUID, key string) (MemoryFact, bool, error)
	// SaveFact 以期望版本守卫写入新版本并关闭旧版本；版本冲突时 applied=false。
	SaveFact(ctx context.Context, fact MemoryFact, expectedVersion int64) (MemoryFact, bool, error)
	// ListFactVersions 按版本倒序列出事实历史。
	ListFactVersions(ctx context.Context, userID uuid.UUID, key string, limit int) ([]MemoryFact, error)
	// ListFacts 按更新时间倒序列出用户全部事实版本，供导出与审计。
	ListFacts(ctx context.Context, userID uuid.UUID, limit int) ([]MemoryFact, error)
	// DeleteFact 关闭生效事实，保留历史版本用于审计。
	DeleteFact(ctx context.Context, userID uuid.UUID, key string, now time.Time) error
}

// FactReader 是上下文组装使用的结构化事实读取端口。
type FactReader interface {
	// ListActiveFacts 读取当前生效事实。
	ListActiveFacts(ctx context.Context, userID uuid.UUID, limit int) ([]MemoryFact, error)
}

// FactTrigger 是事实提取触发端口，供消息写入与 Run 完成路径尽力而为调用。
type FactTrigger interface {
	// MaybeEnqueue 在消息足够时幂等入队事实提取任务。
	MaybeEnqueue(ctx context.Context, userID, sessionID uuid.UUID) error
	// MaybeEnqueueSpeaker 在群聊话题中只提取指定发言人的消息，并把事实写入发言人的私有记忆。
	MaybeEnqueueSpeaker(ctx context.Context, ownerUserID, sessionID, factOwnerID uuid.UUID, speakerOpenID string) error
}

// FactPolicy 是事实写入冲突策略。
type FactPolicy struct {
	// MinConfidence 候选写入的最低置信度，默认 0.6。
	MinConfidence float64
	// CorrectionGuard 用户显式纠正后的覆盖保护阈值，默认 0.9。
	CorrectionGuard float64
	// MaxFactsPerUser 单用户保留的生效事实上限，默认 100。
	MaxFactsPerUser int
}

// normalize 补齐事实策略默认值。
func (p FactPolicy) normalize() FactPolicy {
	if p.MinConfidence <= 0 || p.MinConfidence > 1 {
		p.MinConfidence = 0.6
	}
	if p.CorrectionGuard <= 0 || p.CorrectionGuard > 1 {
		p.CorrectionGuard = 0.9
	}
	if p.MaxFactsPerUser <= 0 {
		p.MaxFactsPerUser = 100
	}
	return p
}

// FactCandidate 是一条待写入的事实候选。
type FactCandidate struct {
	// Key 点分层事实键。
	Key string
	// Value JSON 值。
	Value json.RawMessage
	// Confidence 置信度。
	Confidence float64
	// Correction 是否用户显式纠正。
	Correction bool
	// SourceMessageID 来源消息。
	SourceMessageID *uuid.UUID
	// SourceKind 来源类型。
	SourceKind string
}

// FactApplyResult 是一次事实写入统计。
type FactApplyResult struct {
	// Applied 成功写入的新版本数。
	Applied int
	// Skipped 因冲突或低置信度跳过的候选数。
	Skipped int
	// Conflicts 因并发版本冲突跳过的候选数。
	Conflicts int
}

// FactService 执行事实冲突策略与写入。
type FactService struct {
	repo   FactRepository
	policy FactPolicy
	clock  func() time.Time
}

// NewFactService 构造事实服务。
func NewFactService(repo FactRepository, policy FactPolicy, clock func() time.Time) (*FactService, error) {
	if repo == nil {
		return nil, invalid("fact service requires repository")
	}
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &FactService{repo: repo, policy: policy.normalize(), clock: clock}, nil
}

// Apply 按冲突策略写入候选事实；低置信度候选不得覆盖高置信度或显式纠正事实。
func (s *FactService) Apply(ctx context.Context, userID, sessionID uuid.UUID, candidates []FactCandidate) (FactApplyResult, error) {
	result := FactApplyResult{}
	if userID == uuid.Nil {
		return result, ErrForbidden
	}
	if len(candidates) > 20 {
		candidates = candidates[:20]
	}
	for _, candidate := range candidates {
		candidate.Key = strings.TrimSpace(candidate.Key)
		if candidate.Key == "" || len(candidate.Value) == 0 || !json.Valid(candidate.Value) || len(candidate.Value) > 8*1024 {
			result.Skipped++
			continue
		}
		if candidate.Confidence < s.policy.MinConfidence || candidate.Confidence > 1 {
			result.Skipped++
			continue
		}
		active, found, err := s.repo.GetActiveFact(ctx, userID, candidate.Key)
		if err != nil {
			return result, err
		}
		if !found {
			// 新事实键受单用户上限约束，避免事实表无界增长。
			facts, err := s.repo.ListActiveFacts(ctx, userID, s.policy.MaxFactsPerUser)
			if err != nil {
				return result, err
			}
			if len(facts) >= s.policy.MaxFactsPerUser {
				result.Skipped++
				continue
			}
		}
		if found {
			if bytes.Equal(active.Value, candidate.Value) {
				// 值未变化时不产生新版本，避免无意义的版本膨胀。
				result.Skipped++
				continue
			}
			// 用户显式纠正后，低置信度或非纠正候选不得静默覆盖。
			if active.Corrected && !candidate.Correction && candidate.Confidence < s.policy.CorrectionGuard {
				result.Skipped++
				continue
			}
			if !candidate.Correction && candidate.Confidence < active.Confidence {
				result.Skipped++
				continue
			}
		}
		sourceKind := candidate.SourceKind
		if candidate.Correction {
			sourceKind = FactSourceCorrection
		} else if sourceKind == "" {
			sourceKind = FactSourceAssistant
		}
		expected := int64(0)
		if found {
			expected = active.Version
		}
		now := s.clock()
		fact := MemoryFact{
			ID: uuid.New(), UserID: userID, SessionID: &sessionID, Key: candidate.Key, Value: candidate.Value,
			Confidence: candidate.Confidence, SourceKind: sourceKind, Corrected: candidate.Correction,
			SourceMessageID: candidate.SourceMessageID, Metadata: map[string]any{"extracted_at": now.Format(time.RFC3339Nano)},
			ValidFrom: now, Version: expected + 1, CreatedAt: now, UpdatedAt: now,
		}
		_, applied, err := s.repo.SaveFact(ctx, fact, expected)
		if err != nil {
			return result, err
		}
		if !applied {
			result.Conflicts++
			continue
		}
		result.Applied++
	}
	return result, nil
}

// ListActive 返回当前生效事实，供上下文组装与查询。
func (s *FactService) ListActive(ctx context.Context, userID uuid.UUID, limit int) ([]MemoryFact, error) {
	return s.repo.ListActiveFacts(ctx, userID, limit)
}

// ListActiveFacts 实现上下文组装的只读事实端口。
func (s *FactService) ListActiveFacts(ctx context.Context, userID uuid.UUID, limit int) ([]MemoryFact, error) {
	return s.repo.ListActiveFacts(ctx, userID, limit)
}

// Delete 关闭指定事实键的生效版本，保留历史版本。
func (s *FactService) Delete(ctx context.Context, userID uuid.UUID, key string) error {
	if userID == uuid.Nil || strings.TrimSpace(key) == "" {
		return invalid("fact deletion requires user and key")
	}
	return s.repo.DeleteFact(ctx, userID, strings.TrimSpace(key), s.clock())
}

// FactExtractor 从对话中提取结构化事实候选。
type FactExtractor interface {
	Extract(ctx context.Context, userID, sessionID uuid.UUID, messages []Message) ([]FactCandidate, error)
}

// FactTask 是异步事实提取任务载荷。
type FactTask struct {
	// UserID 任务归属用户，也是会话消息的 RLS 读取身份（群聊话题为会话所有者）。
	UserID uuid.UUID
	// FactOwnerID 事实写入用户；群聊话题中为发言人本人，零值表示与 UserID 相同。
	FactOwnerID uuid.UUID
	// SpeakerOpenID 群聊发言人过滤键；非空时只分析该发言人的用户消息。
	SpeakerOpenID string
	// SessionID 来源会话。
	SessionID uuid.UUID
	// MessageIDs 待分析消息 ID，按时间顺序排列。
	MessageIDs []uuid.UUID
	// Fingerprint 消息内容指纹，执行时重新计算并比对。
	Fingerprint string
	// SourceIndex 映射用消息 ID 顺序，与任务消息一一对应。
	SourceIDs []uuid.UUID
}

// FactJobQueue 是事实提取任务入队端口。
type FactJobQueue interface {
	// EnqueueFacts 幂等入队事实提取任务。
	EnqueueFacts(ctx context.Context, task FactTask, availableAt time.Time) error
}

// FactIndexerConfig 是事实提取触发配置。
type FactIndexerConfig struct {
	// RecentMessages 每次触发最多检查的最近消息数，默认 16。
	RecentMessages int
	// MinMessages 至少包含的消息数，默认 4，避免每轮对话都调用模型。
	MinMessages int
}

// normalize 补齐事实触发默认配置。
func (c FactIndexerConfig) normalize() FactIndexerConfig {
	if c.RecentMessages <= 0 {
		c.RecentMessages = 16
	}
	if c.RecentMessages > 64 {
		c.RecentMessages = 64
	}
	if c.MinMessages <= 0 {
		c.MinMessages = 4
	}
	return c
}

// FactIndexer 为最近对话入队异步事实提取任务。
type FactIndexer struct {
	messages SummaryMessageReader
	queue    FactJobQueue
	config   FactIndexerConfig
	clock    func() time.Time
	logger   *slog.Logger
}

// NewFactIndexer 构造事实提取触发器。
func NewFactIndexer(messages SummaryMessageReader, queue FactJobQueue, config FactIndexerConfig, logger *slog.Logger, clock func() time.Time) (*FactIndexer, error) {
	if messages == nil || queue == nil {
		return nil, invalid("fact indexer requires messages and queue")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &FactIndexer{messages: messages, queue: queue, config: config.normalize(), clock: clock, logger: logger}, nil
}

// MaybeEnqueue 为最近消息入队事实提取任务；消息不足或没有用户发言时不触发。
func (i *FactIndexer) MaybeEnqueue(ctx context.Context, userID, sessionID uuid.UUID) error {
	if userID == uuid.Nil || sessionID == uuid.Nil {
		return invalid("fact indexing requires user and session ids")
	}
	return i.enqueue(ctx, userID, sessionID, uuid.Nil, "")
}

// MaybeEnqueueSpeaker 在群聊话题中只提取指定发言人的消息，并将事实写入该发言人的私有记忆。
// 群聊消息统一归属会话所有者，因此读取仍使用 ownerUserID 满足 RLS，写入目标切换为 factOwnerID。
func (i *FactIndexer) MaybeEnqueueSpeaker(ctx context.Context, ownerUserID, sessionID, factOwnerID uuid.UUID, speakerOpenID string) error {
	if ownerUserID == uuid.Nil || sessionID == uuid.Nil || factOwnerID == uuid.Nil || strings.TrimSpace(speakerOpenID) == "" {
		return invalid("speaker fact indexing requires owner, session, fact owner and speaker")
	}
	return i.enqueue(ctx, ownerUserID, sessionID, factOwnerID, speakerOpenID)
}

// enqueue 按时间正序筛选消息、计算指纹并幂等入队；speakerOpenID 非空时仅保留该发言人的用户消息。
func (i *FactIndexer) enqueue(ctx context.Context, ownerUserID, sessionID, factOwnerID uuid.UUID, speakerOpenID string) error {
	recent, err := i.messages.ListRecentMessages(ctx, ownerUserID, sessionID, i.config.RecentMessages)
	if err != nil {
		return err
	}
	if len(recent) < i.config.MinMessages {
		return nil
	}
	// ListRecentMessages 按时间倒序返回：倒序遍历恢复正序，确保指纹与 Worker 按 ID 读取的顺序一致。
	selected := make([]Message, 0, len(recent))
	hasUser := false
	for position := len(recent) - 1; position >= 0; position-- {
		message := recent[position]
		if message.Role != MessageUser && message.Role != MessageAssistant {
			continue
		}
		if speakerOpenID != "" && (message.Role != MessageUser || message.SpeakerOpenID != speakerOpenID) {
			continue
		}
		if strings.TrimSpace(message.Content) == "" {
			continue
		}
		if message.Role == MessageUser {
			hasUser = true
		}
		selected = append(selected, message)
	}
	// 私有记忆要求存在用户发言；群聊发言人只保留其本人用户消息，天然满足该约束。
	if !hasUser || len(selected) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(selected))
	for _, message := range selected {
		ids = append(ids, message.ID)
	}
	fingerprint := messageFingerprint(selected)
	task := FactTask{
		UserID: ownerUserID, FactOwnerID: factOwnerID, SpeakerOpenID: speakerOpenID,
		SessionID: sessionID, MessageIDs: ids, Fingerprint: fingerprint, SourceIDs: ids,
	}
	if err := i.queue.EnqueueFacts(ctx, task, i.clock()); err != nil {
		return fmt.Errorf("enqueue fact task: %w", err)
	}
	i.logger.Info("fact extraction task enqueued", "session_id", sessionID, "messages", len(ids),
		"fact_owner_id", factOwnerID, "speaker_open_id", speakerOpenID)
	return nil
}

// messageFingerprint 计算消息内容指纹，检测任务执行前的消息变化。
func messageFingerprint(messages []Message) string {
	return SummarySourceFingerprint(messages)
}

// FactExtractionWorkerConfig 是事实提取 Worker 配置。
type FactExtractionWorkerConfig struct {
	// WorkerID 实例标识。
	WorkerID string
	// JobType 任务类型，默认 memory.fact.extract。
	JobType string
	// Concurrency 单实例并发任务数。
	Concurrency int
	// PollInterval 无任务时的轮询间隔。
	PollInterval time.Duration
	// Lease 租约时长。
	Lease time.Duration
	// Heartbeat 续约间隔。
	Heartbeat time.Duration
	// MaxFacts 单次提取的最大事实数。
	MaxFacts int
}

// normalize 补齐事实 Worker 默认配置。
func (c FactExtractionWorkerConfig) normalize() FactExtractionWorkerConfig {
	if c.JobType == "" {
		c.JobType = FactExtractJobType
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
	if c.MaxFacts <= 0 {
		c.MaxFacts = 20
	}
	return c
}

// FactExtractionWorkerDependencies 聚合事实提取 Worker 依赖。
type FactExtractionWorkerDependencies struct {
	// Jobs 持久化任务队列。
	Jobs JobQueue
	// Messages 消息读取端口。
	Messages SummaryMessageReader
	// Facts 事实冲突策略服务。
	Facts *FactService
	// Extractor 事实提取器。
	Extractor FactExtractor
	// Logger 结构化日志。
	Logger *slog.Logger
	// Clock 当前时间函数。
	Clock func() time.Time
}

// FactExtractionWorker 认领事实提取任务并写入结构化事实。
type FactExtractionWorker struct {
	jobs      JobQueue
	messages  SummaryMessageReader
	facts     *FactService
	extractor FactExtractor
	config    FactExtractionWorkerConfig
	clock     func() time.Time
	logger    *slog.Logger
}

// NewFactExtractionWorker 构造事实提取 Worker。
func NewFactExtractionWorker(dependencies FactExtractionWorkerDependencies, config FactExtractionWorkerConfig) (*FactExtractionWorker, error) {
	if dependencies.Jobs == nil || dependencies.Messages == nil || dependencies.Facts == nil || dependencies.Extractor == nil {
		return nil, invalid("fact worker requires jobs, messages, fact service and extractor")
	}
	if strings.TrimSpace(config.WorkerID) == "" {
		return nil, invalid("fact worker id is required")
	}
	logger := dependencies.Logger
	if logger == nil {
		logger = slog.Default()
	}
	clock := dependencies.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &FactExtractionWorker{
		jobs: dependencies.Jobs, messages: dependencies.Messages, facts: dependencies.Facts,
		extractor: dependencies.Extractor, config: config.normalize(), clock: clock, logger: logger,
	}, nil
}

// Start 启动配置数量的事实提取协程。
func (w *FactExtractionWorker) Start(ctx context.Context) {
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
func (w *FactExtractionWorker) loop(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if _, err := w.ProcessOne(ctx); err != nil && ctx.Err() == nil {
			w.logger.Warn("claim fact job failed", "error", err)
		}
		timer.Reset(w.config.PollInterval)
	}
}

// ProcessOne 认领并处理至多一个事实提取任务。
func (w *FactExtractionWorker) ProcessOne(ctx context.Context) (bool, error) {
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

// handle 加载来源消息、提取事实并按策略写入。
func (w *FactExtractionWorker) handle(ctx context.Context, job Job) {
	task, err := decodeFactTask(job)
	if err != nil {
		w.logger.Error("invalid fact job payload", "job_id", job.ID, "error", err)
		_ = w.jobs.Complete(context.WithoutCancel(ctx), job.ID, w.config.WorkerID, job.FencingToken)
		return
	}
	messages, err := w.messages.ListMessagesByIDs(ctx, task.UserID, task.SessionID, task.MessageIDs)
	if err != nil {
		w.failJob(ctx, job, err)
		return
	}
	if len(messages) != len(task.MessageIDs) || messageFingerprint(messages) != task.Fingerprint {
		w.logger.Warn("fact source changed; completing job", "job_id", job.ID)
		_ = w.jobs.Complete(context.WithoutCancel(ctx), job.ID, w.config.WorkerID, job.FencingToken)
		return
	}
	candidates, err := w.extractor.Extract(ctx, task.UserID, task.SessionID, messages)
	if err != nil {
		if errors.Is(err, ErrFactExtractionUnavailable) || errors.Is(err, ErrFactPermanent) || errors.Is(err, ErrSummaryModelUnavailable) {
			w.logger.Warn("fact extraction is not retryable; completing job", "job_id", job.ID, "error", err)
			_ = w.jobs.Complete(context.WithoutCancel(ctx), job.ID, w.config.WorkerID, job.FencingToken)
			return
		}
		w.failJob(ctx, job, err)
		return
	}
	if len(candidates) > w.config.MaxFacts {
		candidates = candidates[:w.config.MaxFacts]
	}
	// 群聊话题的事实只能写入发言人本人：FactOwnerID 非空时覆盖任务归属用户。
	factUserID := task.FactOwnerID
	if factUserID == uuid.Nil {
		factUserID = task.UserID
	}
	result, err := w.facts.Apply(ctx, factUserID, task.SessionID, candidates)
	if err != nil {
		w.failJob(ctx, job, err)
		return
	}
	w.logger.Info("facts applied", "session_id", task.SessionID, "fact_user_id", factUserID,
		"speaker_open_id", task.SpeakerOpenID, "applied", result.Applied,
		"skipped", result.Skipped, "conflicts", result.Conflicts)
	_ = w.jobs.Complete(context.WithoutCancel(ctx), job.ID, w.config.WorkerID, job.FencingToken)
}

// failJob 记录可重试失败并退避。
func (w *FactExtractionWorker) failJob(ctx context.Context, job Job, cause error) {
	if err := w.jobs.Fail(context.WithoutCancel(ctx), job.ID, w.config.WorkerID, job.FencingToken,
		w.clock().Add(backoffDelay(job.Attempts)), sanitizeExecutionError(cause), w.clock()); err != nil {
		w.logger.Warn("fail fact job failed", "job_id", job.ID, "error", err)
	}
}

// decodeFactTask 解析事实任务载荷。
func decodeFactTask(job Job) (FactTask, error) {
	userID, err := payloadUUID(job.Payload, "user_id")
	if err != nil {
		return FactTask{}, err
	}
	sessionID, err := payloadUUID(job.Payload, "session_id")
	if err != nil {
		return FactTask{}, err
	}
	rawIDs, ok := job.Payload["message_ids"].([]any)
	if !ok || len(rawIDs) == 0 {
		return FactTask{}, fmt.Errorf("message_ids is invalid")
	}
	ids := make([]uuid.UUID, 0, len(rawIDs))
	for _, raw := range rawIDs {
		text, ok := raw.(string)
		if !ok {
			return FactTask{}, fmt.Errorf("message_ids contains a non-string value")
		}
		id, err := uuid.Parse(text)
		if err != nil || id == uuid.Nil {
			return FactTask{}, fmt.Errorf("message_ids contains an invalid id")
		}
		ids = append(ids, id)
	}
	fingerprint, _ := job.Payload["fingerprint"].(string)
	if len(fingerprint) != 64 {
		return FactTask{}, fmt.Errorf("fingerprint is invalid")
	}
	// fact_owner_id 与 speaker_open_id 为群聊话题的可选字段，兼容既有私有记忆任务。
	var factOwnerID uuid.UUID
	if raw, ok := job.Payload["fact_owner_id"].(string); ok && raw != "" {
		parsed, err := uuid.Parse(raw)
		if err != nil || parsed == uuid.Nil {
			return FactTask{}, fmt.Errorf("fact_owner_id is invalid")
		}
		factOwnerID = parsed
	}
	speakerOpenID, _ := job.Payload["speaker_open_id"].(string)
	return FactTask{
		UserID: userID, FactOwnerID: factOwnerID, SpeakerOpenID: speakerOpenID,
		SessionID: sessionID, MessageIDs: ids, Fingerprint: fingerprint, SourceIDs: ids,
	}, nil
}

// ParseFactModelOutput 解析模型输出为事实候选，供 LLM 提取器复用。
func ParseFactModelOutput(text string, messages []Message) ([]FactCandidate, error) {
	parsed, err := memory.ParseFactCandidates(text, 20, 8*1024)
	if err != nil {
		return nil, err
	}
	candidates := make([]FactCandidate, 0, len(parsed))
	for _, candidate := range parsed {
		sourceKind := FactSourceAssistant
		var sourceID *uuid.UUID
		if candidate.SourceIndex > 0 && candidate.SourceIndex <= len(messages) {
			message := messages[candidate.SourceIndex-1]
			id := message.ID
			sourceID = &id
			if message.Role == MessageUser {
				sourceKind = FactSourceUser
			}
		}
		if candidate.Correction {
			sourceKind = FactSourceCorrection
		}
		candidates = append(candidates, FactCandidate{
			Key: candidate.Key, Value: candidate.Value, Confidence: candidate.Confidence,
			Correction: candidate.Correction, SourceMessageID: sourceID, SourceKind: sourceKind,
		})
	}
	return candidates, nil
}

// 编译期断言事实触发器与读取端口实现。
var (
	_ FactTrigger = (*FactIndexer)(nil)
	_ FactReader  = (*FactService)(nil)
)
