package data

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// uuidArray 是 PostgreSQL uuid[] 的自定义映射，避免把 JSON 数组误写为数组字面量。
type uuidArray []uuid.UUID

// Value 把 UUID 列表编码为 PostgreSQL 数组字面量。
func (a uuidArray) Value() (driver.Value, error) {
	if len(a) == 0 {
		return "{}", nil
	}
	parts := make([]string, 0, len(a))
	for _, id := range a {
		parts = append(parts, id.String())
	}
	return "{" + strings.Join(parts, ",") + "}", nil
}

// Scan 解析 PostgreSQL uuid[] 文本格式。
func (a *uuidArray) Scan(value any) error {
	var raw string
	switch typed := value.(type) {
	case nil:
		*a = nil
		return nil
	case string:
		raw = typed
	case []byte:
		raw = string(typed)
	default:
		return fmt.Errorf("unsupported uuid array type %T", value)
	}
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		*a = uuidArray{}
		return nil
	}
	if !strings.HasPrefix(raw, "{") || !strings.HasSuffix(raw, "}") {
		return fmt.Errorf("invalid uuid array literal")
	}
	parts := strings.Split(raw[1:len(raw)-1], ",")
	ids := make(uuidArray, 0, len(parts))
	for _, part := range parts {
		id, err := uuid.Parse(strings.Trim(strings.TrimSpace(part), `"`))
		if err != nil {
			return fmt.Errorf("invalid uuid in array: %w", err)
		}
		ids = append(ids, id)
	}
	*a = ids
	return nil
}

// sessionSummaryModel 对应 session_summaries 表：版本化滚动摘要。
type sessionSummaryModel struct {
	// ID 摘要版本主键。
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// SessionID 所属会话。
	SessionID uuid.UUID `gorm:"type:uuid"`
	// UserID 所属用户，RLS 隔离键。
	UserID uuid.UUID `gorm:"type:uuid"`
	// Version 会话内版本号。
	Version int64
	// SummaryText 摘要正文。
	SummaryText string
	// SourceMessageIDs 本版本新增来源消息 ID 数组。
	SourceMessageIDs uuidArray `gorm:"type:uuid[]"`
	// SourceStartMessageID 来源范围起始消息。
	SourceStartMessageID *uuid.UUID `gorm:"type:uuid"`
	// SourceEndMessageID 来源范围结束消息。
	SourceEndMessageID *uuid.UUID `gorm:"type:uuid"`
	// SourceEndAt 边界消息创建时间。
	SourceEndAt *time.Time
	// SourceFingerprint 来源内容指纹。
	SourceFingerprint string
	// CoveredMessageCount 累计覆盖消息数。
	CoveredMessageCount int
	// ParentSummaryID 上一版本摘要。
	ParentSummaryID *uuid.UUID `gorm:"type:uuid"`
	// Status 版本状态。
	Status string
	// ModelProviderID 摘要模型供应商。
	ModelProviderID *uuid.UUID `gorm:"type:uuid"`
	// ModelID 摘要模型标识。
	ModelID string
	// InputTokens 输入 Token。
	InputTokens int
	// OutputTokens 输出 Token。
	OutputTokens int
	// CreatedAt 创建时间。
	CreatedAt time.Time
	// UpdatedAt 更新时间。
	UpdatedAt time.Time
}

// TableName 指定摘要表名。
func (sessionSummaryModel) TableName() string { return "session_summaries" }

// SummaryRepository 是摘要版本的 GORM 实现，所有查询与写入均强制 user_id。
type SummaryRepository struct {
	db *gorm.DB
}

// NewSummaryRepository 构造摘要版本仓库。
func NewSummaryRepository(db *gorm.DB) *SummaryRepository {
	return &SummaryRepository{db: db}
}

