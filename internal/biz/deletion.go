package biz

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// 删除任务错误。
var (
	// ErrDeletionUnsupported 不支持的删除资源类型或状态。
	ErrDeletionUnsupported = errors.New("deletion request is unsupported")
)

// DeletionResourceType 是异步删除的资源类型。
type DeletionResourceType string

// DeletionStatus 是删除请求生命周期状态。
type DeletionStatus string

const (
	// DeletionResourceSession 删除单个会话及其全部派生数据。
	DeletionResourceSession DeletionResourceType = "session"
	// DeletionResourceMemory 删除用户全部长期记忆与摘要。
	DeletionResourceMemory DeletionResourceType = "memory"
	// DeletionResourceAccount 删除账号及其全部数据。
	DeletionResourceAccount DeletionResourceType = "account"
	// DeletionPending 等待执行。
	DeletionPending DeletionStatus = "pending"
	// DeletionRunning 正在执行。
	DeletionRunning DeletionStatus = "running"
	// DeletionCompleted 已完成。
	DeletionCompleted DeletionStatus = "completed"
	// DeletionFailed 超出重试预算，等待人工处理。
	DeletionFailed DeletionStatus = "failed"
)

// DeletionRequest 是一次异步删除编排记录。
type DeletionRequest struct {
	// ID 删除请求唯一标识。
	ID uuid.UUID
	// UserID 所属用户。
	UserID uuid.UUID
	// ResourceType 资源类型：session/memory/account。
	ResourceType DeletionResourceType
	// ResourceID 资源标识；会话删除为会话 ID，记忆删除为用户 ID。
	ResourceID string
	// Status 生命周期状态。
	Status DeletionStatus
	// RequestedBy 请求来源。
	RequestedBy string
	// RequestedSource 请求渠道：web/api/admin。
	RequestedSource string
	// Attempts 已领取次数。
	Attempts int
	// MaxAttempts 最大尝试次数。
	MaxAttempts int
	// FencingToken 租约栅栏令牌。
	FencingToken int64
	// LeaseOwner 当前租约持有者。
	LeaseOwner string
	// AvailableAt 下次可领取时间。
	AvailableAt time.Time
	// LastError 最近失败原因（脱敏）。
	LastError string
	// CreatedAt 创建时间。
	CreatedAt time.Time
	// UpdatedAt 更新时间。
	UpdatedAt time.Time
	// CompletedAt 完成时间。
	CompletedAt *time.Time
}

// DeletionRepository 定义删除请求持久化与租约契约。
type DeletionRepository interface {
	// Create 幂等创建删除请求；同一用户同一资源已有待处理请求时返回既有记录。
	Create(ctx context.Context, request DeletionRequest, event OutboxEvent) (DeletionRequest, error)
	// Get 按用户与请求 ID 读取。
	Get(ctx context.Context, userID, requestID uuid.UUID) (DeletionRequest, error)
	// List 按创建时间倒序分页列出当前用户删除请求。
	List(ctx context.Context, userID uuid.UUID, limit int, cursor string) (Page[DeletionRequest], error)
	// Claim 使用 SKIP LOCKED 领取待处理或租约过期的删除请求。
	Claim(ctx context.Context, workerID string, lease time.Duration, now time.Time) (DeletionRequest, bool, error)
	// Renew 续约删除请求租约；栅栏失效时返回冲突。
	Renew(ctx context.Context, requestID uuid.UUID, workerID string, fencingToken int64, lease time.Duration, now time.Time) error
	// Complete 标记完成并写入审计事件。
	Complete(ctx context.Context, requestID uuid.UUID, workerID string, fencingToken int64, now time.Time, event OutboxEvent) error
	// Fail 记录失败并在预算内退避，超出预算进入 failed。
	Fail(ctx context.Context, requestID uuid.UUID, workerID string, fencingToken int64, retryAt time.Time, reason string, now time.Time) error
}

