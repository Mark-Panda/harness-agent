package data

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// jobModel 对应 jobs 表：持久化异步任务与租约状态。
type jobModel struct {
	// ID 任务主键。
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// UserID 任务归属用户，系统任务可为空。
	UserID *uuid.UUID `gorm:"type:uuid"`
	// JobType 任务类型。
	JobType string
	// DedupeKey 幂等去重键，同一键只允许一个任务。
	DedupeKey *string
	// Payload 任务载荷 JSON。
	Payload datatypes.JSON
	// Status 任务状态：pending/running/completed/failed/dead。
	Status string
	// Attempts 已领取次数。
	Attempts int
	// MaxAttempts 最大尝试次数。
	MaxAttempts int
	// AvailableAt 下次可领取时间。
	AvailableAt time.Time
	// LeaseOwner 当前租约持有者。
	LeaseOwner string
	// LeaseExpiresAt 租约到期时间。
	LeaseExpiresAt *time.Time
	// FencingToken 租约栅栏令牌。
	FencingToken int64
	// LastError 最近一次失败原因。
	LastError string
	// CreatedAt 创建时间。
	CreatedAt time.Time
	// UpdatedAt 更新时间。
	UpdatedAt time.Time
	// CompletedAt 完成时间。
	CompletedAt *time.Time
}

// TableName 指定任务表名。
func (jobModel) TableName() string { return "jobs" }

// JobRepository 是 jobs 表的 GORM 实现，使用 SKIP LOCKED 支持多 Worker 并发认领。
type JobRepository struct {
	db *gorm.DB
}

// NewJobRepository 构造任务仓库。
func NewJobRepository(db *gorm.DB) *JobRepository {
	return &JobRepository{db: db}
}

// EnqueueRun 幂等写入 Run 执行任务；去重键为 run:<run_id>，重复入队直接视为成功。
func (r *JobRepository) EnqueueRun(ctx context.Context, userID, runID uuid.UUID, availableAt time.Time) error {
	if userID == uuid.Nil || runID == uuid.Nil {
		return fmt.Errorf("%w: run job requires user and run ids", biz.ErrInvalidConfig)
	}
	if availableAt.IsZero() {
		availableAt = time.Now().UTC()
	}
	payload, err := json.Marshal(map[string]any{"run_id": runID.String(), "user_id": userID.String()})
	if err != nil {
		return err
	}
	dedupe := "run:" + runID.String()
	record := jobModel{
		ID: uuid.New(), UserID: &userID, JobType: "agent.run", DedupeKey: &dedupe,
		Payload: datatypes.JSON(payload), Status: "pending", MaxAttempts: 8,
		AvailableAt: availableAt, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	err = WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		return tx.Create(&record).Error
	})
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		// Worker 重启或客户端重试可能重复入队，幂等键保证只有一个任务。
		return nil
	}
	return catalogError("enqueue run job", err)
}

// RequeueRun 把同一 Run 的失败或死信任务重置为 pending，供人工重放。
func (r *JobRepository) RequeueRun(ctx context.Context, runID uuid.UUID, now time.Time) error {
	if runID == uuid.Nil {
		return fmt.Errorf("%w: run id is required", biz.ErrInvalidConfig)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	dedupe := "run:" + runID.String()
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		result := tx.Model(&jobModel{}).
			Where("dedupe_key = ? AND status IN ?", dedupe, []string{"failed", "dead"}).
			Updates(map[string]any{
				"status": "pending", "attempts": 0, "available_at": now, "updated_at": now,
				"lease_owner": "", "lease_expires_at": nil, "last_error": "",
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return fmt.Errorf("%w: no failed run job to replay", biz.ErrResourceConflict)
		}
		return nil
	})
	return catalogError("requeue run job", err)
}