// GetActiveSummary 读取当前生效摘要；来源边界之前消息数不一致时标记失效并返回未命中。
func (r *SummaryRepository) GetActiveSummary(ctx context.Context, userID, sessionID uuid.UUID) (biz.SessionSummary, bool, error) {
	if userID == uuid.Nil || sessionID == uuid.Nil {
		return biz.SessionSummary{}, false, fmt.Errorf("%w: user and session ids are required", biz.ErrInvalidConfig)
	}
	var record sessionSummaryModel
	found := false
	invalid := false
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		err := tx.Where("user_id = ? AND session_id = ? AND status = ?", userID, sessionID, biz.SummaryActive).
			Order("version DESC").First(&record).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		if record.SourceEndMessageID == nil || record.SourceEndAt == nil {
			invalid = true
			return tx.Model(&sessionSummaryModel{}).Where("id = ?", record.ID).
				Updates(map[string]any{"status": biz.SummaryInvalidated, "updated_at": time.Now().UTC()}).Error
		}
		// 边界之前的消息被删除会使摘要失真；计数不一致时立即失效，禁止把陈旧摘要冒充最新状态。
		var count int64
		if err := tx.Model(&messageModel{}).
			Where("user_id = ? AND session_id = ? AND deleted_at IS NULL AND (created_at < ? OR (created_at = ? AND id <= ?))",
				userID, sessionID, *record.SourceEndAt, *record.SourceEndAt, *record.SourceEndMessageID).
			Count(&count).Error; err != nil {
			return err
		}
		if count != int64(record.CoveredMessageCount) {
			invalid = true
			return tx.Model(&sessionSummaryModel{}).Where("id = ?", record.ID).
				Updates(map[string]any{"status": biz.SummaryInvalidated, "updated_at": time.Now().UTC()}).Error
		}
		return nil
	})
	if err != nil {
		return biz.SessionSummary{}, false, catalogError("get active summary", err)
	}
	if !found || invalid {
		return biz.SessionSummary{}, false, nil
	}
	summary, err := toBizSummary(record)
	if err != nil {
		return biz.SessionSummary{}, false, err
	}
	return summary, true, nil
}

// LatestSummaryVersion 返回包含失效版本在内的最大版本号。
func (r *SummaryRepository) LatestSummaryVersion(ctx context.Context, userID, sessionID uuid.UUID) (int64, error) {
	var version int64
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Model(&sessionSummaryModel{}).Where("user_id = ? AND session_id = ?", userID, sessionID).
			Select("COALESCE(MAX(version), 0)").Scan(&version).Error
	})
	if err != nil {
		return 0, catalogError("latest summary version", err)
	}
	return version, nil
}