// DeletionExecutor 执行实际数据清理；实现必须幂等、可重试且保持用户隔离。
type DeletionExecutor interface {
	// DeleteSession 清理会话、消息、Run、摘要、事实、向量、外部绑定与缓存。
	DeleteSession(ctx context.Context, userID, sessionID uuid.UUID) error
	// DeleteMemory 清理用户全部长期记忆、摘要与缓存，不删除会话消息。
	DeleteMemory(ctx context.Context, userID uuid.UUID) error
	// DeleteAccount 清理账号全部业务数据并匿名化用户行，保留审计所需外键。
	DeleteAccount(ctx context.Context, userID uuid.UUID) error
}

// DeletionService 创建与查询异步删除请求。
type DeletionService struct {
	repo     DeletionRepository
	sessions SessionRepository
	clock    func() time.Time
	logger   *slog.Logger
}

// NewDeletionService 构造删除编排服务。
func NewDeletionService(repo DeletionRepository, sessions SessionRepository, logger *slog.Logger, clock func() time.Time) (*DeletionService, error) {
	if repo == nil || sessions == nil {
		return nil, invalid("deletion service requires repository and sessions")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &DeletionService{repo: repo, sessions: sessions, clock: clock, logger: logger}, nil
}

// RequestSessionDeletion 创建会话删除请求；会话必须属于当前用户且未处于删除中。
func (s *DeletionService) RequestSessionDeletion(ctx context.Context, principal Principal, sessionID uuid.UUID, source string) (DeletionRequest, error) {
	if principal.UserID == uuid.Nil || sessionID == uuid.Nil {
		return DeletionRequest{}, ErrForbidden
	}
	if _, err := s.sessions.GetSession(ctx, principal.UserID, sessionID); err != nil {
		return DeletionRequest{}, err
	}
	request := DeletionRequest{
		ID: uuid.New(), UserID: principal.UserID, ResourceType: DeletionResourceSession,
		ResourceID: sessionID.String(), Status: DeletionPending, RequestedBy: principal.UserID.String(),
		RequestedSource: normalizeDeletionSource(source), MaxAttempts: 5, AvailableAt: s.clock(), CreatedAt: s.clock(),
	}
	event := OutboxEvent{
		UserID: principal.UserID, EventType: "deletion.requested", AggregateType: "deletion_request", AggregateID: request.ID.String(),
		DedupeKey: fmt.Sprintf("deletion.requested:%s", request.ID),
		Payload:   map[string]any{"resource_type": string(request.ResourceType), "resource_id": request.ResourceID},
	}
	return s.repo.Create(ctx, request, event)
}

// RequestMemoryDeletion 创建用户长期记忆删除请求。
func (s *DeletionService) RequestMemoryDeletion(ctx context.Context, principal Principal, source string) (DeletionRequest, error) {
	if principal.UserID == uuid.Nil {
		return DeletionRequest{}, ErrForbidden
	}
	request := DeletionRequest{
		ID: uuid.New(), UserID: principal.UserID, ResourceType: DeletionResourceMemory,
		ResourceID: principal.UserID.String(), Status: DeletionPending, RequestedBy: principal.UserID.String(),
		RequestedSource: normalizeDeletionSource(source), MaxAttempts: 5, AvailableAt: s.clock(), CreatedAt: s.clock(),
	}
	event := OutboxEvent{
		UserID: principal.UserID, EventType: "deletion.requested", AggregateType: "deletion_request", AggregateID: request.ID.String(),
		DedupeKey: fmt.Sprintf("deletion.requested:%s", request.ID),
		Payload:   map[string]any{"resource_type": string(request.ResourceType), "resource_id": request.ResourceID},
	}
	return s.repo.Create(ctx, request, event)
}

// RequestAccountDeletion 创建账号级删除请求；执行后业务数据清空、用户行匿名化。
func (s *DeletionService) RequestAccountDeletion(ctx context.Context, principal Principal, source string) (DeletionRequest, error) {
	if principal.UserID == uuid.Nil {
		return DeletionRequest{}, ErrForbidden
	}
	request := DeletionRequest{
		ID: uuid.New(), UserID: principal.UserID, ResourceType: DeletionResourceAccount,
		ResourceID: principal.UserID.String(), Status: DeletionPending, RequestedBy: principal.UserID.String(),
		RequestedSource: normalizeDeletionSource(source), MaxAttempts: 5, AvailableAt: s.clock(), CreatedAt: s.clock(),
	}
	event := OutboxEvent{
		UserID: principal.UserID, EventType: "deletion.requested", AggregateType: "deletion_request", AggregateID: request.ID.String(),
		DedupeKey: fmt.Sprintf("deletion.requested:%s", request.ID),
		Payload:   map[string]any{"resource_type": string(request.ResourceType), "resource_id": request.ResourceID},
	}
	return s.repo.Create(ctx, request, event)
}

// Get 读取当前用户的删除请求状态。
func (s *DeletionService) Get(ctx context.Context, principal Principal, requestID uuid.UUID) (DeletionRequest, error) {
	if principal.UserID == uuid.Nil || requestID == uuid.Nil {
		return DeletionRequest{}, ErrForbidden
	}
	return s.repo.Get(ctx, principal.UserID, requestID)
}

// List 列出当前用户的删除请求。
func (s *DeletionService) List(ctx context.Context, principal Principal, limit int, cursor string) (Page[DeletionRequest], error) {
	if principal.UserID == uuid.Nil {
		return Page[DeletionRequest]{}, ErrForbidden
	}
	return s.repo.List(ctx, principal.UserID, limit, cursor)
}

// normalizeDeletionSource 归一化请求来源，未知来源记为 web。
func normalizeDeletionSource(source string) string {
	switch strings.TrimSpace(source) {
	case "api":
		return "api"
	case "admin":
		return "admin"
	default:
		return "web"
	}
}

// DeletionWorkerConfig 是删除 Worker 配置。
type DeletionWorkerConfig struct {
	// WorkerID 实例标识。
	WorkerID string
	// Concurrency 单实例并发删除任务数。
	Concurrency int
	// PollInterval 无任务时的轮询间隔。
	PollInterval time.Duration
	// Lease 任务租约时长。
	Lease time.Duration
	// Heartbeat 租约续约间隔。
	Heartbeat time.Duration
}

// normalize 补齐删除 Worker 默认配置。
func (c DeletionWorkerConfig) normalize() DeletionWorkerConfig {
	if c.Concurrency <= 0 {
		c.Concurrency = 1
	}
	if c.PollInterval <= 0 {
		c.PollInterval = time.Second
	}
	if c.Lease <= 0 {
		c.Lease = 5 * time.Minute
	}
	if c.Heartbeat <= 0 || c.Heartbeat >= c.Lease {
		c.Heartbeat = c.Lease / 3
	}
	return c
}

// DeletionWorkerDependencies 聚合删除 Worker 依赖。
type DeletionWorkerDependencies struct {
	// Requests 删除请求仓储。
	Requests DeletionRepository
	// Executor 实际数据清理执行器。
	Executor DeletionExecutor
	// Logger 结构化日志。
	Logger *slog.Logger
	// Clock 当前时间函数。
	Clock func() time.Time
}

// DeletionWorker 认领删除请求并执行清理，失败按退避重试直到预算耗尽。
type DeletionWorker struct {
	requests DeletionRepository
	executor DeletionExecutor
	config   DeletionWorkerConfig
	clock    func() time.Time
	logger   *slog.Logger
}

// NewDeletionWorker 构造删除 Worker。
func NewDeletionWorker(dependencies DeletionWorkerDependencies, config DeletionWorkerConfig) (*DeletionWorker, error) {
	if dependencies.Requests == nil || dependencies.Executor == nil {
		return nil, invalid("deletion worker requires requests and executor")
	}
	if strings.TrimSpace(config.WorkerID) == "" {
		return nil, invalid("deletion worker id is required")
	}
	logger := dependencies.Logger
	if logger == nil {
		logger = slog.Default()
	}
	clock := dependencies.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &DeletionWorker{requests: dependencies.Requests, executor: dependencies.Executor,
		config: config.normalize(), clock: clock, logger: logger}, nil
}

// Start 启动配置数量的删除协程。
func (w *DeletionWorker) Start(ctx context.Context) {
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

// loop 持续认领删除请求直到上下文取消。
func (w *DeletionWorker) loop(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if _, err := w.ProcessOne(ctx); err != nil && ctx.Err() == nil {
			w.logger.Warn("claim deletion request failed", "error", err)
		}
		timer.Reset(w.config.PollInterval)
	}
}

// ProcessOne 认领并处理至多一个删除请求。
func (w *DeletionWorker) ProcessOne(ctx context.Context) (bool, error) {
	request, claimed, err := w.requests.Claim(ctx, w.config.WorkerID, w.config.Lease, w.clock())
	if err != nil {
		return false, err
	}
	if !claimed {
		return false, nil
	}
	w.handle(ctx, request)
	return true, nil
}

// handle 按资源类型执行清理，成功写完成事件，失败按预算退避。
func (w *DeletionWorker) handle(ctx context.Context, request DeletionRequest) {
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
			if err := w.requests.Renew(runCtx, request.ID, w.config.WorkerID, request.FencingToken, w.config.Lease, w.clock()); err != nil {
				// 主流程结束时主动 cancel 会让在途续约返回 context.Canceled；
				// 只有上下文仍有效时的续约失败才代表租约真的丢失。
				if runCtx.Err() == nil {
					close(leaseLost)
					cancel()
				}
				return
			}
		}
	}()

	var err error
	switch request.ResourceType {
	case DeletionResourceSession:
		sessionID, parseErr := uuid.Parse(request.ResourceID)
		if parseErr != nil {
			err = fmt.Errorf("%w: invalid session id", ErrInvalidConfig)
			break
		}
		err = w.executor.DeleteSession(runCtx, request.UserID, sessionID)
	case DeletionResourceMemory:
		err = w.executor.DeleteMemory(runCtx, request.UserID)
	case DeletionResourceAccount:
		err = w.executor.DeleteAccount(runCtx, request.UserID)
	default:
		err = fmt.Errorf("%w: %s", ErrDeletionUnsupported, request.ResourceType)
	}
	cancel()
	// 先等待心跳协程退出再判定租约：心跳可能在 cancel 之后才写入 leaseLost，
	// 提前判定会把正常结束误判为丢租约并跳过完成写入。
	<-done
	select {
	case <-leaseLost:
		w.logger.Warn("deletion request lease lost", "request_id", request.ID)
		return
	default:
	}

	if err != nil {
		if errors.Is(err, ErrDeletionUnsupported) {
			w.logger.Warn("deletion request is unsupported; failing permanently", "request_id", request.ID, "error", err)
			_ = w.requests.Fail(context.WithoutCancel(ctx), request.ID, w.config.WorkerID, request.FencingToken,
				w.clock(), sanitizeExecutionError(err), w.clock())
			return
		}
		retryAt := w.clock().Add(backoffDelay(request.Attempts))
		if failErr := w.requests.Fail(context.WithoutCancel(ctx), request.ID, w.config.WorkerID, request.FencingToken,
			retryAt, sanitizeExecutionError(err), w.clock()); failErr != nil {
			w.logger.Warn("record deletion failure failed", "request_id", request.ID, "error", failErr)
		}
		return
	}
	event := OutboxEvent{
		UserID: request.UserID, EventType: "deletion.completed", AggregateType: "deletion_request", AggregateID: request.ID.String(),
		DedupeKey: fmt.Sprintf("deletion.completed:%s", request.ID),
		Payload:   map[string]any{"resource_type": string(request.ResourceType), "resource_id": request.ResourceID},
	}
	if err := w.requests.Complete(context.WithoutCancel(ctx), request.ID, w.config.WorkerID, request.FencingToken, w.clock(), event); err != nil {
		w.logger.Warn("complete deletion request failed", "request_id", request.ID, "error", err)
		return
	}
	w.logger.Info("deletion request completed", "request_id", request.ID, "resource_type", request.ResourceType)
}
