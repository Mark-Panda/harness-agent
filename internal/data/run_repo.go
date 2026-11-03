package data

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// runModel 对应 agent_runs 表：一次执行的快照与状态。
type runModel struct {
	// ID Run 主键。
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// SessionID 所属会话。
	SessionID uuid.UUID `gorm:"type:uuid"`
	// UserID 所属用户，RLS 隔离键。
	UserID uuid.UUID `gorm:"type:uuid"`
	// AgentID 使用的 Agent 版本。
	AgentID *uuid.UUID `gorm:"type:uuid"`
	// AgentVersion Agent 版本号。
	AgentVersion *int
	// Mode 编排模式。
	Mode string
	// Status 生命周期状态。
	Status string
	// ConfigSnapshot Agent 配置快照 JSON。
	ConfigSnapshot datatypes.JSON
	// ModelPolicySnapshot 模型策略快照 JSON。
	ModelPolicySnapshot datatypes.JSON
	// CurrentStepSeq 当前步骤序号。
	CurrentStepSeq int64
	// AttemptNo 当前模型尝试序号。
	AttemptNo int
	// IdempotencyKey 创建幂等键。
	IdempotencyKey *string
	// ErrorCode 失败业务码。
	ErrorCode string
	// ErrorMessage 脱敏失败原因。
	ErrorMessage string
	// DeadlineAt Run 截止时间。
	DeadlineAt *time.Time
	// StartedAt 开始时间。
	StartedAt *time.Time
	// FinishedAt 结束时间。
	FinishedAt *time.Time
	// LeaseOwner 当前持有执行租约的 Worker 标识。
	LeaseOwner string
	// LeaseExpiresAt 租约到期时间。
	LeaseExpiresAt *time.Time
	// FencingToken 租约栅栏令牌，每次认领递增。
	FencingToken int64
	// CreatedAt 创建时间。
	CreatedAt time.Time
	// UpdatedAt 更新时间。
	UpdatedAt time.Time
}

// TableName 指定 Run 表名。
func (runModel) TableName() string { return "agent_runs" }

// runEventModel 对应 run_events 表：可重放事件时间线。
type runEventModel struct {
	// ID 事件行主键。
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// RunID 所属 Run。
	RunID uuid.UUID `gorm:"type:uuid"`
	// UserID 所属用户。
	UserID uuid.UUID `gorm:"type:uuid"`
	// Seq Run 内单调序号。
	Seq int64
	// EventID 事件唯一标识。
	EventID uuid.UUID `gorm:"type:uuid"`
	// EventType 事件类型。
	EventType string
	// SchemaVersion 载荷版本。
	SchemaVersion int
	// Payload 事件载荷 JSON。
	Payload datatypes.JSON
	// OccurredAt 发生时间。
	OccurredAt time.Time
}

// TableName 指定事件表名。
func (runEventModel) TableName() string { return "run_events" }

// RunRepository 是 Run 与事件的 GORM 实现，所有查询强制携带 user_id。
type RunRepository struct {
	db *gorm.DB
}

// NewRunRepository 构造 Run 仓库。
func NewRunRepository(db *gorm.DB) *RunRepository {
	return &RunRepository{db: db}
}

// CreateRun 在用户作用域内创建 Run；幂等键冲突时返回既有 Run。
func (r *RunRepository) CreateRun(ctx context.Context, run biz.Run) (biz.Run, error) {
	record, err := fromBizRun(run)
	if err != nil {
		return biz.Run{}, err
	}
	err = WithScope(ctx, r.db, Scope{UserID: run.UserID}, func(tx *gorm.DB) error {
		return tx.Create(&record).Error
	})
	if errors.Is(err, gorm.ErrDuplicatedKey) && run.IdempotencyKey != "" {
		// 同键重复创建返回第一次的结果，保证客户端重试安全。
		return r.findRunByIdempotencyKey(ctx, run.UserID, run.IdempotencyKey)
	}
	if err != nil {
		return biz.Run{}, catalogError("create run", err)
	}
	return toBizRun(record)
}

// findRunByIdempotencyKey 按用户与幂等键读取既有 Run。
func (r *RunRepository) findRunByIdempotencyKey(ctx context.Context, userID uuid.UUID, key string) (biz.Run, error) {
	var record runModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.First(&record, "user_id = ? AND idempotency_key = ?", userID, key).Error
	})
	if err != nil {
		return biz.Run{}, catalogError("get idempotent run", err)
	}
	return toBizRun(record)
}