// SaveSummary 以期望版本、父版本与来源完整性守卫写入新版本，并原子替换旧 active 版本。
// 返回 applied=false 表示任务已过期或被并发版本超越，调用方应视为幂等完成。
func (r *SummaryRepository) SaveSummary(ctx context.Context, summary biz.SessionSummary, expectedVersion int64, event biz.OutboxEvent) (biz.SessionSummary, bool, error) {
	if summary.UserID == uuid.Nil || summary.SessionID == uuid.Nil || summary.ID == uuid.Nil {
		return biz.SessionSummary{}, false, fmt.Errorf("%w: summary identity is required", biz.ErrInvalidConfig)
	}
	if expectedVersion < 0 || summary.Version != expectedVersion+1 {
		return biz.SessionSummary{}, false, fmt.Errorf("%w: summary version must be expected + 1", biz.ErrInvalidConfig)
	}
	if summary.SourceEndMessageID == nil || summary.SourceEndAt == nil {
		return biz.SessionSummary{}, false, fmt.Errorf("%w: summary source boundary is required", biz.ErrInvalidConfig)
	}
	now := time.Now().UTC()
	record := sessionSummaryModel{
		ID: summary.ID, SessionID: summary.SessionID, UserID: summary.UserID, Version: summary.Version,
		SummaryText: summary.SummaryText, SourceMessageIDs: uuidArray(summary.SourceMessageIDs),
		SourceStartMessageID: summary.SourceStartMessageID, SourceEndMessageID: summary.SourceEndMessageID,
		SourceEndAt: summary.SourceEndAt, SourceFingerprint: summary.SourceFingerprint,
		CoveredMessageCount: summary.CoveredMessageCount, ParentSummaryID: summary.ParentSummaryID,
		Status: string(biz.SummaryActive), ModelProviderID: summary.ModelProviderID, ModelID: summary.ModelID,
		InputTokens: summary.InputTokens, OutputTokens: summary.OutputTokens,
		CreatedAt: now, UpdatedAt: now,
	}
	applied := false
	conflict := false
	err := WithScope(ctx, r.db, Scope{UserID: summary.UserID}, func(tx *gorm.DB) error {
		var active sessionSummaryModel
		lockErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("user_id = ? AND session_id = ? AND status = ?", summary.UserID, summary.SessionID, biz.SummaryActive).
			Order("version DESC").First(&active).Error
		if lockErr != nil && !errors.Is(lockErr, gorm.ErrRecordNotFound) {
			return lockErr
		}
		var latest int64
		if err := tx.Model(&sessionSummaryModel{}).Where("user_id = ? AND session_id = ?", summary.UserID, summary.SessionID).
			Select("COALESCE(MAX(version), 0)").Scan(&latest).Error; err != nil {
			return err
		}
		// 期望版本必须仍是最大版本，否则说明已有更新版本写入，旧任务不得覆盖。
		if latest != expectedVersion {
			return nil
		}
		if lockErr == nil {
			if summary.ParentSummaryID == nil || *summary.ParentSummaryID != active.ID {
				return nil
			}
			if active.SourceEndMessageID == nil || active.SourceEndAt == nil {
				return nil
			}
			if !boundaryAfter(summary.SourceEndAt, summary.SourceEndMessageID, active.SourceEndAt, active.SourceEndMessageID) {
				return nil
			}
		} else if summary.ParentSummaryID != nil {
			// 没有生效父版本时不得携带父版本 ID，避免版本链断裂。
			return nil
		}
		// 来源消息必须全部存在且未删除，数量和指纹已在 Worker 侧校验，这里做并发删除的最后守卫。
		if len(summary.SourceMessageIDs) > 0 {
			var sourceCount int64
			if err := tx.Model(&messageModel{}).
				Where("user_id = ? AND session_id = ? AND deleted_at IS NULL AND id IN ?", summary.UserID, summary.SessionID, summary.SourceMessageIDs).
				Count(&sourceCount).Error; err != nil {
				return err
			}
			if sourceCount != int64(len(summary.SourceMessageIDs)) {
				return fmt.Errorf("%w: source messages were deleted before save", biz.ErrSummarySourceInvalid)
			}
		}
		// 累计覆盖数必须与边界前的实际消息数一致，禁止来源范围出现空洞。
		var covered int64
		if err := tx.Model(&messageModel{}).
			Where("user_id = ? AND session_id = ? AND deleted_at IS NULL AND (created_at < ? OR (created_at = ? AND id <= ?))",
				summary.UserID, summary.SessionID, *summary.SourceEndAt, *summary.SourceEndAt, *summary.SourceEndMessageID).
			Count(&covered).Error; err != nil {
			return err
		}
		if covered != int64(summary.CoveredMessageCount) {
			return fmt.Errorf("%w: covered message count mismatch", biz.ErrSummarySourceInvalid)
		}
		// 先替换旧 active 再插入新版本，避免触发“单会话仅一个 active”的唯一索引。
		if lockErr == nil {
			if err := tx.Model(&sessionSummaryModel{}).Where("id = ?", active.ID).
				Updates(map[string]any{"status": biz.SummarySuperseded, "updated_at": now}).Error; err != nil {
				return err
			}
		}
		if err := tx.Create(&record).Error; err != nil {
			if errors.Is(err, gorm.ErrDuplicatedKey) {
				// 并发任务已写入同一版本：回滚事务以恢复旧 active 状态，并视为过期任务。
				conflict = true
				return fmt.Errorf("%w: summary version already exists", biz.ErrResourceConflict)
			}
			return err
		}
		if err := appendOutbox(tx, event); err != nil {
			return err
		}
		applied = true
		return nil
	})
	if err != nil {
		if conflict {
			return biz.SessionSummary{}, false, nil
		}
		return biz.SessionSummary{}, false, catalogError("save summary", err)
	}
	if !applied {
		return biz.SessionSummary{}, false, nil
	}
	summary.Status = biz.SummaryActive
	summary.CreatedAt = now
	summary.UpdatedAt = now
	return summary, true, nil
}