// EnqueueSummary 幂等写入异步摘要任务；去重键绑定会话与来源指纹，同一来源范围只执行一次。
func (r *JobRepository) EnqueueSummary(ctx context.Context, task biz.SummaryTask, availableAt time.Time) error {
	if task.UserID == uuid.Nil || task.SessionID == uuid.Nil || task.EndMessageID == uuid.Nil {
		return fmt.Errorf("%w: summary job requires user, session and end message ids", biz.ErrInvalidConfig)
	}
	if len(task.SourceMessageIDs) == 0 || task.Fingerprint == "" {
		return fmt.Errorf("%w: summary job requires source messages and fingerprint", biz.ErrInvalidConfig)
	}
	if availableAt.IsZero() {
		availableAt = time.Now().UTC()
	}
	ids := make([]string, 0, len(task.SourceMessageIDs))
	for _, id := range task.SourceMessageIDs {
		ids = append(ids, id.String())
	}
	payload, err := json.Marshal(map[string]any{
		"user_id": task.UserID.String(), "session_id": task.SessionID.String(),
		"base_version": task.BaseVersion, "source_message_ids": ids,
		"fingerprint": task.Fingerprint, "end_message_id": task.EndMessageID.String(),
		"end_at": task.EndAt.UTC().Format(time.RFC3339Nano), "covered_message_count": task.CoveredMessageCount,
	})
	if err != nil {
		return err
	}
	dedupe := "summary:" + task.SessionID.String() + ":" + task.Fingerprint
	record := jobModel{
		ID: uuid.New(), UserID: &task.UserID, JobType: biz.SummaryJobType, DedupeKey: &dedupe,
		Payload: datatypes.JSON(payload), Status: "pending", MaxAttempts: 8,
		AvailableAt: availableAt, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	err = WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		return tx.Create(&record).Error
	})
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		// 同一来源范围的重复触发直接视为成功，任务由已有记录继续处理。
		return nil
	}
	return catalogError("enqueue summary job", err)
}

// EnqueueMemory 幂等写入向量化任务；去重键绑定会话、最后一条消息与索引版本组合。
func (r *JobRepository) EnqueueMemory(ctx context.Context, task biz.MemoryTask, availableAt time.Time) error {
	if task.UserID == uuid.Nil || task.SessionID == uuid.Nil || len(task.MessageIDs) == 0 || len(task.Versions) == 0 {
		return fmt.Errorf("%w: memory job requires user, session, messages and versions", biz.ErrInvalidConfig)
	}
	if availableAt.IsZero() {
		availableAt = time.Now().UTC()
	}
	ids := make([]string, 0, len(task.MessageIDs))
	for _, id := range task.MessageIDs {
		ids = append(ids, id.String())
	}
	payload, err := json.Marshal(map[string]any{
		"user_id": task.UserID.String(), "session_id": task.SessionID.String(),
		"message_ids": ids, "versions": task.Versions,
	})
	if err != nil {
		return err
	}
	dedupe := fmt.Sprintf("memory:%s:%s:%x", task.SessionID, task.MessageIDs[len(task.MessageIDs)-1], versionHash(task.Versions))
	record := jobModel{
		ID: uuid.New(), UserID: &task.UserID, JobType: biz.MemoryEmbedJobType, DedupeKey: &dedupe,
		Payload: datatypes.JSON(payload), Status: "pending", MaxAttempts: 8,
		AvailableAt: availableAt, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	err = WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		return tx.Create(&record).Error
	})
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		// 同一批消息与版本的重复触发直接视为成功。
		return nil
	}
	return catalogError("enqueue memory job", err)
}

// versionHash 生成索引版本组合的稳定短哈希。
func versionHash(versions []string) []byte {
	sum := sha256.Sum256([]byte(strings.Join(versions, "\x00")))
	return sum[:8]
}

// EnqueueFacts 幂等写入事实提取任务；去重键绑定会话、最后消息与内容指纹。
func (r *JobRepository) EnqueueFacts(ctx context.Context, task biz.FactTask, availableAt time.Time) error {
	if task.UserID == uuid.Nil || task.SessionID == uuid.Nil || len(task.MessageIDs) == 0 || task.Fingerprint == "" {
		return fmt.Errorf("%w: fact job requires user, session, messages and fingerprint", biz.ErrInvalidConfig)
	}
	if availableAt.IsZero() {
		availableAt = time.Now().UTC()
	}
	ids := make([]string, 0, len(task.MessageIDs))
	for _, id := range task.MessageIDs {
		ids = append(ids, id.String())
	}
	payload := map[string]any{
		"user_id": task.UserID.String(), "session_id": task.SessionID.String(),
		"message_ids": ids, "fingerprint": task.Fingerprint,
	}
	// 群聊话题事实写入发言人私有记忆：显式保存写入用户与发言人过滤键。
	if task.FactOwnerID != uuid.Nil {
		payload["fact_owner_id"] = task.FactOwnerID.String()
	}
	if task.SpeakerOpenID != "" {
		payload["speaker_open_id"] = task.SpeakerOpenID
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	// 去重键包含事实归属用户，避免同一会话不同发言人的任务互相吞并。
	dedupe := fmt.Sprintf("fact:%s:%s:%s:%s", task.SessionID, task.MessageIDs[len(task.MessageIDs)-1], task.FactOwnerID, task.Fingerprint[:16])
	record := jobModel{
		ID: uuid.New(), UserID: &task.UserID, JobType: biz.FactExtractJobType, DedupeKey: &dedupe,
		Payload: datatypes.JSON(encoded), Status: "pending", MaxAttempts: 8,
		AvailableAt: availableAt, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	err = WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		return tx.Create(&record).Error
	})
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return nil
	}
	return catalogError("enqueue fact job", err)
}