// GetRun 按用户与 Run ID 读取。
func (r *RunRepository) GetRun(ctx context.Context, userID, runID uuid.UUID) (biz.Run, error) {
	var record runModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.First(&record, "id = ? AND user_id = ?", runID, userID).Error
	})
	if err != nil {
		return biz.Run{}, catalogError("get run", err)
	}
	return toBizRun(record)
}

// ListRuns 按创建时间与 ID 降序游标分页。
func (r *RunRepository) ListRuns(ctx context.Context, userID, sessionID uuid.UUID, limit int, cursor string) (biz.Page[biz.Run], error) {
	decoded, err := DecodeCursor(cursor)
	if err != nil {
		return biz.Page[biz.Run]{}, fmt.Errorf("%w: invalid run cursor", biz.ErrInvalidConfig)
	}
	limit = NormalizeLimit(limit)
	var records []runModel
	err = WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		query := tx.Where("user_id = ? AND session_id = ?", userID, sessionID)
		if decoded.ID != uuid.Nil {
			query = query.Where("(created_at < ? OR (created_at = ? AND id < ?))", decoded.CreatedAt, decoded.CreatedAt, decoded.ID)
		}
		return query.Order("created_at DESC, id DESC").Limit(limit + 1).Find(&records).Error
	})
	if err != nil {
		return biz.Page[biz.Run]{}, catalogError("list runs", err)
	}
	page := biz.Page[biz.Run]{Items: make([]biz.Run, 0, min(limit, len(records)))}
	if len(records) > limit {
		records = records[:limit]
		last := records[len(records)-1]
		page.NextCursor, err = EncodeCursor(Cursor{CreatedAt: last.CreatedAt, ID: last.ID})
		if err != nil {
			return biz.Page[biz.Run]{}, err
		}
	}
	for _, record := range records {
		run, err := toBizRun(record)
		if err != nil {
			return biz.Page[biz.Run]{}, err
		}
		page.Items = append(page.Items, run)
	}
	return page, nil
}

// TransitionRun 以状态 CAS 推进 Run，非法迁移由领域状态机拒绝。
func (r *RunRepository) TransitionRun(ctx context.Context, userID, runID uuid.UUID, from, to biz.RunStatus, fields map[string]any) (biz.Run, error) {
	var record runModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		if err := tx.First(&record, "id = ? AND user_id = ?", runID, userID).Error; err != nil {
			return err
		}
		if biz.RunStatus(record.Status) != from {
			return fmt.Errorf("%w: run status changed concurrently", biz.ErrResourceConflict)
		}
		if err := biz.TransitionRunStatus(from, to); err != nil {
			return err
		}
		updates := map[string]any{"status": string(to), "updated_at": time.Now().UTC()}
		for key, value := range fields {
			updates[key] = value
		}
		result := tx.Model(&runModel{}).Where("id = ? AND user_id = ? AND status = ?", runID, userID, string(from)).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("%w: run status changed concurrently", biz.ErrResourceConflict)
		}
		return tx.First(&record, "id = ? AND user_id = ?", runID, userID).Error
	})
	if err != nil {
		return biz.Run{}, catalogError("transition run", err)
	}
	return toBizRun(record)
}

// ClaimRun 认领排队中或租约已过期的 Run，并在同一事务内递增 fencing token。
// 认领规则：queued 可直接执行；running 仅在租约缺失或过期时允许恢复，避免抢占健康 Worker。
func (r *RunRepository) ClaimRun(ctx context.Context, userID, runID uuid.UUID, owner string, lease time.Duration, now time.Time) (biz.Run, biz.RunFence, error) {
	if owner == "" || lease <= 0 {
		return biz.Run{}, biz.RunFence{}, fmt.Errorf("%w: worker owner and positive lease are required", biz.ErrInvalidConfig)
	}
	var record runModel
	leaseUntil := now.Add(lease)
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		result := tx.Model(&runModel{}).
			Where("id = ? AND user_id = ? AND (status = ? OR (status = ? AND (lease_expires_at IS NULL OR lease_expires_at < ?)))",
				runID, userID, string(biz.RunQueued), string(biz.RunRunning), now).
			Updates(map[string]any{
				"status": string(biz.RunRunning), "lease_owner": owner, "lease_expires_at": leaseUntil,
				"fencing_token": gorm.Expr("fencing_token + 1"),
				"started_at":    gorm.Expr("COALESCE(started_at, ?)", now), "updated_at": now,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("%w: run is not claimable", biz.ErrResourceConflict)
		}
		return tx.First(&record, "id = ? AND user_id = ?", runID, userID).Error
	})
	if err != nil {
		return biz.Run{}, biz.RunFence{}, catalogError("claim run", err)
	}
	run, err := toBizRun(record)
	if err != nil {
		return biz.Run{}, biz.RunFence{}, err
	}
	return run, biz.RunFence{Owner: record.LeaseOwner, Token: record.FencingToken}, nil
}

