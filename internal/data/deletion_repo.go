package data

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// deletionRequestModel 对应 deletion_requests 表：异步删除编排记录。
type deletionRequestModel struct {
	// ID 请求主键。
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// UserID 所属用户。
	UserID uuid.UUID `gorm:"type:uuid"`
	// ResourceType 资源类型。
	ResourceType string
	// ResourceID 资源标识。
	ResourceID string
	// Status 生命周期状态。
	Status string
	// RequestedBy 请求来源。
	RequestedBy string
	// RequestedSource 请求渠道。
	RequestedSource string
	// Attempts 已领取次数。
	Attempts int
	// MaxAttempts 最大尝试次数。
	MaxAttempts int
	// AvailableAt 下次可领取时间。
	AvailableAt time.Time
	// LeaseOwner 租约持有者。
	LeaseOwner string
	// LeaseExpiresAt 租约到期时间。
	LeaseExpiresAt *time.Time
	// FencingToken 租约栅栏令牌。
	FencingToken int64
	// LastError 最近失败原因。
	LastError string
	// CreatedAt 创建时间。
	CreatedAt time.Time
	// UpdatedAt 更新时间。
	UpdatedAt time.Time
	// CompletedAt 完成时间。
	CompletedAt *time.Time
}

// TableName 指定删除请求表名。
func (deletionRequestModel) TableName() string { return "deletion_requests" }

// DeletionRepository 是删除请求的 GORM 实现，创建/查询按用户隔离，领取使用系统作用域。
type DeletionRepository struct {
	db *gorm.DB
}

// NewDeletionRepository 构造删除请求仓库。
func NewDeletionRepository(db *gorm.DB) *DeletionRepository {
	return &DeletionRepository{db: db}
}

// Create 幂等创建删除请求；同一用户同一资源已有待处理请求时返回既有记录。
func (r *DeletionRepository) Create(ctx context.Context, request biz.DeletionRequest, event biz.OutboxEvent) (biz.DeletionRequest, error) {
	if request.UserID == uuid.Nil || request.ID == uuid.Nil || request.ResourceID == "" {
		return biz.DeletionRequest{}, fmt.Errorf("%w: deletion request identity is required", biz.ErrInvalidConfig)
	}
	record := deletionRequestModel{
		ID: request.ID, UserID: request.UserID, ResourceType: string(request.ResourceType),
		ResourceID: request.ResourceID, Status: string(biz.DeletionPending), RequestedBy: request.RequestedBy,
		RequestedSource: request.RequestedSource, Attempts: 0, MaxAttempts: request.MaxAttempts,
		AvailableAt: request.AvailableAt, CreatedAt: request.CreatedAt, UpdatedAt: request.CreatedAt,
	}
	err := WithScope(ctx, r.db, Scope{UserID: request.UserID}, func(tx *gorm.DB) error {
		if err := tx.Create(&record).Error; err != nil {
			return err
		}
		return appendOutbox(tx, event)
	})
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		// 重复请求返回既有待处理任务，保证客户端重试幂等。
		var existing deletionRequestModel
		findErr := WithScope(ctx, r.db, Scope{UserID: request.UserID}, func(tx *gorm.DB) error {
			return tx.Where("user_id = ? AND resource_type = ? AND resource_id = ? AND status IN ?",
				request.UserID, request.ResourceType, request.ResourceID,
				[]string{string(biz.DeletionPending), string(biz.DeletionRunning)}).First(&existing).Error
		})
		if findErr != nil {
			return biz.DeletionRequest{}, catalogError("find active deletion request", findErr)
		}
		return toBizDeletionRequest(existing), nil
	}
	if err != nil {
		return biz.DeletionRequest{}, catalogError("create deletion request", err)
	}
	return toBizDeletionRequest(record), nil
}

// Get 按用户与请求 ID 读取。
func (r *DeletionRepository) Get(ctx context.Context, userID, requestID uuid.UUID) (biz.DeletionRequest, error) {
	var record deletionRequestModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Where("id = ? AND user_id = ?", requestID, userID).First(&record).Error
	})
	if err != nil {
		return biz.DeletionRequest{}, catalogError("get deletion request", err)
	}
	return toBizDeletionRequest(record), nil
}

