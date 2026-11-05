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

	"github.com/Mark-Panda/harness-agent/pkg/memory"
	"github.com/google/uuid"
)

// 摘要任务错误，用于区分可重试的基础设施失败与不可重试的来源/版本失效。
var (
	// ErrSummarySourceInvalid 来源消息缺失、被删除或内容指纹不匹配，摘要不得基于不完整来源生成。
	ErrSummarySourceInvalid = errors.New("summary source is invalid")
	// ErrSummaryModelUnavailable 当前会话没有可用于摘要的模型或凭证，任务应终止而不是无限重试。
	ErrSummaryModelUnavailable = errors.New("summary model is unavailable")
	// ErrSummaryPermanent 摘要请求被供应商永久拒绝（参数错误、认证失败或上下文超限），重试无意义。
	ErrSummaryPermanent = errors.New("summary generation failed permanently")
	// ErrSummaryProviderBusy 供应商并发舱壁已满，任务应退避后重试。
	ErrSummaryProviderBusy = errors.New("summary provider concurrency is full")
)

// SummaryStatus 是摘要版本的生命周期状态。
type SummaryStatus string

const (
	// SummaryActive 当前生效的摘要版本。
	SummaryActive SummaryStatus = "active"
	// SummarySuperseded 已被更新版本替换。
	SummarySuperseded SummaryStatus = "superseded"
	// SummaryInvalidated 来源消息被删除或篡改，摘要不再可信。
	SummaryInvalidated SummaryStatus = "invalidated"
	// SummaryJobType 异步摘要任务的任务类型。
	SummaryJobType = "memory.summarize"
)

// MessagePosition 是消息的稳定排序位置：创建时间加消息 ID，用于版本边界与游标。
type MessagePosition struct {
	// CreatedAt 消息创建时间（UTC）。
	CreatedAt time.Time
	// ID 消息 ID，同一创建时间下用于稳定排序。
	ID uuid.UUID
}

// IsZero 判断位置是否为空，空位置表示从会话起点开始。
func (p MessagePosition) IsZero() bool {
	return p.ID == uuid.Nil || p.CreatedAt.IsZero()
}

// After 判断当前位置是否严格晚于另一个位置。
func (p MessagePosition) After(other MessagePosition) bool {
	if p.IsZero() {
		return false
	}
	if other.IsZero() {
		return true
	}
	if p.CreatedAt.After(other.CreatedAt) {
		return true
	}
	if p.CreatedAt.Equal(other.CreatedAt) {
		return p.ID.String() > other.ID.String()
	}
	return false
}

// SessionSummary 是会话的版本化滚动摘要。
// SourceMessageIDs 只记录本版本新增的消息，CoveredMessageCount 记录累计覆盖数量。
type SessionSummary struct {
	// ID 摘要版本唯一标识。
	ID uuid.UUID
	// SessionID 所属会话。
	SessionID uuid.UUID
	// UserID 所属用户，RLS 隔离键。
	UserID uuid.UUID
	// Version 会话内单调递增版本号。
	Version int64
	// SummaryText 模型生成的公开摘要正文，不含隐藏思维链。
	SummaryText string
	// SourceMessageIDs 本版本新增来源消息 ID，按时间顺序排列。
	SourceMessageIDs []uuid.UUID
	// SourceStartMessageID 本版本来源范围的第一条消息。
	SourceStartMessageID *uuid.UUID
	// SourceEndMessageID 本版本来源范围的最后一条消息，也是摘要覆盖边界。
	SourceEndMessageID *uuid.UUID
	// SourceEndAt 边界消息的创建时间，与 ID 组成稳定排序。
	SourceEndAt *time.Time
	// SourceFingerprint 本版本来源消息的内容指纹。
	SourceFingerprint string
	// CoveredMessageCount 摘要累计覆盖的非删除消息数。
	CoveredMessageCount int
	// ParentSummaryID 上一版本摘要 ID，首个版本为空。
	ParentSummaryID *uuid.UUID
	// Status 版本生命周期状态。
	Status SummaryStatus
	// ModelProviderID 生成摘要使用的供应商 ID。
	ModelProviderID *uuid.UUID
	// ModelID 生成摘要使用的模型标识。
	ModelID string
	// InputTokens 摘要调用输入 Token。
	InputTokens int
	// OutputTokens 摘要调用输出 Token。
	OutputTokens int
	// CreatedAt 版本创建时间（UTC）。
	CreatedAt time.Time
	// UpdatedAt 最后更新时间（UTC）。
	UpdatedAt time.Time
}