// RenewRunLease 续约执行租约；仅租约持有者且 token 匹配时成功。
func (r *RunRepository) RenewRunLease(ctx context.Context, userID, runID uuid.UUID, fence biz.RunFence, lease time.Duration, now time.Time) error {
	if fence.Owner == "" || fence.Token <= 0 || lease <= 0 {
		return fmt.Errorf("%w: valid fence and positive lease are required", biz.ErrInvalidConfig)
	}
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		result := tx.Model(&runModel{}).
			Where("id = ? AND user_id = ? AND status = ? AND lease_owner = ? AND fencing_token = ?",
				runID, userID, string(biz.RunRunning), fence.Owner, fence.Token).
			Updates(map[string]any{"lease_expires_at": now.Add(lease), "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("%w: run lease is no longer valid", biz.ErrResourceConflict)
		}
		return nil
	})
	return catalogError("renew run lease", err)
}

// PauseRun 在栅栏有效时把 running 推进到 waiting_approval，清空租约并在同一事务写入 run.paused 事件。
// 重复暂停幂等返回既有记录，避免 Worker 在审批竞态下重复写事件。
func (r *RunRepository) PauseRun(ctx context.Context, userID, runID uuid.UUID, fence biz.RunFence, now time.Time) (biz.Run, error) {
	if fence.Owner == "" || fence.Token <= 0 {
		return biz.Run{}, fmt.Errorf("%w: valid fence is required to pause a run", biz.ErrInvalidConfig)
	}
	var record runModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		result := tx.Model(&runModel{}).
			Where("id = ? AND user_id = ? AND status = ? AND lease_owner = ? AND fencing_token = ?",
				runID, userID, string(biz.RunRunning), fence.Owner, fence.Token).
			Updates(map[string]any{
				"status": string(biz.RunWaitingApproval), "lease_owner": "", "lease_expires_at": nil, "updated_at": now,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			// 并发暂停或审批已决策时读取最新状态：已是等待审批则幂等成功。
			if err := tx.First(&record, "id = ? AND user_id = ?", runID, userID).Error; err != nil {
				return err
			}
			if biz.RunStatus(record.Status) == biz.RunWaitingApproval {
				return nil
			}
			return fmt.Errorf("%w: run cannot be paused from its current state", biz.ErrResourceConflict)
		}
		if _, err := appendRunEvent(tx, biz.RunEvent{
			RunID: runID, UserID: userID, EventType: "run.paused",
			Payload: map[string]any{"reason": "approval_required"}, OccurredAt: now,
		}); err != nil {
			return err
		}
		return tx.First(&record, "id = ? AND user_id = ?", runID, userID).Error
	})
	if err != nil {
		return biz.Run{}, catalogError("pause run", err)
	}
	return toBizRun(record)
}

