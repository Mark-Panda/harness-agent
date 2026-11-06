package biz

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Job 是持久化异步任务，Worker 以租约与 fencing token 完成至少一次消费。
type Job struct {
	// ID 任务唯一标识。
	ID uuid.UUID
	// UserID 任务归属用户；系统任务可为空。
	UserID *uuid.UUID
	// Type 任务类型，如 agent.run。
	Type string
	// Payload 任务载荷，必须可 JSON 序列化且不含凭证。
	Payload map[string]any
	// Attempts 已领取次数，领取时递增。
	Attempts int
	// MaxAttempts 最大尝试次数，超过后进入死信。
	MaxAttempts int
	// FencingToken 租约栅栏令牌，每次领取递增。
	FencingToken int64
	// LeaseOwner 当前持有租约的 Worker 标识。
	LeaseOwner string
	// AvailableAt 下次可领取时间。
	AvailableAt time.Time
	// CreatedAt 创建时间（UTC）。
	CreatedAt time.Time
}

// JobQueue 定义持久化任务队列契约，所有操作均为至少一次语义。
type JobQueue interface {
	// EnqueueRun 幂等入队 Run 执行任务；同一 Run 重复入队不产生重复任务。
	EnqueueRun(ctx context.Context, userID, runID uuid.UUID, availableAt time.Time) error
	// Claim 领取一个指定类型的到期任务并递增 attempt 与 fencing token；无任务时返回 false。
	Claim(ctx context.Context, workerID string, jobType string, lease time.Duration, now time.Time) (Job, bool, error)
	// Renew 续约任务租约；栅栏失效时返回冲突。
	Renew(ctx context.Context, jobID uuid.UUID, workerID string, fencingToken int64, lease time.Duration, now time.Time) error
	// Release 在尚未开始执行时把任务退回 pending，且不计入失败尝试次数。
	Release(ctx context.Context, jobID uuid.UUID, workerID string, fencingToken int64, retryAt time.Time, now time.Time) error
	// Complete 标记任务成功完成。
	Complete(ctx context.Context, jobID uuid.UUID, workerID string, fencingToken int64) error
	// Fail 记录失败并在预算内退避重试，超出预算进入死信。
	Fail(ctx context.Context, jobID uuid.UUID, workerID string, fencingToken int64, retryAt time.Time, reason string, now time.Time) error
}

// RunJobReplayer 是可选的任务重放能力：把同一 Run 的 failed/dead 任务重置为 pending。
type RunJobReplayer interface {
	// RequeueRun 重置 Run 执行任务并清空租约与失败计数。
	RequeueRun(ctx context.Context, runID uuid.UUID, now time.Time) error
}

// RunEnqueuer 是 Run 创建后的入队端口，供 RunService 解耦具体队列实现。
type RunEnqueuer interface {
	// EnqueueRun 幂等入队 Run 执行任务。
	EnqueueRun(ctx context.Context, userID, runID uuid.UUID, availableAt time.Time) error
}