// Boundary 返回摘要覆盖边界位置。
func (s SessionSummary) Boundary() MessagePosition {
	if s.SourceEndMessageID == nil || s.SourceEndAt == nil {
		return MessagePosition{}
	}
	return MessagePosition{CreatedAt: *s.SourceEndAt, ID: *s.SourceEndMessageID}
}

// SummaryRepository 定义摘要版本持久化契约，所有方法强制携带 userID。
type SummaryRepository interface {
	// GetActiveSummary 读取当前生效摘要；来源消息被删除时标记失效并返回未命中。
	GetActiveSummary(ctx context.Context, userID, sessionID uuid.UUID) (SessionSummary, bool, error)
	// LatestSummaryVersion 返回包含失效版本在内的最大版本号，用于分配新版本。
	LatestSummaryVersion(ctx context.Context, userID, sessionID uuid.UUID) (int64, error)
	// SaveSummary 以期望版本和来源校验守卫写入新版本，原子替换旧 active；stale 时返回 applied=false。
	SaveSummary(ctx context.Context, summary SessionSummary, expectedVersion int64, event OutboxEvent) (SessionSummary, bool, error)
	// InvalidateActive 显式失效当前摘要，用于消息删除或编辑后的立即失效。
	InvalidateActive(ctx context.Context, userID, sessionID uuid.UUID, now time.Time) error
	// ListSummaries 按版本倒序列出摘要历史，供审计与导出。
	ListSummaries(ctx context.Context, userID, sessionID uuid.UUID, limit int) ([]SessionSummary, error)
	// ListUserSummaries 按创建时间倒序列出用户全部摘要版本，供记忆导出。
	ListUserSummaries(ctx context.Context, userID uuid.UUID, limit int) ([]SessionSummary, error)
}

// SummaryMessageReader 读取摘要来源消息；实现必须强制 user_id 与会话隔离。
type SummaryMessageReader interface {
	// ListRecentMessages 按时间倒序读取最近消息。
	ListRecentMessages(ctx context.Context, userID, sessionID uuid.UUID, limit int) ([]Message, error)
	// ListMessagesAfter 按时间正序读取指定位置之后的消息。
	ListMessagesAfter(ctx context.Context, userID, sessionID uuid.UUID, after MessagePosition, limit int) ([]Message, error)
	// ListMessagesByIDs 按 ID 读取消息并按时间正序返回，缺失或删除的消息不会返回。
	ListMessagesByIDs(ctx context.Context, userID, sessionID uuid.UUID, messageIDs []uuid.UUID) ([]Message, error)
	// CountMessagesThrough 统计边界位置（含）之前的非删除消息数，用于校验累计覆盖。
	CountMessagesThrough(ctx context.Context, userID, sessionID uuid.UUID, boundary MessagePosition) (int64, error)
}

// SummaryTask 是异步摘要任务的载荷，包含来源范围、期望版本与内容指纹。
type SummaryTask struct {
	// UserID 任务归属用户。
	UserID uuid.UUID
	// SessionID 目标会话。
	SessionID uuid.UUID
	// BaseVersion 入队时的最新摘要版本；写入时必须仍为该版本，防止旧任务覆盖新版本。
	BaseVersion int64
	// SourceMessageIDs 本版本新增来源消息 ID，按时间顺序排列。
	SourceMessageIDs []uuid.UUID
	// Fingerprint 来源消息内容指纹，执行时重新计算并比对。
	Fingerprint string
	// EndMessageID 来源范围结束消息 ID。
	EndMessageID uuid.UUID
	// EndAt 来源范围结束消息创建时间。
	EndAt time.Time
	// CoveredMessageCount 写入后累计覆盖消息数，包含此前所有版本。
	CoveredMessageCount int
}

// SummaryJobQueue 是摘要任务入队端口；同一来源指纹只保留一个任务。
type SummaryJobQueue interface {
	// EnqueueSummary 幂等入队摘要任务。
	EnqueueSummary(ctx context.Context, task SummaryTask, availableAt time.Time) error
}

// SummaryTrigger 是摘要触发端口，供消息写入与 Run 完成路径以尽力而为方式调用。
type SummaryTrigger interface {
	// MaybeEnqueue 在达到阈值时幂等入队摘要任务。
	MaybeEnqueue(ctx context.Context, userID, sessionID uuid.UUID) error
}