// ResumeRun 在审批全部决策后把 waiting_approval 推进到 running，并按暂停时长顺延截止时间。
// 人工审批等待不应消耗 Run 的执行时长预算，否则长时间未决策会把合法 Run 判为超时。
func (r *RunRepository) ResumeRun(ctx context.Context, userID, runID uuid.UUID, now time.Time) (biz.Run, error) {
	var record runModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		result := tx.Model(&runModel{}).
			Where("id = ? AND user_id = ? AND status = ?", runID, userID, string(biz.RunWaitingApproval)).
			Updates(map[string]any{
				"status": string(biz.RunRunning), "lease_owner": "", "lease_expires_at": nil, "updated_at": now,
				// 右值读取更新前的 updated_at，等于把审批等待时长补回执行预算。
				"deadline_at": gorm.Expr("CASE WHEN deadline_at IS NULL THEN NULL ELSE deadline_at + (? - updated_at) END", now),
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			// 已恢复或已被取消时读取最新状态；已是 running 则幂等返回。
			if err := tx.First(&record, "id = ? AND user_id = ?", runID, userID).Error; err != nil {
				return err
			}
			if biz.RunStatus(record.Status) == biz.RunRunning {
				return nil
			}
			return fmt.Errorf("%w: run cannot be resumed from its current state", biz.ErrResourceConflict)
		}
		if _, err := appendRunEvent(tx, biz.RunEvent{
			RunID: runID, UserID: userID, EventType: "run.resumed",
			Payload: map[string]any{"reason": "approval_decided"}, OccurredAt: now,
		}); err != nil {
			return err
		}
		return tx.First(&record, "id = ? AND user_id = ?", runID, userID).Error
	})
	if err != nil {
		return biz.Run{}, catalogError("resume run", err)
	}
	return toBizRun(record)
}

// MarkRunReconciliationRequired 在副作用结果未知时把 Run 推进到人工对账状态。
// 该状态不是终态，但必须停止自动执行，禁止盲目重试可能已发生的副作用。
func (r *RunRepository) MarkRunReconciliationRequired(ctx context.Context, userID, runID uuid.UUID, fence biz.RunFence, code, message string, now time.Time) (biz.Run, error) {
	if fence.Owner == "" || fence.Token <= 0 {
		return biz.Run{}, fmt.Errorf("%w: valid fence is required to reconcile a run", biz.ErrInvalidConfig)
	}
	if len(message) > 512 {
		message = message[:512]
	}
	var record runModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		result := tx.Model(&runModel{}).
			Where("id = ? AND user_id = ? AND status = ? AND lease_owner = ? AND fencing_token = ?",
				runID, userID, string(biz.RunRunning), fence.Owner, fence.Token).
			Updates(map[string]any{
				"status": string(biz.RunReconciliationRequired), "error_code": code, "error_message": message,
				"lease_owner": "", "lease_expires_at": nil, "updated_at": now,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			if err := tx.First(&record, "id = ? AND user_id = ?", runID, userID).Error; err != nil {
				return err
			}
			if biz.RunStatus(record.Status) == biz.RunReconciliationRequired {
				return nil
			}
			return fmt.Errorf("%w: run cannot enter reconciliation from its current state", biz.ErrResourceConflict)
		}
		if _, err := appendRunEvent(tx, biz.RunEvent{
			RunID: runID, UserID: userID, EventType: "run.reconciliation_required",
			Payload: map[string]any{"code": code, "message": message}, OccurredAt: now,
		}); err != nil {
			return err
		}
		return tx.First(&record, "id = ? AND user_id = ?", runID, userID).Error
	})
	if err != nil {
		return biz.Run{}, catalogError("mark run reconciliation required", err)
	}
	return toBizRun(record)
}

// FinishRun 在栅栏有效时推进终态；状态迁移仍由领域状态机校验。
func (r *RunRepository) FinishRun(ctx context.Context, userID, runID uuid.UUID, from, to biz.RunStatus, fields map[string]any, fence biz.RunFence, now time.Time) (biz.Run, error) {
	return r.finishRun(ctx, userID, runID, from, to, fields, nil, fence, now)
}

// FinishRunWithEvent 在同一事务内推进终态并写入事件；事件失败会回滚状态变更。
func (r *RunRepository) FinishRunWithEvent(ctx context.Context, userID, runID uuid.UUID, from, to biz.RunStatus, fields map[string]any, event biz.RunEvent, fence biz.RunFence, now time.Time) (biz.Run, error) {
	event.RunID = runID
	event.UserID = userID
	return r.finishRun(ctx, userID, runID, from, to, fields, &event, fence, now)
}

