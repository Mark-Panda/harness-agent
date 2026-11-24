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
	"gorm.io/gorm/clause"
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

// lockJobForWrite 在结果写事务中锁定并复核租约，行锁持有到提交，封闭前置检查与写入之间的失租窗口。
func lockJobForWrite(tx *gorm.DB, fence biz.JobWriteFence) error {
	if fence.JobID == uuid.Nil || strings.TrimSpace(fence.Owner) == "" || fence.Token <= 0 {
		return fmt.Errorf("%w: valid job write fence is required", biz.ErrInvalidConfig)
	}
	var locked bool
	result := tx.Raw(`SELECT true FROM jobs
WHERE id = ? AND status = 'running' AND lease_owner = ? AND fencing_token = ?
  AND lease_expires_at > clock_timestamp()
FOR UPDATE`, fence.JobID, fence.Owner, fence.Token).Scan(&locked)
	if result.Error != nil {
		return result.Error
	}
	if !locked {
		return fmt.Errorf("%w: job lease is no longer valid", biz.ErrResourceConflict)
	}
	return nil
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
	err = WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		// dedupe_key 上是部分唯一索引，ON CONFLICT 不指定列才能匹配到该索引。
		result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&record)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected > 0 {
			return nil
		}
		// 已有同 Run 的 pending 任务：重复入队通常发生在“审批决策/恢复需要立即唤醒”场景。
		// 若旧任务被退避到了更晚的 available_at，直接返回会推迟唤醒；这里只在提前时前移。
		return tx.Model(&jobModel{}).
			Where("user_id = ? AND dedupe_key = ? AND status = ? AND available_at > ?", userID, dedupe, "pending", availableAt).
			Updates(map[string]any{"available_at": availableAt, "updated_at": time.Now().UTC()}).Error
	})
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		// Worker 重启或客户端重试可能重复入队，幂等键保证只有一个任务。
		return nil
	}
	return catalogError("enqueue run job", err)
}

// HasRunJob 检查幂等重试的 Run 是否已入队，避免重复创建任务。
func (r *JobRepository) HasRunJob(ctx context.Context, userID, runID uuid.UUID) (bool, error) {
	var count int64
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Model(&jobModel{}).
			Where("user_id = ? AND dedupe_key = ?", userID, "run:"+runID.String()).Count(&count).Error
	})
	return count > 0, catalogError("check run job", err)
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
				"payload": gorm.Expr("payload - 'run_settlement'"),
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
		// 去重键可能指向已终止但未产出摘要的旧任务（生成被永久拒绝或版本落后被跳过），
		// 仅“视为成功”会让同一来源范围被永久吸收，摘要再也无法推进；此处按尝试次数重新激活。
		return r.rearmTerminalSummaryJob(ctx, dedupe, availableAt)
	}
	return catalogError("enqueue summary job", err)
}

// rearmTerminalSummaryJob 把同一来源指纹下已终止的摘要任务重新置为待处理。
// attempts 保留并由领取流程递增，达到 max_attempts 后不再激活，避免永久失败的会话无限调用模型。
func (r *JobRepository) rearmTerminalSummaryJob(ctx context.Context, dedupe string, availableAt time.Time) error {
	now := time.Now().UTC()
	if availableAt.Before(now) {
		availableAt = now
	}
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		return tx.Exec(`
UPDATE jobs SET status = 'pending',
  available_at = GREATEST(?::timestamptz, ?::timestamptz + (LEAST(attempts, 10) * interval '1 minute')),
  lease_owner = '', lease_expires_at = NULL, last_error = '', updated_at = ?
WHERE dedupe_key = ? AND status IN ('completed', 'failed', 'dead') AND attempts < max_attempts`,
			availableAt, now, now, dedupe).Error
	})
	return catalogError("rearm summary job", err)
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
		ID             uuid.UUID
		UserID         *uuid.UUID
		JobType        string
		Payload        datatypes.JSON
		Attempts       int
		MaxAttempts    int
		FencingToken   int64
		LeaseOwner     string
		SettlementOnly bool
	}
	var record claimed
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		// 先锁定可领取行，再更新租约与 fencing token；SKIP LOCKED 避免多 Worker 阻塞。
		query := `
WITH candidate AS (
  SELECT j.id, (j.dedupe_key IS NOT NULL AND j.dedupe_key LIKE 'run:%' AND
    (j.attempts >= j.max_attempts OR j.payload->'run_settlement' IS NOT NULL OR j.status = 'dead')) AS settlement_only
  FROM jobs AS j
  WHERE j.job_type = ? AND (
    (j.status = 'pending' AND j.available_at <= ?)
    OR (j.status = 'running' AND (j.lease_expires_at IS NULL OR j.lease_expires_at < ?))
    OR (j.status = 'dead' AND j.dedupe_key LIKE 'run:%' AND EXISTS (
      SELECT 1 FROM agent_runs AS ar WHERE ar.user_id = j.user_id
        AND j.dedupe_key = 'run:' || ar.id::text
        AND ar.status IN ('queued', 'running', 'waiting_approval')
    ))
  )
  ORDER BY j.available_at, j.created_at, j.id
  FOR UPDATE SKIP LOCKED
  LIMIT 1
)
UPDATE jobs SET status = 'running',
  attempts = attempts + CASE WHEN candidate.settlement_only THEN 0 ELSE 1 END, lease_owner = ?,
  lease_expires_at = ?, fencing_token = fencing_token + 1, updated_at = ?
FROM candidate WHERE jobs.id = candidate.id
RETURNING jobs.id, jobs.user_id, jobs.job_type, jobs.payload, jobs.attempts, jobs.max_attempts,
  jobs.fencing_token, jobs.lease_owner, candidate.settlement_only`
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
	job := biz.Job{
		ID: record.ID, UserID: record.UserID, Type: record.JobType, Payload: payload,
		Attempts: record.Attempts, MaxAttempts: record.MaxAttempts, FencingToken: record.FencingToken,
		LeaseOwner: record.LeaseOwner, AvailableAt: now,
		RunSettlementOnly: record.SettlementOnly,
	}
	if record.SettlementOnly {
		var meta struct {
			Settlement *biz.RunJobSettlement `json:"run_settlement"`
		}
		// A malformed intent is treated as an unknown outcome by the settlement worker.
		if err := json.Unmarshal(record.Payload, &meta); err == nil && meta.Settlement != nil {
			switch meta.Settlement.Status {
			case biz.RunCompleted, biz.RunFailed, biz.RunReconciliationRequired:
				job.RunSettlement = meta.Settlement
			}
		}
	}
	return job, true, nil
}