// SummaryPlanConfig 是摘要触发策略。
type SummaryPlanConfig struct {
	// TriggerMessages 会话消息总数达到该值后才触发摘要。
	TriggerMessages int
	// KeepRecent 始终保留在最近窗口、不进入摘要的消息条数。
	KeepRecent int
	// MaxSourceMessages 单个摘要任务最多包含的新增消息数，防止任务无限膨胀。
	MaxSourceMessages int
}

// normalize 补齐摘要触发策略的安全默认值。
func (c SummaryPlanConfig) normalize() SummaryPlanConfig {
	if c.TriggerMessages <= 0 {
		c.TriggerMessages = 24
	}
	if c.KeepRecent <= 0 {
		c.KeepRecent = 12
	}
	if c.TriggerMessages <= c.KeepRecent {
		c.TriggerMessages = c.KeepRecent + 1
	}
	if c.MaxSourceMessages <= 0 {
		c.MaxSourceMessages = 10000
	}
	return c
}

// summaryPlannerClock 是摘要规划时钟，测试可注入固定时间。
type summaryPlannerClock func() time.Time

// SummaryPlanner 根据会话消息量与当前摘要版本决定是否入队异步摘要任务。
type SummaryPlanner struct {
	messages  SummaryMessageReader
	summaries SummaryRepository
	queue     SummaryJobQueue
	config    SummaryPlanConfig
	clock     summaryPlannerClock
	logger    *slog.Logger
}