// finishRun 是终态提交的共享实现；event 非空时与状态变更同事务写入。
func (r *RunRepository) finishRun(ctx context.Context, userID, runID uuid.UUID, from, to biz.RunStatus, fields map[string]any, event *biz.RunEvent, fence biz.RunFence, now time.Time) (biz.Run, error) {
	if err := biz.TransitionRunStatus(from, to); err != nil {
		return biz.Run{}, err
	}
	var record runModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		updates := map[string]any{"status": string(to), "finished_at": now, "updated_at": now, "lease_owner": "", "lease_expires_at": nil}
		for key, value := range fields {
			updates[key] = value
		}
		result := tx.Model(&runModel{}).
			Where("id = ? AND user_id = ? AND status = ? AND lease_owner = ? AND fencing_token = ?",
				runID, userID, string(from), fence.Owner, fence.Token).
			Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("%w: run lease or status changed concurrently", biz.ErrResourceConflict)
		}
		// 事件与状态同事务写入；任何事件写入失败都会回滚终态，避免幽灵事件。
		if event != nil {
			if _, err := appendRunEvent(tx, *event); err != nil {
				return err
			}
		}
		return tx.First(&record, "id = ? AND user_id = ?", runID, userID).Error
	})
	if err != nil {
		return biz.Run{}, catalogError("finish run", err)
	}
	return toBizRun(record)
}

// SetRunAttempt 在栅栏有效时更新当前 Attempt 序号，保证崩溃恢复后序号连续。
func (r *RunRepository) SetRunAttempt(ctx context.Context, userID, runID uuid.UUID, attemptNo int, fence biz.RunFence) error {
	if attemptNo < 0 {
		return fmt.Errorf("%w: attempt number cannot be negative", biz.ErrInvalidConfig)
	}
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		result := tx.Model(&runModel{}).
			Where("id = ? AND user_id = ? AND status = ? AND lease_owner = ? AND fencing_token = ?",
				runID, userID, string(biz.RunRunning), fence.Owner, fence.Token).
			Updates(map[string]any{"attempt_no": attemptNo, "updated_at": time.Now().UTC()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("%w: run lease is no longer valid", biz.ErrResourceConflict)
		}
		return nil
	})
	return catalogError("set run attempt", err)
}

// AppendEvent 在 Run 行锁内分配单调 seq 后写入事件，避免并发产生序号空洞或冲突。
func (r *RunRepository) AppendEvent(ctx context.Context, event biz.RunEvent) (biz.RunEvent, error) {
	var record runEventModel
	err := WithScope(ctx, r.db, Scope{UserID: event.UserID}, func(tx *gorm.DB) error {
		var err error
		record, err = appendRunEvent(tx, event)
		return err
	})
	if err != nil {
		return biz.RunEvent{}, catalogError("append run event", err)
	}
	return toBizRunEvent(record)
}

// AppendEventFenced 在租约栅栏有效时追加事件；过期 Worker 写入时返回冲突。
func (r *RunRepository) AppendEventFenced(ctx context.Context, event biz.RunEvent, fence biz.RunFence) (biz.RunEvent, error) {
	var record runEventModel
	err := WithScope(ctx, r.db, Scope{UserID: event.UserID}, func(tx *gorm.DB) error {
		result := tx.Model(&runModel{}).
			Where("id = ? AND user_id = ? AND lease_owner = ? AND fencing_token = ? AND status IN ?",
				event.RunID, event.UserID, fence.Owner, fence.Token, []string{string(biz.RunRunning), string(biz.RunWaitingApproval)}).
			Update("updated_at", time.Now().UTC())
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("%w: run lease is no longer valid", biz.ErrResourceConflict)
		}
		var err error
		record, err = appendRunEvent(tx, event)
		return err
	})
	if err != nil {
		return biz.RunEvent{}, catalogError("append fenced run event", err)
	}
	return toBizRunEvent(record)
}

// appendRunEvent 在事务内分配单调 seq 并插入事件行，供普通与栅栏写入复用。
func appendRunEvent(tx *gorm.DB, event biz.RunEvent) (runEventModel, error) {
	payload, err := json.Marshal(nonNilMap(event.Payload))
	if err != nil {
		return runEventModel{}, err
	}
	if event.EventID == uuid.Nil {
		event.EventID = uuid.New()
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	}
	if event.SchemaVersion <= 0 {
		event.SchemaVersion = 1
	}
	// 以 run_id 作为 advisory lock，串行化同一 Run 的事件序号分配。
	if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtext(?))", event.RunID.String()).Error; err != nil {
		return runEventModel{}, err
	}
	var maximum int64
	if err := tx.Model(&runEventModel{}).Where("run_id = ?", event.RunID).
		Select("COALESCE(MAX(seq), 0)").Scan(&maximum).Error; err != nil {
		return runEventModel{}, err
	}
	record := runEventModel{
		ID: uuid.New(), RunID: event.RunID, UserID: event.UserID, Seq: maximum + 1, EventID: event.EventID,
		EventType: event.EventType, SchemaVersion: event.SchemaVersion, Payload: datatypes.JSON(payload), OccurredAt: event.OccurredAt,
	}
	if err := tx.Create(&record).Error; err != nil {
		return runEventModel{}, err
	}
	return record, nil
}