// boundaryAfter 判断候选边界是否严格晚于当前 active 边界。
func boundaryAfter(at *time.Time, id *uuid.UUID, activeAt *time.Time, activeID *uuid.UUID) bool {
	if at == nil || id == nil || activeAt == nil || activeID == nil {
		return false
	}
	if at.After(*activeAt) {
		return true
	}
	if at.Equal(*activeAt) {
		return id.String() > activeID.String()
	}
	return false
}

// InvalidateActive 显式失效当前摘要，用于消息删除或编辑后的立即失效。
func (r *SummaryRepository) InvalidateActive(ctx context.Context, userID, sessionID uuid.UUID, now time.Time) error {
	if userID == uuid.Nil || sessionID == uuid.Nil {
		return fmt.Errorf("%w: user and session ids are required", biz.ErrInvalidConfig)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Model(&sessionSummaryModel{}).
			Where("user_id = ? AND session_id = ? AND status = ?", userID, sessionID, biz.SummaryActive).
			Updates(map[string]any{"status": biz.SummaryInvalidated, "updated_at": now}).Error
	})
	return catalogError("invalidate summary", err)
}

// ListSummaries 按版本倒序读取摘要历史，供审计与导出。
func (r *SummaryRepository) ListSummaries(ctx context.Context, userID, sessionID uuid.UUID, limit int) ([]biz.SessionSummary, error) {
	if userID == uuid.Nil || sessionID == uuid.Nil {
		return nil, fmt.Errorf("%w: user and session ids are required", biz.ErrInvalidConfig)
	}
	limit = NormalizeLimit(limit)
	var records []sessionSummaryModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Where("user_id = ? AND session_id = ?", userID, sessionID).
			Order("version DESC").Limit(limit).Find(&records).Error
	})
	if err != nil {
		return nil, catalogError("list summaries", err)
	}
	summaries := make([]biz.SessionSummary, 0, len(records))
	for _, record := range records {
		summary, err := toBizSummary(record)
		if err != nil {
			return nil, err
		}
		summaries = append(summaries, summary)
	}
	return summaries, nil
}

// ListUserSummaries 按创建时间倒序列出用户全部摘要版本，供记忆导出。
func (r *SummaryRepository) ListUserSummaries(ctx context.Context, userID uuid.UUID, limit int) ([]biz.SessionSummary, error) {
	if userID == uuid.Nil {
		return nil, fmt.Errorf("%w: user id is required", biz.ErrInvalidConfig)
	}
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	var records []sessionSummaryModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Where("user_id = ?", userID).Order("created_at DESC, id DESC").Limit(limit).Find(&records).Error
	})
	if err != nil {
		return nil, catalogError("list user summaries", err)
	}
	summaries := make([]biz.SessionSummary, 0, len(records))
	for _, record := range records {
		summary, err := toBizSummary(record)
		if err != nil {
			return nil, err
		}
		summaries = append(summaries, summary)
	}
	return summaries, nil
}

// ListRecentMessages 按时间倒序读取最近消息，用于确定摘要边界。
func (r *SessionRepository) ListRecentMessages(ctx context.Context, userID, sessionID uuid.UUID, limit int) ([]biz.Message, error) {
	if userID == uuid.Nil || sessionID == uuid.Nil {
		return nil, fmt.Errorf("%w: user and session ids are required", biz.ErrInvalidConfig)
	}
	limit = NormalizeLimit(limit)
	var records []messageModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Where("user_id = ? AND session_id = ? AND deleted_at IS NULL", userID, sessionID).
			Order("created_at DESC, id DESC").Limit(limit).Find(&records).Error
	})
	if err != nil {
		return nil, catalogError("list recent messages", err)
	}
	return toBizMessages(records)
}