// List 按创建时间倒序游标分页列出当前用户删除请求。
func (r *DeletionRepository) List(ctx context.Context, userID uuid.UUID, limit int, cursor string) (biz.Page[biz.DeletionRequest], error) {
	decoded, err := DecodeCursor(cursor)
	if err != nil {
		return biz.Page[biz.DeletionRequest]{}, fmt.Errorf("%w: invalid deletion cursor", biz.ErrInvalidConfig)
	}
	limit = NormalizeLimit(limit)
	var records []deletionRequestModel
	err = WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		query := tx.Where("user_id = ?", userID)
		if decoded.ID != uuid.Nil {
			query = query.Where("(created_at < ? OR (created_at = ? AND id < ?))", decoded.CreatedAt, decoded.CreatedAt, decoded.ID)
		}
		return query.Order("created_at DESC, id DESC").Limit(limit + 1).Find(&records).Error
	})
	if err != nil {
		return biz.Page[biz.DeletionRequest]{}, catalogError("list deletion requests", err)
	}
	page := biz.Page[biz.DeletionRequest]{Items: make([]biz.DeletionRequest, 0, min(limit, len(records)))}
	if len(records) > limit {
		records = records[:limit]
		last := records[len(records)-1]
		page.NextCursor, err = EncodeCursor(Cursor{CreatedAt: last.CreatedAt, ID: last.ID})
		if err != nil {
			return biz.Page[biz.DeletionRequest]{}, err
		}
	}
	for _, record := range records {
		page.Items = append(page.Items, toBizDeletionRequest(record))
	}
	return page, nil
}

// Claim 使用 SKIP LOCKED 领取待处理或租约过期的删除请求。
func (r *DeletionRepository) Claim(ctx context.Context, workerID string, lease time.Duration, now time.Time) (biz.DeletionRequest, bool, error) {
	if workerID == "" || lease <= 0 {
		return biz.DeletionRequest{}, false, fmt.Errorf("%w: worker and lease are required", biz.ErrInvalidConfig)
	}
	type claimed struct {
		ID              uuid.UUID
		UserID          uuid.UUID
		ResourceType    string
		ResourceID      string
		Status          string
		RequestedBy     string
		RequestedSource string
		Attempts        int
		MaxAttempts     int
		FencingToken    int64
		LeaseOwner      string
		CreatedAt       time.Time
	}
	var record claimed
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		query := `
WITH candidate AS (
  SELECT id FROM deletion_requests
  WHERE (
    (status = 'pending' AND available_at <= ?)
    OR (status = 'running' AND (lease_expires_at IS NULL OR lease_expires_at < ?))
  )
  ORDER BY available_at, created_at, id
  FOR UPDATE SKIP LOCKED
  LIMIT 1
)
UPDATE deletion_requests SET status = 'running', attempts = attempts + 1, lease_owner = ?,
  lease_expires_at = ?, fencing_token = fencing_token + 1, updated_at = ?
FROM candidate WHERE deletion_requests.id = candidate.id
RETURNING deletion_requests.id, deletion_requests.user_id, deletion_requests.resource_type,
  deletion_requests.resource_id, deletion_requests.status, deletion_requests.requested_by,
  deletion_requests.requested_source, deletion_requests.attempts, deletion_requests.max_attempts,
  deletion_requests.fencing_token, deletion_requests.lease_owner, deletion_requests.created_at`
		return tx.Raw(query, now, now, workerID, now.Add(lease), now).Scan(&record).Error
	})
	if err != nil {
		return biz.DeletionRequest{}, false, catalogError("claim deletion request", err)
	}
	if record.ID == uuid.Nil {
		return biz.DeletionRequest{}, false, nil
	}
	return biz.DeletionRequest{
		ID: record.ID, UserID: record.UserID, ResourceType: biz.DeletionResourceType(record.ResourceType),
		ResourceID: record.ResourceID, Status: biz.DeletionStatus(record.Status), RequestedBy: record.RequestedBy,
		RequestedSource: record.RequestedSource, Attempts: record.Attempts, MaxAttempts: record.MaxAttempts,
		FencingToken: record.FencingToken, LeaseOwner: record.LeaseOwner, CreatedAt: record.CreatedAt,
	}, true, nil
}