// ListEvents 按 seq 升序读取 afterSeq 之后的事件，limit 上限 200。
func (r *RunRepository) ListEvents(ctx context.Context, userID, runID uuid.UUID, afterSeq int64, limit int) ([]biz.RunEvent, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	var records []runEventModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Where("user_id = ? AND run_id = ? AND seq > ?", userID, runID, afterSeq).
			Order("seq").Limit(limit).Find(&records).Error
	})
	if err != nil {
		return nil, catalogError("list run events", err)
	}
	events := make([]biz.RunEvent, 0, len(records))
	for _, record := range records {
		event, err := toBizRunEvent(record)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, nil
}

// fromBizRun 将领域 Run 转换为数据库行。
func fromBizRun(run biz.Run) (runModel, error) {
	config, err := json.Marshal(nonNilMap(run.ConfigSnapshot))
	if err != nil {
		return runModel{}, err
	}
	policy, err := json.Marshal(nonNilMap(run.ModelPolicySnapshot))
	if err != nil {
		return runModel{}, err
	}
	record := runModel{
		ID: run.ID, SessionID: run.SessionID, UserID: run.UserID, AgentID: run.AgentID, AgentVersion: run.AgentVersion,
		Mode: string(run.Mode), Status: string(run.Status), ConfigSnapshot: datatypes.JSON(config), ModelPolicySnapshot: datatypes.JSON(policy),
		CurrentStepSeq: run.CurrentStepSeq, AttemptNo: run.AttemptNo, ErrorCode: run.ErrorCode, ErrorMessage: run.ErrorMessage,
		DeadlineAt: run.DeadlineAt, StartedAt: run.StartedAt, FinishedAt: run.FinishedAt,
	}
	if run.IdempotencyKey != "" {
		key := run.IdempotencyKey
		record.IdempotencyKey = &key
	}
	return record, nil
}

// toBizRun 将数据库行转换为领域 Run。
func toBizRun(record runModel) (biz.Run, error) {
	var config map[string]any
	if err := json.Unmarshal(record.ConfigSnapshot, &config); err != nil {
		return biz.Run{}, err
	}
	var policy map[string]any
	if err := json.Unmarshal(record.ModelPolicySnapshot, &policy); err != nil {
		return biz.Run{}, err
	}
	run := biz.Run{
		ID: record.ID, SessionID: record.SessionID, UserID: record.UserID, AgentID: record.AgentID, AgentVersion: record.AgentVersion,
		Mode: biz.AgentMode(record.Mode), Status: biz.RunStatus(record.Status), ConfigSnapshot: config, ModelPolicySnapshot: policy,
		CurrentStepSeq: record.CurrentStepSeq, AttemptNo: record.AttemptNo, ErrorCode: record.ErrorCode, ErrorMessage: record.ErrorMessage,
		DeadlineAt: record.DeadlineAt, StartedAt: record.StartedAt, FinishedAt: record.FinishedAt,
		LeaseOwner: record.LeaseOwner, LeaseExpiresAt: record.LeaseExpiresAt, FencingToken: record.FencingToken,
		CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
	}
	if record.IdempotencyKey != nil {
		run.IdempotencyKey = *record.IdempotencyKey
	}
	return run, nil
}

// toBizRunEvent 将数据库行转换为领域事件并解码载荷。
func toBizRunEvent(record runEventModel) (biz.RunEvent, error) {
	var payload map[string]any
	if err := json.Unmarshal(record.Payload, &payload); err != nil {
		return biz.RunEvent{}, err
	}
	return biz.RunEvent{
		RunID: record.RunID, UserID: record.UserID, Seq: record.Seq, EventID: record.EventID,
		EventType: record.EventType, SchemaVersion: record.SchemaVersion, Payload: payload, OccurredAt: record.OccurredAt,
	}, nil
}

var _ biz.RunRepository = (*RunRepository)(nil)
