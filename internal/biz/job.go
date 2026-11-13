package biz

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// JobWriteFence 标识允许提交任务结果的租约；数据层必须在写事务内复核，不能只依赖 Worker 前置检查。
type JobWriteFence struct {
	// JobID 任务唯一标识。
	JobID uuid.UUID
	// Owner 当前租约持有者。
	Owner string
	// Token 当前 fencing token。
	Token int64
}

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
	// RunSettlementOnly 表示本次领取仅用于收敛 Run，未占用新的执行次数。
	RunSettlementOnly bool
	// RunSettlement 是先前写入失败的 Run 状态与事件。
	RunSettlement *RunJobSettlement
}

func newJobWriteFence(job Job, workerID string) JobWriteFence {
	return JobWriteFence{JobID: job.ID, Owner: workerID, Token: job.FencingToken}
}

// RunJobSettlement 保存终态重试所需的信息，避免重新执行模型或工具。
type RunJobSettlement struct {
	Status  RunStatus      `json:"status"`
	Code    string         `json:"code,omitempty"`
	Message string         `json:"message,omitempty"`
	Payload map[string]any `json:"payload,omitempty"`
}

// JobQueue 定义持久化任务队列契约，所有操作均为至少一次语义。
type JobQueue interface {
	// EnqueueRun 幂等入队 Run 执行任务；同一 Run 重复入队不产生重复任务。
	EnqueueRun(ctx context.Context, userID, runID uuid.UUID, availableAt time.Time) error
	// Claim 领取一个指定类型的到期任务并递增 fencing token；只收敛 Run 时不增加 attempts。
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

// jobLeaseGuard 在任务执行期间持续续约；续约失败会取消执行上下文并记录失租原因。
type jobLeaseGuard struct {
	cancel context.CancelFunc
	done   chan struct{}
	lost   chan struct{}
	err    error
}

// beginJobLease 启动与 summary Worker 相同语义的心跳：主动停止续约造成的取消不算失租。
func beginJobLease(ctx context.Context, jobs JobQueue, job Job, workerID string, lease, heartbeat time.Duration, clock func() time.Time) (context.Context, *jobLeaseGuard) {
	runCtx, cancel := context.WithCancel(ctx)
	guard := &jobLeaseGuard{cancel: cancel, done: make(chan struct{}), lost: make(chan struct{})}
	go func() {
		defer close(guard.done)
		ticker := time.NewTicker(heartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
			}
			if err := jobs.Renew(runCtx, job.ID, workerID, job.FencingToken, lease, clock()); err != nil {
				if runCtx.Err() == nil {
					guard.err = err
					close(guard.lost)
					cancel()
				}
				return
			}
		}
	}()
	return runCtx, guard
}

// check 在执行关键写入前非阻塞检查租约。
func (g *jobLeaseGuard) check() error {
	select {
	case <-g.lost:
		return g.err
	default:
		return nil
	}
}

// stop 等待心跳退出后再次检查租约，避免在途续约被漏判；重复调用安全。
func (g *jobLeaseGuard) stop() error {
	g.cancel()
	<-g.done
	return g.check()
}

// RunExecutionQueue 支持独立于执行预算重试 Run 的终态写入。
type RunExecutionQueue interface {
	JobQueue
	// DeferRunSettlement 退回 Run 收敛任务而不更改执行计数。settlement 为空时保留已有信息。
	DeferRunSettlement(ctx context.Context, job Job, settlement *RunJobSettlement, retryAt, now time.Time) error
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

// RunJobLookup lets an idempotent Create distinguish an existing job from a Run that still needs enqueue.
type RunJobLookup interface {
	HasRunJob(ctx context.Context, userID, runID uuid.UUID) (bool, error)
}