// ListMessagesAfter 按时间正序读取指定位置之后的消息。
func (r *SessionRepository) ListMessagesAfter(ctx context.Context, userID, sessionID uuid.UUID, after biz.MessagePosition, limit int) ([]biz.Message, error) {
	if userID == uuid.Nil || sessionID == uuid.Nil {
		return nil, fmt.Errorf("%w: user and session ids are required", biz.ErrInvalidConfig)
	}
	limit = NormalizeLimit(limit)
	var records []messageModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		query := tx.Where("user_id = ? AND session_id = ? AND deleted_at IS NULL", userID, sessionID)
		if !after.IsZero() {
			query = query.Where("(created_at > ? OR (created_at = ? AND id > ?))", after.CreatedAt, after.CreatedAt, after.ID)
		}
		return query.Order("created_at ASC, id ASC").Limit(limit).Find(&records).Error
	})
	if err != nil {
		return nil, catalogError("list messages after", err)
	}
	return toBizMessages(records)
}

// ListMessagesByIDs 按 ID 读取消息并按时间正序返回；缺失或已删除的消息不会返回。
func (r *SessionRepository) ListMessagesByIDs(ctx context.Context, userID, sessionID uuid.UUID, messageIDs []uuid.UUID) ([]biz.Message, error) {
	if userID == uuid.Nil || sessionID == uuid.Nil || len(messageIDs) == 0 {
		return nil, fmt.Errorf("%w: user, session and message ids are required", biz.ErrInvalidConfig)
	}
	if len(messageIDs) > 10000 {
		return nil, fmt.Errorf("%w: too many message ids", biz.ErrInvalidConfig)
	}
	var records []messageModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Where("user_id = ? AND session_id = ? AND deleted_at IS NULL AND id IN ?", userID, sessionID, messageIDs).
			Order("created_at ASC, id ASC").Find(&records).Error
	})
	if err != nil {
		return nil, catalogError("list messages by ids", err)
	}
	return toBizMessages(records)
}

// CountMessagesThrough 统计边界位置（含）之前的非删除消息数。
func (r *SessionRepository) CountMessagesThrough(ctx context.Context, userID, sessionID uuid.UUID, boundary biz.MessagePosition) (int64, error) {
	if userID == uuid.Nil || sessionID == uuid.Nil || boundary.IsZero() {
		return 0, fmt.Errorf("%w: user, session and boundary are required", biz.ErrInvalidConfig)
	}
	var count int64
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Model(&messageModel{}).
			Where("user_id = ? AND session_id = ? AND deleted_at IS NULL AND (created_at < ? OR (created_at = ? AND id <= ?))",
				userID, sessionID, boundary.CreatedAt, boundary.CreatedAt, boundary.ID).
			Count(&count).Error
	})
	if err != nil {
		return 0, catalogError("count messages through", err)
	}
	return count, nil
}

// toBizMessages 把消息行批量转换为领域消息。
func toBizMessages(records []messageModel) ([]biz.Message, error) {
	messages := make([]biz.Message, 0, len(records))
	for _, record := range records {
		message, err := toBizMessage(record)
		if err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, nil
}

// toBizSummary 把摘要行转换为领域摘要。
func toBizSummary(record sessionSummaryModel) (biz.SessionSummary, error) {
	parsed := make([]uuid.UUID, 0, len(record.SourceMessageIDs))
	parsed = append(parsed, record.SourceMessageIDs...)
	return biz.SessionSummary{
		ID: record.ID, SessionID: record.SessionID, UserID: record.UserID, Version: record.Version,
		SummaryText: record.SummaryText, SourceMessageIDs: parsed,
		SourceStartMessageID: record.SourceStartMessageID, SourceEndMessageID: record.SourceEndMessageID,
		SourceEndAt: record.SourceEndAt, SourceFingerprint: record.SourceFingerprint,
		CoveredMessageCount: record.CoveredMessageCount, ParentSummaryID: record.ParentSummaryID,
		Status: biz.SummaryStatus(record.Status), ModelProviderID: record.ModelProviderID, ModelID: record.ModelID,
		InputTokens: record.InputTokens, OutputTokens: record.OutputTokens,
		CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
	}, nil
}

// 编译期断言摘要仓储实现。
var _ biz.SummaryRepository = (*SummaryRepository)(nil)

// 编译期断言摘要消息读取端口实现。
var _ biz.SummaryMessageReader = (*SessionRepository)(nil)