// EnqueueFeishuEvent 幂等写入飞书事件消费任务；去重键绑定 event_id，重复入队直接视为成功。
func (r *JobRepository) EnqueueFeishuEvent(ctx context.Context, eventID string, availableAt time.Time) error {
	if eventID == "" {
		return fmt.Errorf("%w: feishu event job requires event id", biz.ErrInvalidConfig)
	}
	if availableAt.IsZero() {
		availableAt = time.Now().UTC()
	}
	payload, err := json.Marshal(map[string]any{"event_id": eventID})
	if err != nil {
		return err
	}
	dedupe := "feishu.event:" + eventID
	record := jobModel{
		ID: uuid.New(), JobType: biz.FeishuEventJobType, DedupeKey: &dedupe,
		Payload: datatypes.JSON(payload), Status: "pending", MaxAttempts: 8,
		AvailableAt: availableAt, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	err = WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		return tx.Create(&record).Error
	})
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		// 事件重复投递或上次入库后入队失败，已有任务继续处理即可。
		return nil
	}
	return catalogError("enqueue feishu event job", err)
}

// Claim 使用 SKIP LOCKED 领取一个到期任务；running 且租约过期的任务允许恢复。
func (r *JobRepository) Claim(ctx context.Context, workerID string, jobType string, lease time.Duration, now time.Time) (biz.Job, bool, error) {
	if workerID == "" || lease <= 0 || jobType == "" {
		return biz.Job{}, false, fmt.Errorf("%w: worker, lease and job types are required", biz.ErrInvalidConfig)
	}
	type claimed struct {
		ID           uuid.UUID
		UserID       *uuid.UUID
		JobType      string
		Payload      datatypes.JSON
		Attempts     int
		MaxAttempts  int
		FencingToken int64
		LeaseOwner   string
	}
	var record claimed
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		// 先锁定可领取行，再更新租约与 fencing token；SKIP LOCKED 避免多 Worker 阻塞。
		query := `
WITH candidate AS (
  SELECT id FROM jobs
  WHERE job_type = ? AND (
    (status = 'pending' AND available_at <= ?)
    OR (status = 'running' AND (lease_expires_at IS NULL OR lease_expires_at < ?))
  )
  ORDER BY available_at, created_at, id
  FOR UPDATE SKIP LOCKED
  LIMIT 1
)
UPDATE jobs SET status = 'running', attempts = attempts + 1, lease_owner = ?,
  lease_expires_at = ?, fencing_token = fencing_token + 1, updated_at = ?
FROM candidate WHERE jobs.id = candidate.id
RETURNING jobs.id, jobs.user_id, jobs.job_type, jobs.payload, jobs.attempts, jobs.max_attempts,
  jobs.fencing_token, jobs.lease_owner`
		return tx.Raw(query, jobType, now, now, workerID, now.Add(lease), now).Scan(&record).Error
	})
	if err != nil {
		return biz.Job{}, false, catalogError("claim job", err)
	}
	if record.ID == uuid.Nil {
		return biz.Job{}, false, nil
	}
	var payload map[string]any
	if err := json.Unmarshal(record.Payload, &payload); err != nil {
		return biz.Job{}, false, fmt.Errorf("decode job payload: %w", err)
	}
	return biz.Job{
		ID: record.ID, UserID: record.UserID, Type: record.JobType, Payload: payload,
		Attempts: record.Attempts, MaxAttempts: record.MaxAttempts, FencingToken: record.FencingToken,
		LeaseOwner: record.LeaseOwner, AvailableAt: now,
	}, true, nil
}