// Renew 续约任务租约；租约过期或已被其他 Worker 接管时返回冲突。
func (r *JobRepository) Renew(ctx context.Context, jobID uuid.UUID, workerID string, fencingToken int64, lease time.Duration, now time.Time) error {
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		result := tx.Exec(`
WITH locked_job AS MATERIALIZED (
  SELECT id, lease_expires_at FROM jobs
  WHERE id = ? AND status = 'running' AND lease_owner = ? AND fencing_token = ?
  FOR UPDATE
)
UPDATE jobs SET lease_expires_at = ?, updated_at = ?
FROM locked_job
WHERE jobs.id = locked_job.id AND locked_job.lease_expires_at > GREATEST(?::timestamptz, clock_timestamp())`,
			jobID, workerID, fencingToken, now.Add(lease), now, now)
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
				"payload":      gorm.Expr("CASE WHEN dedupe_key LIKE 'run:%' THEN payload - 'run_settlement' ELSE payload END"),
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

// DeferRunSettlement 保留 Run 的终态重试信息，使用当前 Job 栅栏退回队列。
func (r *JobRepository) DeferRunSettlement(ctx context.Context, job biz.Job, settlement *biz.RunJobSettlement, retryAt, now time.Time) error {
	if job.UserID == nil || *job.UserID == uuid.Nil {
		return fmt.Errorf("%w: run settlement requires a job owner", biz.ErrInvalidConfig)
	}
	fields := map[string]any{
		"status": "pending", "available_at": retryAt, "lease_owner": "", "lease_expires_at": nil, "updated_at": now,
	}
	if settlement != nil {
		payload, err := json.Marshal(settlement)
		if err != nil {
			return err
		}
		fields["payload"] = gorm.Expr("jsonb_set(payload, '{run_settlement}', CAST(? AS jsonb))", string(payload))
	}
	err := WithScope(ctx, r.db, Scope{UserID: *job.UserID}, func(tx *gorm.DB) error {
		result := tx.Model(&jobModel{}).
			Where("id = ? AND user_id = ? AND status = 'running' AND lease_owner = ? AND fencing_token = ?",
				job.ID, *job.UserID, job.LeaseOwner, job.FencingToken).Updates(fields)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("%w: job lease is no longer valid", biz.ErrResourceConflict)
		}
		return nil
	})
	return catalogError("defer run settlement", err)
}

// Fail 记录失败：预算内退回 pending 并按指数退避延后，超出预算进入 dead 等待人工重放。
func (r *JobRepository) Fail(ctx context.Context, jobID uuid.UUID, workerID string, fencingToken int64, retryAt time.Time, reason string, now time.Time) error {
	if len(reason) > 1024 {
		reason = reason[:1024]
	}
	if retryAt.IsZero() {
		retryAt = now
	}
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		result := tx.Model(&jobModel{}).
			Where("id = ? AND status = 'running' AND lease_owner = ? AND fencing_token = ?", jobID, workerID, fencingToken).
			Updates(map[string]any{
				"status":           gorm.Expr("CASE WHEN attempts >= max_attempts THEN 'dead' ELSE 'pending' END"),
				"available_at":     gorm.Expr("CASE WHEN attempts < max_attempts THEN ? ELSE available_at END", retryAt),
				"completed_at":     gorm.Expr("CASE WHEN attempts >= max_attempts THEN ? ELSE completed_at END", now),
				"last_error":       reason,
				"lease_owner":      "",
				"lease_expires_at": nil,
				"updated_at":       now,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("%w: job lease is no longer valid", biz.ErrResourceConflict)
		}
		return nil
	})
	return catalogError("fail job", err)
}

// 编译期断言执行队列接口实现。
var _ biz.RunExecutionQueue = (*JobRepository)(nil)

// 编译期断言 Run 入队端口实现。
var _ biz.RunEnqueuer = (*JobRepository)(nil)

var _ biz.RunJobLookup = (*JobRepository)(nil)

// 编译期断言摘要任务入队端口实现。
var _ biz.SummaryJobQueue = (*JobRepository)(nil)

// 编译期断言向量任务入队端口实现。
var _ biz.MemoryJobQueue = (*JobRepository)(nil)

// 编译期断言事实任务入队端口实现。
var _ biz.FactJobQueue = (*JobRepository)(nil)

// 编译期断言飞书事件任务入队端口实现。
var _ biz.FeishuEventEnqueuer = (*JobRepository)(nil)