// Renew 续约删除请求租约；栅栏失效时返回冲突。
func (r *DeletionRepository) Renew(ctx context.Context, requestID uuid.UUID, workerID string, fencingToken int64, lease time.Duration, now time.Time) error {
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		result := tx.Model(&deletionRequestModel{}).
			Where("id = ? AND status = 'running' AND lease_owner = ? AND fencing_token = ?", requestID, workerID, fencingToken).
			Updates(map[string]any{"lease_expires_at": now.Add(lease), "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("%w: deletion lease is no longer valid", biz.ErrResourceConflict)
		}
		return nil
	})
	return catalogError("renew deletion lease", err)
}

// Complete 标记完成并写入审计事件。
func (r *DeletionRepository) Complete(ctx context.Context, requestID uuid.UUID, workerID string, fencingToken int64, now time.Time, event biz.OutboxEvent) error {
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		result := tx.Model(&deletionRequestModel{}).
			Where("id = ? AND status = 'running' AND lease_owner = ? AND fencing_token = ?", requestID, workerID, fencingToken).
			Updates(map[string]any{"status": string(biz.DeletionCompleted), "lease_owner": "",
				"lease_expires_at": nil, "completed_at": now, "updated_at": now, "last_error": ""})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("%w: deletion lease is no longer valid", biz.ErrResourceConflict)
		}
		return appendOutbox(tx, event)
	})
	return catalogError("complete deletion request", err)
}

// Fail 记录失败并在预算内退避，超出预算进入 failed。
func (r *DeletionRepository) Fail(ctx context.Context, requestID uuid.UUID, workerID string, fencingToken int64, retryAt time.Time, reason string, now time.Time) error {
	if len(reason) > 1024 {
		reason = reason[:1024]
	}
	status := string(biz.DeletionPending)
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		var record deletionRequestModel
		if err := tx.First(&record, "id = ? AND status = 'running' AND lease_owner = ? AND fencing_token = ?",
			requestID, workerID, fencingToken).Error; err != nil {
			return err
		}
		if record.Attempts >= record.MaxAttempts {
			status = string(biz.DeletionFailed)
		}
		updates := map[string]any{
			"status": status, "last_error": reason, "lease_owner": "", "lease_expires_at": nil, "updated_at": now,
		}
		if status == string(biz.DeletionPending) {
			updates["available_at"] = retryAt
		} else {
			updates["completed_at"] = now
		}
		return tx.Model(&deletionRequestModel{}).Where("id = ?", requestID).Updates(updates).Error
	})
	return catalogError("fail deletion request", err)
}

// toBizDeletionRequest 把数据库行转换为领域请求。
func toBizDeletionRequest(record deletionRequestModel) biz.DeletionRequest {
	return biz.DeletionRequest{
		ID: record.ID, UserID: record.UserID, ResourceType: biz.DeletionResourceType(record.ResourceType),
		ResourceID: record.ResourceID, Status: biz.DeletionStatus(record.Status), RequestedBy: record.RequestedBy,
		RequestedSource: record.RequestedSource, Attempts: record.Attempts, MaxAttempts: record.MaxAttempts,
		FencingToken: record.FencingToken, LeaseOwner: record.LeaseOwner, AvailableAt: record.AvailableAt,
		LastError: record.LastError, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
		CompletedAt: record.CompletedAt,
	}
}

// DeletionExecutor 执行会话与记忆的实际清理。
type DeletionExecutor struct {
	db  *gorm.DB
	hot biz.HotMessageStore
}

// NewDeletionExecutor 构造删除执行器；hot 可为空，表示无热缓存需要清理。
func NewDeletionExecutor(db *gorm.DB, hot biz.HotMessageStore) *DeletionExecutor {
	return &DeletionExecutor{db: db, hot: hot}
}