// Renew 续约任务租约；租约已被其他 Worker 接管时返回冲突。
func (r *JobRepository) Renew(ctx context.Context, jobID uuid.UUID, workerID string, fencingToken int64, lease time.Duration, now time.Time) error {
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		result := tx.Model(&jobModel{}).
			Where("id = ? AND status = 'running' AND lease_owner = ? AND fencing_token = ?", jobID, workerID, fencingToken).
			Updates(map[string]any{"lease_expires_at": now.Add(lease), "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("%w: job lease is no longer valid", biz.ErrResourceConflict)
		}
		return nil
	})
	return catalogError("renew job lease", err)
}

// Release 把尚未开始执行的任务退回 pending，并回退本次领取计数，避免舱壁排队消耗重试预算。
func (r *JobRepository) Release(ctx context.Context, jobID uuid.UUID, workerID string, fencingToken int64, retryAt time.Time, now time.Time) error {
	if retryAt.IsZero() {
		retryAt = now
	}
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		result := tx.Model(&jobModel{}).
			Where("id = ? AND status = 'running' AND lease_owner = ? AND fencing_token = ?", jobID, workerID, fencingToken).
			Updates(map[string]any{
				"status": "pending", "attempts": gorm.Expr("GREATEST(attempts - 1, 0)"),
				"available_at": retryAt, "lease_owner": "", "lease_expires_at": nil, "updated_at": now,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("%w: job lease is no longer valid", biz.ErrResourceConflict)
		}
		return nil
	})
	return catalogError("release job", err)
}

// Complete 标记任务完成；已完成或租约失效的任务不再修改。
func (r *JobRepository) Complete(ctx context.Context, jobID uuid.UUID, workerID string, fencingToken int64) error {
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		result := tx.Model(&jobModel{}).
			Where("id = ? AND status = 'running' AND lease_owner = ? AND fencing_token = ?", jobID, workerID, fencingToken).
			Updates(map[string]any{"status": "completed", "lease_owner": "", "lease_expires_at": nil,
				"completed_at": time.Now().UTC(), "updated_at": time.Now().UTC()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("%w: job lease is no longer valid", biz.ErrResourceConflict)
		}
		return nil
	})
	return catalogError("complete job", err)
}

// Fail 记录失败：预算内退回 pending 并按指数退避延后，超出预算进入 dead 等待人工重放。
func (r *JobRepository) Fail(ctx context.Context, jobID uuid.UUID, workerID string, fencingToken int64, retryAt time.Time, reason string, now time.Time) error {
	if len(reason) > 1024 {
		reason = reason[:1024]
	}
	status := "pending"
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		var record jobModel
		if err := tx.First(&record, "id = ? AND status = 'running' AND lease_owner = ? AND fencing_token = ?",
			jobID, workerID, fencingToken).Error; err != nil {
			return err
		}
		if record.Attempts >= record.MaxAttempts {
			status = "dead"
		}
		updates := map[string]any{
			"status": status, "last_error": reason, "lease_owner": "", "lease_expires_at": nil, "updated_at": now,
		}
		if status == "pending" {
			if retryAt.IsZero() {
				retryAt = now
			}
			updates["available_at"] = retryAt
		} else {
			updates["completed_at"] = now
		}
		return tx.Model(&jobModel{}).Where("id = ?", jobID).Updates(updates).Error
	})
	return catalogError("fail job", err)
}

// 编译期断言接口实现。
var _ biz.JobQueue = (*JobRepository)(nil)

// 编译期断言 Run 入队端口实现。
var _ biz.RunEnqueuer = (*JobRepository)(nil)

// 编译期断言摘要任务入队端口实现。
var _ biz.SummaryJobQueue = (*JobRepository)(nil)

// 编译期断言向量任务入队端口实现。
var _ biz.MemoryJobQueue = (*JobRepository)(nil)

// 编译期断言事实任务入队端口实现。
var _ biz.FactJobQueue = (*JobRepository)(nil)

// 编译期断言飞书事件任务入队端口实现。
var _ biz.FeishuEventEnqueuer = (*JobRepository)(nil)