// NewSummaryPlanner 构造摘要规划器；缺失关键依赖时返回错误。
func NewSummaryPlanner(messages SummaryMessageReader, summaries SummaryRepository, queue SummaryJobQueue, config SummaryPlanConfig, logger *slog.Logger, clock func() time.Time) (*SummaryPlanner, error) {
	if messages == nil || summaries == nil || queue == nil {
		return nil, invalid("summary planner requires message reader, summary repository and job queue")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &SummaryPlanner{
		messages: messages, summaries: summaries, queue: queue,
		config: config.normalize(), clock: clock, logger: logger,
	}, nil
}

// MaybeEnqueue 检查是否达到摘要触发条件；满足时按当前版本与来源范围幂等入队。
// 该方法只做轻量读取与入队，不阻塞消息写入或 Run 完成路径。
func (p *SummaryPlanner) MaybeEnqueue(ctx context.Context, userID, sessionID uuid.UUID) error {
	if userID == uuid.Nil || sessionID == uuid.Nil {
		return invalid("summary planning requires user and session ids")
	}
	// 多取一条用于同时判断触发阈值与“保留最近窗口”之外的边界消息。
	fetch := p.config.TriggerMessages
	if p.config.KeepRecent > fetch {
		fetch = p.config.KeepRecent
	}
	recent, err := p.messages.ListRecentMessages(ctx, userID, sessionID, fetch+1)
	if err != nil {
		return err
	}
	if len(recent) <= p.config.TriggerMessages || len(recent) <= p.config.KeepRecent {
		return nil
	}
	boundary := recent[p.config.KeepRecent]
	boundaryPosition := MessagePosition{CreatedAt: boundary.CreatedAt, ID: boundary.ID}

	latest, err := p.summaries.LatestSummaryVersion(ctx, userID, sessionID)
	if err != nil {
		return err
	}
	active, found, err := p.summaries.GetActiveSummary(ctx, userID, sessionID)
	if err != nil {
		return err
	}
	// 已有摘要已经覆盖到边界或更晚位置时无需重复摘要。
	if found && !boundaryPosition.After(active.Boundary()) {
		return nil
	}
	after := MessagePosition{}
	covered := 0
	parentVersion := latest
	if found {
		after = active.Boundary()
		covered = active.CoveredMessageCount
	}
	// 只取期望版本之后、边界（含）之前的消息，避免把最近窗口也压进摘要。
	messages, err := p.messages.ListMessagesAfter(ctx, userID, sessionID, after, p.config.MaxSourceMessages)
	if err != nil {
		return err
	}
	filtered := make([]Message, 0, len(messages))
	for _, message := range messages {
		position := MessagePosition{CreatedAt: message.CreatedAt, ID: message.ID}
		if !position.After(boundaryPosition) && !position.IsZero() {
			filtered = append(filtered, message)
		}
		// 已越过边界后停止，保持来源范围连续。
		if position.After(boundaryPosition) {
			break
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(filtered))
	for _, message := range filtered {
		ids = append(ids, message.ID)
	}
	// 单任务来源可能被上限截断，结束边界必须取实际最后一条消息，不能越过未包含的消息。
	last := filtered[len(filtered)-1]
	task := SummaryTask{
		UserID: userID, SessionID: sessionID, BaseVersion: parentVersion,
		SourceMessageIDs: ids, Fingerprint: SummarySourceFingerprint(filtered),
		EndMessageID: last.ID, EndAt: last.CreatedAt,
		CoveredMessageCount: covered + len(filtered),
	}
	if err := p.queue.EnqueueSummary(ctx, task, p.clock()); err != nil {
		return fmt.Errorf("enqueue summary task: %w", err)
	}
	p.logger.Info("summary task enqueued", "session_id", sessionID, "base_version", parentVersion,
		"source_messages", len(ids), "covered_messages", task.CoveredMessageCount)
	return nil
}

// SummarySourceFingerprint 计算来源消息内容指纹：按时间顺序绑定 ID 与正文，检测缺失或篡改。
func SummarySourceFingerprint(messages []Message) string {
	hash := sha256.New()
	for _, message := range messages {
		_, _ = hash.Write([]byte(message.ID.String()))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(message.Content))
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// SummaryGeneration 是摘要生成结果与模型元数据。
type SummaryGeneration struct {
	// Text 摘要正文。
	Text string
	// ProviderID 使用的供应商 ID。
	ProviderID *uuid.UUID
	// ModelID 使用的模型标识。
	ModelID string
	// InputTokens 输入 Token。
	InputTokens int
	// OutputTokens 输出 Token。
	OutputTokens int
}

// SummaryGenerator 生成摘要；实现可以是 LLM，也可以是测试替身。
type SummaryGenerator interface {
	// Summarize 基于已有摘要与新增消息生成完整最新摘要。
	Summarize(ctx context.Context, userID, sessionID uuid.UUID, previous string, messages []Message) (SummaryGeneration, error)
}

// SummaryWorkerConfig 是摘要 Worker 的轮询与租约配置。
type SummaryWorkerConfig struct {
	// WorkerID 实例标识，写入任务租约。
	WorkerID string
	// JobType 认领的任务类型，默认 memory.summarize。
	JobType string
	// Concurrency 单实例并发摘要任务数。
	Concurrency int
	// PollInterval 无任务时的轮询间隔。
	PollInterval time.Duration
	// Lease 任务租约时长。
	Lease time.Duration
	// Heartbeat 租约续约间隔，必须小于 Lease。
	Heartbeat time.Duration
	// MaxSummaryBytes 单份摘要允许的最大字节数。
	MaxSummaryBytes int
}

// normalize 补齐摘要 Worker 默认配置。
func (c SummaryWorkerConfig) normalize() SummaryWorkerConfig {
	if c.JobType == "" {
		c.JobType = SummaryJobType
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
	if c.MaxSummaryBytes <= 0 {
		c.MaxSummaryBytes = 64 * 1024
	}
	return c
}

// SummaryWorkerDependencies 聚合摘要 Worker 所需依赖。
type SummaryWorkerDependencies struct {
	// Jobs 持久化任务队列。
	Jobs JobQueue
	// Sessions 会话仓储，用于校验会话仍活跃。
	Sessions SessionRepository
	// Messages 摘要来源消息读取端口。
	Messages SummaryMessageReader
	// Summaries 摘要版本仓储。
	Summaries SummaryRepository
	// Generator 摘要生成器。
	Generator SummaryGenerator
	// Logger 结构化日志。
	Logger *slog.Logger
	// Clock 当前时间函数。
	Clock func() time.Time
}

// SummaryWorker 认领摘要任务并生成新版本，重复执行与乱序任务由版本守卫消除。
type SummaryWorker struct {
	jobs      JobQueue
	sessions  SessionRepository
	messages  SummaryMessageReader
	summaries SummaryRepository
	generator SummaryGenerator
	config    SummaryWorkerConfig
	clock     func() time.Time
	logger    *slog.Logger
}

// NewSummaryWorker 构造摘要 Worker；缺少关键依赖时返回错误。
func NewSummaryWorker(dependencies SummaryWorkerDependencies, config SummaryWorkerConfig) (*SummaryWorker, error) {
	if dependencies.Jobs == nil || dependencies.Sessions == nil || dependencies.Messages == nil ||
		dependencies.Summaries == nil || dependencies.Generator == nil {
		return nil, invalid("summary worker requires jobs, sessions, messages, summaries and generator")
	}
	if strings.TrimSpace(config.WorkerID) == "" {
		return nil, invalid("summary worker id is required")
	}
	logger := dependencies.Logger
	if logger == nil {
		logger = slog.Default()
	}
	clock := dependencies.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &SummaryWorker{
		jobs: dependencies.Jobs, sessions: dependencies.Sessions, messages: dependencies.Messages,
		summaries: dependencies.Summaries, generator: dependencies.Generator,
		config: config.normalize(), clock: clock, logger: logger,
	}, nil
}

// Start 启动配置数量的摘要协程并在上下文取消后返回。
func (w *SummaryWorker) Start(ctx context.Context) {
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

// loop 持续认领摘要任务；无任务时按轮询间隔等待。
func (w *SummaryWorker) loop(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if _, err := w.ProcessOne(ctx); err != nil && ctx.Err() == nil {
			w.logger.Warn("claim summary job failed", "error", err)
		}
		timer.Reset(w.config.PollInterval)
	}
}

// ProcessOne 认领并处理至多一个摘要任务。
func (w *SummaryWorker) ProcessOne(ctx context.Context) (bool, error) {
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

// handle 解析任务载荷并生成摘要；可重试错误退避重试，来源/版本失效直接完成。
func (w *SummaryWorker) handle(ctx context.Context, job Job) {
	finishJob := beginJob()
	failureClass := ""
	defer func() {
		outcome := "success"
		if failureClass != "" {
			outcome = "failure"
		}
		finishJob(outcome)
	}()
	task, err := decodeSummaryTask(job)
	if err != nil {
		w.logger.Error("invalid summary job payload", "job_id", job.ID, "error", err)
		failureClass = "invalid_job"
		_ = w.jobs.Complete(context.WithoutCancel(ctx), job.ID, w.config.WorkerID, job.FencingToken)
		return
	}
	// 会话归档或删除后不再生成新摘要，避免继续消耗模型额度。
	session, err := w.sessions.GetSession(ctx, task.UserID, task.SessionID)
	if err != nil {
		_ = w.jobs.Complete(context.WithoutCancel(ctx), job.ID, w.config.WorkerID, job.FencingToken)
		return
	}
	if session.Status != SessionActive {
		_ = w.jobs.Complete(context.WithoutCancel(ctx), job.ID, w.config.WorkerID, job.FencingToken)
		return
	}
	// 先确认当前版本仍等于入队时版本，防止乱序任务用旧上下文覆盖新摘要。
	latest, err := w.summaries.LatestSummaryVersion(ctx, task.UserID, task.SessionID)
	if err != nil {
		failureClass = "retryable_failure"
		w.failJob(ctx, job, err)
		return
	}
	if latest != task.BaseVersion {
		w.logger.Info("summary task is stale", "job_id", job.ID, "base_version", task.BaseVersion, "latest_version", latest)
		_ = w.jobs.Complete(context.WithoutCancel(ctx), job.ID, w.config.WorkerID, job.FencingToken)
		return
	}
	active, found, err := w.summaries.GetActiveSummary(ctx, task.UserID, task.SessionID)
	if err != nil {
		failureClass = "retryable_failure"
		w.failJob(ctx, job, err)
		return
	}
	previous := ""
	if found {
		previous = active.SummaryText
	}

	source, err := w.messages.ListMessagesByIDs(ctx, task.UserID, task.SessionID, task.SourceMessageIDs)
	if err != nil {
		failureClass = "retryable_failure"
		w.failJob(ctx, job, err)
		return
	}
	if err := validateSummarySource(task, source); err != nil {
		w.logger.Warn("summary source invalid; completing job", "job_id", job.ID, "session_id", task.SessionID, "error", err)
		_ = w.jobs.Complete(context.WithoutCancel(ctx), job.ID, w.config.WorkerID, job.FencingToken)
		return
	}

	// 生成期间持续续约；租约丢失时取消调用，避免过期 Worker 提交结果。
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	leaseLost := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(w.config.Heartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
			}
			if err := w.jobs.Renew(runCtx, job.ID, w.config.WorkerID, job.FencingToken, w.config.Lease, w.clock()); err != nil {
				close(leaseLost)
				cancel()
				return
			}
		}
	}()

	generation, generateErr := w.generator.Summarize(runCtx, task.UserID, task.SessionID, previous, source)
	cancel()
	select {
	case <-leaseLost:
		w.logger.Warn("summary job lease lost", "job_id", job.ID)
		<-done
		return
	default:
	}
	<-done
	if generateErr != nil {
		if errors.Is(generateErr, ErrSummaryModelUnavailable) || errors.Is(generateErr, ErrSummaryPermanent) {
			w.logger.Warn("summary generation is not retryable; completing job", "job_id", job.ID, "error", generateErr)
			_ = w.jobs.Complete(context.WithoutCancel(ctx), job.ID, w.config.WorkerID, job.FencingToken)
			return
		}
		w.failJob(ctx, job, generateErr)
		failureClass = "retryable_failure"
		return
	}
	if err := memory.ValidateSummaryText(generation.Text, w.config.MaxSummaryBytes); err != nil {
		w.logger.Warn("summary text rejected", "job_id", job.ID, "error", err)
		_ = w.jobs.Complete(context.WithoutCancel(ctx), job.ID, w.config.WorkerID, job.FencingToken)
		return
	}

	summary := SessionSummary{
		ID: uuid.New(), SessionID: task.SessionID, UserID: task.UserID, Version: task.BaseVersion + 1,
		SummaryText: strings.TrimSpace(generation.Text), SourceMessageIDs: task.SourceMessageIDs,
		SourceFingerprint: task.Fingerprint, CoveredMessageCount: task.CoveredMessageCount,
		Status: SummaryActive, ModelProviderID: generation.ProviderID, ModelID: generation.ModelID,
		InputTokens: generation.InputTokens, OutputTokens: generation.OutputTokens,
	}
	if len(source) > 0 {
		firstID, lastID := source[0].ID, source[len(source)-1].ID
		lastAt := source[len(source)-1].CreatedAt
		summary.SourceStartMessageID = &firstID
		summary.SourceEndMessageID = &lastID
		summary.SourceEndAt = &lastAt
	}
	if found {
		parentID := active.ID
		summary.ParentSummaryID = &parentID
	}
	event := OutboxEvent{
		UserID: task.UserID, EventType: "memory.summary.created", AggregateType: "session_summary", AggregateID: summary.ID.String(),
		DedupeKey: fmt.Sprintf("memory.summary.created:%s", summary.ID),
		Payload: map[string]any{
			"session_id": task.SessionID.String(), "version": summary.Version,
			"covered_messages": summary.CoveredMessageCount, "source_messages": len(task.SourceMessageIDs),
		},
	}
	saved, applied, err := w.summaries.SaveSummary(context.WithoutCancel(ctx), summary, task.BaseVersion, event)
	if err != nil {
		if errors.Is(err, ErrSummarySourceInvalid) {
			w.logger.Warn("summary source became invalid before save", "job_id", job.ID, "error", err)
			_ = w.jobs.Complete(context.WithoutCancel(ctx), job.ID, w.config.WorkerID, job.FencingToken)
			return
		}
		w.failJob(ctx, job, err)
		return
	}
	if !applied {
		w.logger.Info("summary version superseded; skipping save", "job_id", job.ID, "base_version", task.BaseVersion)
		_ = w.jobs.Complete(context.WithoutCancel(ctx), job.ID, w.config.WorkerID, job.FencingToken)
		return
	}
	w.logger.Info("summary version created", "session_id", task.SessionID, "version", saved.Version,
		"covered_messages", saved.CoveredMessageCount, "input_tokens", saved.InputTokens, "output_tokens", saved.OutputTokens)
	_ = w.jobs.Complete(context.WithoutCancel(ctx), job.ID, w.config.WorkerID, job.FencingToken)
}

// failJob 记录可重试失败并在预算内退避；超出预算由队列进入死信等待人工处理。
func (w *SummaryWorker) failJob(ctx context.Context, job Job, cause error) {
	delay := backoffDelay(job.Attempts)
	if err := w.jobs.Fail(context.WithoutCancel(ctx), job.ID, w.config.WorkerID, job.FencingToken,
		w.clock().Add(delay), sanitizeExecutionError(cause), w.clock()); err != nil {
		w.logger.Warn("fail summary job failed", "job_id", job.ID, "error", err)
	}
}

// validateSummarySource 校验任务来源：消息数量、内容指纹与结束边界必须完全匹配。
func validateSummarySource(task SummaryTask, source []Message) error {
	if len(source) == 0 || len(source) != len(task.SourceMessageIDs) {
		return fmt.Errorf("%w: expected %d messages, got %d", ErrSummarySourceInvalid, len(task.SourceMessageIDs), len(source))
	}
	if task.EndMessageID == uuid.Nil || task.EndAt.IsZero() {
		return fmt.Errorf("%w: missing end boundary", ErrSummarySourceInvalid)
	}
	last := source[len(source)-1]
	if last.ID != task.EndMessageID || !last.CreatedAt.Equal(task.EndAt) {
		return fmt.Errorf("%w: end boundary mismatch", ErrSummarySourceInvalid)
	}
	if SummarySourceFingerprint(source) != task.Fingerprint {
		return fmt.Errorf("%w: source fingerprint mismatch", ErrSummarySourceInvalid)
	}
	return nil
}

// decodeSummaryTask 解析任务载荷并校验必填字段。
func decodeSummaryTask(job Job) (SummaryTask, error) {
	userID, err := payloadUUID(job.Payload, "user_id")
	if err != nil {
		return SummaryTask{}, err
	}
	sessionID, err := payloadUUID(job.Payload, "session_id")
	if err != nil {
		return SummaryTask{}, err
	}
	baseVersion, ok := payloadInt64(job.Payload["base_version"])
	if !ok || baseVersion < 0 {
		return SummaryTask{}, fmt.Errorf("base_version is invalid")
	}
	covered, ok := payloadInt(job.Payload["covered_message_count"])
	if !ok || covered < 0 {
		return SummaryTask{}, fmt.Errorf("covered_message_count is invalid")
	}
	endID, err := payloadUUID(job.Payload, "end_message_id")
	if err != nil {
		return SummaryTask{}, err
	}
	endAt, err := payloadTime(job.Payload["end_at"])
	if err != nil {
		return SummaryTask{}, err
	}
	rawIDs, ok := job.Payload["source_message_ids"].([]any)
	if !ok || len(rawIDs) == 0 {
		return SummaryTask{}, fmt.Errorf("source_message_ids is invalid")
	}
	ids := make([]uuid.UUID, 0, len(rawIDs))
	for _, raw := range rawIDs {
		text, ok := raw.(string)
		if !ok {
			return SummaryTask{}, fmt.Errorf("source_message_ids contains a non-string value")
		}
		id, err := uuid.Parse(text)
		if err != nil {
			return SummaryTask{}, fmt.Errorf("source_message_ids contains an invalid id")
		}
		ids = append(ids, id)
	}
	fingerprint, _ := job.Payload["fingerprint"].(string)
	if len(fingerprint) != 64 {
		return SummaryTask{}, fmt.Errorf("fingerprint is invalid")
	}
	return SummaryTask{
		UserID: userID, SessionID: sessionID, BaseVersion: baseVersion,
		SourceMessageIDs: ids, Fingerprint: fingerprint,
		EndMessageID: endID, EndAt: endAt, CoveredMessageCount: covered,
	}, nil
}

// payloadUUID 读取载荷中的 UUID 字符串。
func payloadUUID(payload map[string]any, key string) (uuid.UUID, error) {
	text, ok := payload[key].(string)
	if !ok {
		return uuid.Nil, fmt.Errorf("%s is required", key)
	}
	id, err := uuid.Parse(text)
	if err != nil || id == uuid.Nil {
		return uuid.Nil, fmt.Errorf("%s is invalid", key)
	}
	return id, nil
}

// payloadInt 兼容进程内 int 与 JSON 反序列化后的 float64。
func payloadInt(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		return int(typed), typed == float64(int(typed))
	default:
		return 0, false
	}
}

// payloadInt64 读取 64 位整数载荷。
func payloadInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case int64:
		return typed, true
	case int:
		return int64(typed), true
	case float64:
		return int64(typed), typed == float64(int64(typed))
	default:
		return 0, false
	}
}

// payloadTime 读取 RFC3339 时间载荷。
func payloadTime(value any) (time.Time, error) {
	text, ok := value.(string)
	if !ok {
		return time.Time{}, fmt.Errorf("end_at is required")
	}
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return time.Time{}, fmt.Errorf("end_at is invalid")
	}
	return parsed.UTC(), nil
}

// 编译期断言摘要触发器实现。
var _ SummaryTrigger = (*SummaryPlanner)(nil)