// DeleteSession 清理会话、消息、Run、摘要、事实、向量、外部绑定、关联任务与缓存。
func (e *DeletionExecutor) DeleteSession(ctx context.Context, userID, sessionID uuid.UUID) error {
	if userID == uuid.Nil || sessionID == uuid.Nil {
		return fmt.Errorf("%w: user and session ids are required", biz.ErrInvalidConfig)
	}
	err := WithScope(ctx, e.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		// 先清理指向该会话或其 Run 的待处理任务，避免删除后任务继续执行。
		if err := tx.Exec(`
DELETE FROM jobs WHERE user_id = ? AND (
  payload->>'session_id' = ? OR
  payload->>'run_id' IN (SELECT id::text FROM agent_runs WHERE user_id = ? AND session_id = ?)
)`, userID, sessionID.String(), userID, sessionID).Error; err != nil {
			return err
		}
		statements := []string{
			"DELETE FROM external_conversation_bindings WHERE user_id = ? AND session_id = ?",
			"DELETE FROM memory_vectors WHERE user_id = ? AND session_id = ?",
			"DELETE FROM memory_facts WHERE user_id = ? AND session_id = ?",
			"DELETE FROM session_summaries WHERE user_id = ? AND session_id = ?",
			"DELETE FROM agent_runs WHERE user_id = ? AND session_id = ?",
			"DELETE FROM messages WHERE user_id = ? AND session_id = ?",
			"DELETE FROM session_members WHERE user_id = ? AND session_id = ?",
			"DELETE FROM sessions WHERE user_id = ? AND id = ?",
		}
		for _, statement := range statements {
			if err := tx.Exec(statement, userID, sessionID).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return catalogError("delete session data", err)
	}
	// 缓存删除失败时返回错误重试，避免已删除数据继续从 Redis 热窗口被组装进上下文。
	if e.hot != nil {
		if err := e.hot.Invalidate(ctx, userID, sessionID); err != nil {
			return fmt.Errorf("invalidate session cache: %w", err)
		}
	}
	return nil
}

// DeleteMemory 清理用户全部摘要、事实、向量、记忆任务与全部会话热缓存。
func (e *DeletionExecutor) DeleteMemory(ctx context.Context, userID uuid.UUID) error {
	if userID == uuid.Nil {
		return fmt.Errorf("%w: user id is required", biz.ErrInvalidConfig)
	}
	var sessionIDs []uuid.UUID
	err := WithScope(ctx, e.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		if err := tx.Model(&sessionModel{}).Where("user_id = ?", userID).Pluck("id", &sessionIDs).Error; err != nil {
			return err
		}
		if err := tx.Exec(`DELETE FROM jobs WHERE user_id = ? AND job_type IN ?`,
			userID, []string{biz.MemoryEmbedJobType, biz.SummaryJobType, biz.FactExtractJobType}).Error; err != nil {
			return err
		}
		for _, statement := range []string{
			"DELETE FROM memory_vectors WHERE user_id = ?",
			"DELETE FROM memory_facts WHERE user_id = ?",
			"DELETE FROM session_summaries WHERE user_id = ?",
		} {
			if err := tx.Exec(statement, userID).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return catalogError("delete memory data", err)
	}
	if e.hot != nil {
		var failures []string
		for _, sessionID := range sessionIDs {
			if err := e.hot.Invalidate(ctx, userID, sessionID); err != nil {
				failures = append(failures, sessionID.String())
			}
		}
		if len(failures) > 0 {
			return fmt.Errorf("invalidate %d session caches: %s", len(failures), strings.Join(failures, ","))
		}
	}
	return nil
}

// DeleteAccount 清理账号全部业务数据并匿名化用户行。
// 关键步骤：保留 users 行以维持审计/删除请求外键；清除用户名、密码与身份，令牌全部失效。
func (e *DeletionExecutor) DeleteAccount(ctx context.Context, userID uuid.UUID) error {
	if userID == uuid.Nil {
		return fmt.Errorf("%w: user id is required", biz.ErrInvalidConfig)
	}
	var sessionIDs []uuid.UUID
	err := WithScope(ctx, e.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		if err := tx.Model(&sessionModel{}).Where("user_id = ?", userID).Pluck("id", &sessionIDs).Error; err != nil {
			return err
		}
		// 账号删除还要清理仅允许系统作用域操作的外部外发记录。
		// 所有 DELETE 仍显式携带 user_id 过滤，因此开启系统作用域不会越界到其他用户。
		if err := tx.Exec("SELECT set_config('app.system', 'on', true)").Error; err != nil {
			return err
		}
		// 用户业务表全部按 user_id 清理；审计相关表（audit_logs/outbox_events/deletion_requests）保留。
		// 顺序必须满足外键依赖：先删 Run/Plan/Binding 等引用方，再删 Session，最后删 Agent，
		// 否则 agent_runs、sessions、binding 仍引用 agents 时会触发外键拒绝。
		statements := []string{
			"DELETE FROM jobs WHERE user_id = ?",
			"DELETE FROM tool_approvals WHERE user_id = ?",
			"DELETE FROM execution_payloads WHERE user_id = ?",
			"DELETE FROM model_calls WHERE user_id = ?",
			"DELETE FROM execution_steps WHERE user_id = ?",
			"DELETE FROM run_events WHERE user_id = ?",
			"DELETE FROM supervisor_tasks WHERE user_id = ?",
			"DELETE FROM plan_steps WHERE user_id = ?",
			"DELETE FROM plans WHERE user_id = ?",
			"DELETE FROM external_conversation_bindings WHERE user_id = ?",
			"DELETE FROM feishu_outbound_messages WHERE user_id = ?",
			"DELETE FROM agent_runs WHERE user_id = ?",
			"DELETE FROM memory_vectors WHERE user_id = ?",
			"DELETE FROM memory_facts WHERE user_id = ?",
			"DELETE FROM session_summaries WHERE user_id = ?",
			"DELETE FROM messages WHERE user_id = ?",
			"DELETE FROM session_members WHERE user_id = ?",
			"DELETE FROM sessions WHERE user_id = ?",
			"DELETE FROM agent_mcps WHERE user_id = ?",
			"DELETE FROM agent_skills WHERE user_id = ?",
			"DELETE FROM agents WHERE user_id = ?",
			"DELETE FROM idempotency_keys WHERE user_id = ?",
			"DELETE FROM refresh_tokens WHERE user_id = ?",
			"DELETE FROM user_identities WHERE user_id = ?",
			"DELETE FROM password_reset_tokens WHERE user_id = ?",
			"DELETE FROM feishu_event_receipts WHERE user_id = ?",
			"DELETE FROM usage_counters WHERE scope = 'user' AND scope_id = ?",
		}
		for _, statement := range statements {
			if err := tx.Exec(statement, userID).Error; err != nil {
				return err
			}
		}
		// 匿名化保留 users 行；状态 deleted 阻止登录，credential_version 使所有令牌失效。
		anonymized := "deleted_" + strings.ReplaceAll(userID.String(), "-", "")
		return tx.Exec(`
UPDATE users SET username = ?, username_normalized = ?, password_hash = '', status = 'deleted',
  credential_version = credential_version + 1, updated_at = ? WHERE id = ?`,
			anonymized, anonymized, time.Now().UTC(), userID).Error
	})
	if err != nil {
		return catalogError("delete account data", err)
	}
	if e.hot != nil {
		var failures []string
		for _, sessionID := range sessionIDs {
			if err := e.hot.Invalidate(ctx, userID, sessionID); err != nil {
				failures = append(failures, sessionID.String())
			}
		}
		if len(failures) > 0 {
			return fmt.Errorf("invalidate %d account session caches: %s", len(failures), strings.Join(failures, ","))
		}
	}
	return nil
}

// 编译期断言删除仓储与执行器实现。
var (
	_ biz.DeletionRepository = (*DeletionRepository)(nil)
	_ biz.DeletionExecutor   = (*DeletionExecutor)(nil)
)
