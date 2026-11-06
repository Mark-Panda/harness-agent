package data

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// sessionModel 对应 sessions 表：用户私有会话。
type sessionModel struct {
	// ID 会话主键。
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// UserID 所属用户，RLS 隔离键。
	UserID uuid.UUID `gorm:"type:uuid"`
	// AgentID 绑定的 Agent 版本，可为空。
	AgentID *uuid.UUID `gorm:"type:uuid"`
	// ParentSessionID Supervisor 父会话。
	ParentSessionID *uuid.UUID `gorm:"type:uuid"`
	// ParentRunID Supervisor 主 Run。
	ParentRunID *uuid.UUID `gorm:"type:uuid"`
	// Title 会话标题。
	Title string
	// Source 来源渠道：web/feishu/api。
	Source string
	// MemoryPolicy 记忆策略：private/group/disabled。
	MemoryPolicy string
	// Status 生命周期：active/archived/deleted。
	Status string
	// Version 乐观锁版本号，归档与追加消息时递增。
	Version int64
	// LastMessageAt 最近消息时间，用于列表排序。
	LastMessageAt *time.Time
	// CreatedAt 创建时间。
	CreatedAt time.Time
	// UpdatedAt 更新时间。
	UpdatedAt time.Time
}

// TableName 指定会话表名。
func (sessionModel) TableName() string { return "sessions" }

// sessionMemberModel 对应 session_members 表：会话成员关系。
type sessionMemberModel struct {
	// SessionID 会话 ID。
	SessionID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// UserID 成员用户 ID。
	UserID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// MemberRole 成员角色：owner/member。
	MemberRole string
	// CreatedAt 加入时间。
	CreatedAt time.Time
}

// TableName 指定会话成员表名。
func (sessionMemberModel) TableName() string { return "session_members" }

// messageModel 对应 messages 表：会话规范消息。
type messageModel struct {
	// ID 消息主键。
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// SessionID 所属会话。
	SessionID uuid.UUID `gorm:"type:uuid"`
	// UserID 所属用户，RLS 隔离键。
	UserID uuid.UUID `gorm:"type:uuid"`
	// Role 消息角色：system/user/assistant/tool。
	Role string
	// Content 消息正文。
	Content string
	// ToolCalls 工具调用元数据 JSON 数组。
	ToolCalls datatypes.JSON
	// ToolCallID 工具结果对应的调用 ID。
	ToolCallID string
	// ToolName 工具名称。
	ToolName string
	// TokenCount Token 估算值。
	TokenCount int
	// ExternalMessageID 外部渠道消息 ID，用于幂等去重。
	ExternalMessageID string
	// SpeakerOpenID 飞书群聊发言人 open_id。
	SpeakerOpenID string
	// RunID 产生该消息的 Run ID；用户消息为空。
	RunID *uuid.UUID `gorm:"type:uuid"`
	// CreatedAt 创建时间。
	CreatedAt time.Time
}

// TableName 指定消息表名。
func (messageModel) TableName() string { return "messages" }

// SessionRepository 是会话与消息的 GORM 实现，所有查询强制携带 user_id。
type SessionRepository struct {
	db     *gorm.DB
	agents *AgentRepository
}

// NewSessionRepository 构造会话仓库，并复用 Agent 读取能力。
func NewSessionRepository(db *gorm.DB) *SessionRepository {
	return &SessionRepository{db: db, agents: NewAgentRepository(db)}
}

// CreateSession 在同一事务内创建会话、所有者成员与审计事件。
func (r *SessionRepository) CreateSession(ctx context.Context, session biz.Session, event biz.OutboxEvent) (biz.Session, error) {
	record := sessionModel{
		ID: session.ID, UserID: session.UserID, AgentID: session.AgentID, Title: session.Title,
		ParentSessionID: session.ParentSessionID, ParentRunID: session.ParentRunID,
		Source: string(session.Source), MemoryPolicy: string(normalizeSessionMemoryPolicy(session.MemoryPolicy)),
		Status: string(session.Status), Version: session.Version,
	}
	member := sessionMemberModel{SessionID: session.ID, UserID: session.UserID, MemberRole: "owner"}
	err := WithScope(ctx, r.db, Scope{UserID: session.UserID}, func(tx *gorm.DB) error {
		if err := tx.Create(&record).Error; err != nil {
			return err
		}
		if err := tx.Create(&member).Error; err != nil {
			return err
		}
		return appendOutbox(tx, event)
	})
	if err != nil {
		return biz.Session{}, catalogError("create session", err)
	}
	return toBizSession(record), nil
}

// GetSession 按用户与会话 ID 读取，跨用户查询返回 ErrResourceNotFound。
func (r *SessionRepository) GetSession(ctx context.Context, userID, sessionID uuid.UUID) (biz.Session, error) {
	var record sessionModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.First(&record, "id = ? AND user_id = ?", sessionID, userID).Error
	})
	if err != nil {
		return biz.Session{}, catalogError("get session", err)
	}
	return toBizSession(record), nil
}

// ListSessions 按创建时间与 ID 降序游标分页，多取一条判断下一页。
func (r *SessionRepository) ListSessions(ctx context.Context, userID uuid.UUID, limit int, cursor string) (biz.Page[biz.Session], error) {
	decoded, err := DecodeCursor(cursor)
	if err != nil {
		return biz.Page[biz.Session]{}, fmt.Errorf("%w: invalid session cursor", biz.ErrInvalidConfig)
	}
	limit = NormalizeLimit(limit)
	var records []sessionModel
	err = WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		query := tx.Where("user_id = ? AND parent_session_id IS NULL", userID)
		if decoded.ID != uuid.Nil {
			query = query.Where("(created_at < ? OR (created_at = ? AND id < ?))", decoded.CreatedAt, decoded.CreatedAt, decoded.ID)
		}
		return query.Order("created_at DESC, id DESC").Limit(limit + 1).Find(&records).Error
	})
	if err != nil {
		return biz.Page[biz.Session]{}, catalogError("list sessions", err)
	}
	page := biz.Page[biz.Session]{Items: make([]biz.Session, 0, min(limit, len(records)))}
	if len(records) > limit {
		records = records[:limit]
		last := records[len(records)-1]
		page.NextCursor, err = EncodeCursor(Cursor{CreatedAt: last.CreatedAt, ID: last.ID})
		if err != nil {
			return biz.Page[biz.Session]{}, err
		}
	}
	for _, record := range records {
		page.Items = append(page.Items, toBizSession(record))
	}
	return page, nil
}

// ArchiveSession 使用 version 乐观锁归档，避免并发写覆盖。
func (r *SessionRepository) ArchiveSession(ctx context.Context, session biz.Session, event biz.OutboxEvent) (biz.Session, error) {
	var record sessionModel
	err := WithScope(ctx, r.db, Scope{UserID: session.UserID}, func(tx *gorm.DB) error {
		result := tx.Model(&sessionModel{}).Where("id = ? AND user_id = ? AND version = ? AND status = ?", session.ID, session.UserID, session.Version-1, biz.SessionActive).
			Updates(map[string]any{"status": session.Status, "version": session.Version, "updated_at": time.Now().UTC()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return fmt.Errorf("%w: session version conflict", biz.ErrResourceConflict)
		}
		if err := tx.First(&record, "id = ? AND user_id = ?", session.ID, session.UserID).Error; err != nil {
			return err
		}
		return appendOutbox(tx, event)
	})
	if err != nil {
		return biz.Session{}, catalogError("archive session", err)
	}
	return toBizSession(record), nil
}

// UpdateSessionMemoryPolicy 以版本号乐观锁更新记忆策略，并追加审计事件。
// 关键步骤：只允许活跃会话变更策略，避免已归档会话被重新开启记忆写入。
func (r *SessionRepository) UpdateSessionMemoryPolicy(ctx context.Context, session biz.Session, event biz.OutboxEvent) (biz.Session, error) {
	var record sessionModel
	err := WithScope(ctx, r.db, Scope{UserID: session.UserID}, func(tx *gorm.DB) error {
		result := tx.Model(&sessionModel{}).Where("id = ? AND user_id = ? AND version = ? AND status = ?", session.ID, session.UserID, session.Version-1, biz.SessionActive).
			Updates(map[string]any{
				"memory_policy": string(normalizeSessionMemoryPolicy(session.MemoryPolicy)),
				"version":       session.Version, "updated_at": time.Now().UTC(),
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return fmt.Errorf("%w: session version conflict", biz.ErrResourceConflict)
		}
		if err := tx.First(&record, "id = ? AND user_id = ?", session.ID, session.UserID).Error; err != nil {
			return err
		}
		return appendOutbox(tx, event)
	})
	if err != nil {
		return biz.Session{}, catalogError("update session memory policy", err)
	}
	return toBizSession(record), nil
}

// GetAgent 读取绑定 Agent，用于校验会话引用的 Agent 是否已发布。
func (r *SessionRepository) GetAgent(ctx context.Context, userID, agentID uuid.UUID) (biz.Agent, error) {
	return r.agents.GetAgent(ctx, userID, agentID)
}

// BindSessionAgent 为活跃会话补齐 Agent 绑定，供飞书会话在 Web 端继续使用。
func (r *SessionRepository) BindSessionAgent(ctx context.Context, userID, sessionID, agentID uuid.UUID, now time.Time) error {
	if userID == uuid.Nil || sessionID == uuid.Nil || agentID == uuid.Nil {
		return fmt.Errorf("%w: session and agent ids are required", biz.ErrInvalidConfig)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		result := tx.Model(&sessionModel{}).
			Where("id = ? AND user_id = ? AND status = ?", sessionID, userID, biz.SessionActive).
			Updates(map[string]any{"agent_id": agentID, "updated_at": now, "version": gorm.Expr("version + 1")})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("%w: session is missing or inactive", biz.ErrResourceConflict)
		}
		return nil
	})
	return catalogError("bind session agent", err)
}

// AppendMessage 在同一事务内写入消息、推进会话最近消息时间与版本，并追加审计事件。
func (r *SessionRepository) AppendMessage(ctx context.Context, message biz.Message, event biz.OutboxEvent) (biz.Message, error) {
	toolCalls, err := json.Marshal(message.ToolCalls)
	if message.ToolCalls == nil {
		toolCalls = []byte("[]")
	} else if err != nil {
		return biz.Message{}, err
	}
	now := time.Now().UTC()
	record := messageModel{
		ID: message.ID, SessionID: message.SessionID, UserID: message.UserID, Role: string(message.Role), Content: message.Content,
		ToolCalls: datatypes.JSON(toolCalls), ToolCallID: message.ToolCallID, ToolName: message.ToolName,
		TokenCount: message.TokenCount, ExternalMessageID: message.ExternalMessageID,
		SpeakerOpenID: message.SpeakerOpenID, RunID: message.RunID, CreatedAt: now,
	}
	err = WithScope(ctx, r.db, Scope{UserID: message.UserID}, func(tx *gorm.DB) error {
		if err := tx.Create(&record).Error; err != nil {
			return err
		}
		result := tx.Model(&sessionModel{}).Where("id = ? AND user_id = ? AND status = ?", message.SessionID, message.UserID, biz.SessionActive).
			Updates(map[string]any{"last_message_at": now, "updated_at": now, "version": gorm.Expr("version + 1")})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return fmt.Errorf("%w: session is missing or inactive", biz.ErrResourceConflict)
		}
		return appendOutbox(tx, event)
	})
	if err != nil {
		return biz.Message{}, catalogError("append message", err)
	}
	return toBizMessage(record)
}

// AppendMessageIdempotent 以消息 ID 为幂等键追加 Agent 消息。
// 已存在时直接返回原消息，不重复推进会话时间或写入审计 Outbox，避免 Worker 重试产生重复消息。
func (r *SessionRepository) AppendMessageIdempotent(ctx context.Context, message biz.Message, event biz.OutboxEvent) (biz.Message, error) {
	if message.ID == uuid.Nil || message.UserID == uuid.Nil || message.SessionID == uuid.Nil {
		return biz.Message{}, fmt.Errorf("%w: message identity is required", biz.ErrInvalidConfig)
	}
	if message.Role != biz.MessageAssistant && message.Role != biz.MessageTool {
		return biz.Message{}, fmt.Errorf("%w: idempotent append only accepts agent messages", biz.ErrInvalidConfig)
	}
	toolCalls, err := json.Marshal(message.ToolCalls)
	if message.ToolCalls == nil {
		toolCalls = []byte("[]")
	} else if err != nil {
		return biz.Message{}, err
	}
	now := time.Now().UTC()
	record := messageModel{
		ID: message.ID, SessionID: message.SessionID, UserID: message.UserID, Role: string(message.Role),
		Content: message.Content, ToolCalls: datatypes.JSON(toolCalls), ToolCallID: message.ToolCallID,
		ToolName: message.ToolName, TokenCount: message.TokenCount,
		ExternalMessageID: message.ExternalMessageID, SpeakerOpenID: message.SpeakerOpenID, RunID: message.RunID, CreatedAt: now,
	}
	var existing messageModel
	err = WithScope(ctx, r.db, Scope{UserID: message.UserID}, func(tx *gorm.DB) error {
		var session sessionModel
		if err := tx.First(&session, "id = ? AND user_id = ?", message.SessionID, message.UserID).Error; err != nil {
			return err
		}
		if biz.SessionStatus(session.Status) != biz.SessionActive {
			return fmt.Errorf("%w: session is not active", biz.ErrResourceConflict)
		}
		result := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, DoNothing: true}).Create(&record)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			// 消息已存在：返回原记录，不重复更新会话或写审计事件。
			return tx.First(&existing, "id = ? AND user_id = ?", message.ID, message.UserID).Error
		}
		update := tx.Model(&sessionModel{}).Where("id = ? AND user_id = ?", message.SessionID, message.UserID).
			Updates(map[string]any{"last_message_at": now, "updated_at": now, "version": gorm.Expr("version + 1")})
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return fmt.Errorf("%w: session is missing", biz.ErrResourceConflict)
		}
		return appendOutbox(tx, event)
	})
	if err != nil {
		return biz.Message{}, catalogError("append message idempotently", err)
	}
	if existing.ID != uuid.Nil {
		return toBizMessage(existing)
	}
	return toBizMessage(record)
}

// ListMessages 按会话倒序游标分页，隐藏软删除消息。
func (r *SessionRepository) ListMessages(ctx context.Context, userID, sessionID uuid.UUID, limit int, cursor string) (biz.Page[biz.Message], error) {
	decoded, err := DecodeCursor(cursor)
	if err != nil {
		return biz.Page[biz.Message]{}, fmt.Errorf("%w: invalid message cursor", biz.ErrInvalidConfig)
	}
	limit = NormalizeLimit(limit)
	var records []messageModel
	err = WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		query := tx.Where("user_id = ? AND session_id = ? AND deleted_at IS NULL", userID, sessionID)
		if decoded.ID != uuid.Nil {
			query = query.Where("(created_at < ? OR (created_at = ? AND id < ?))", decoded.CreatedAt, decoded.CreatedAt, decoded.ID)
		}
		return query.Order("created_at DESC, id DESC").Limit(limit + 1).Find(&records).Error
	})
	if err != nil {
		return biz.Page[biz.Message]{}, catalogError("list messages", err)
	}
	page := biz.Page[biz.Message]{Items: make([]biz.Message, 0, min(limit, len(records)))}
	if len(records) > limit {
		records = records[:limit]
		last := records[len(records)-1]
		page.NextCursor, err = EncodeCursor(Cursor{CreatedAt: last.CreatedAt, ID: last.ID})
		if err != nil {
			return biz.Page[biz.Message]{}, err
		}
	}
	for _, record := range records {
		message, err := toBizMessage(record)
		if err != nil {
			return biz.Page[biz.Message]{}, err
		}
		page.Items = append(page.Items, message)
	}
	return page, nil
}

// CountSessions 统计当前用户未删除的会话总数。
func (r *SessionRepository) CountSessions(ctx context.Context, userID uuid.UUID) (int64, error) {
	var count int64
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Model(&sessionModel{}).
			Where("user_id = ? AND parent_session_id IS NULL AND status <> ?", userID, biz.SessionDeleted).
			Count(&count).Error
	})
	if err != nil {
		return 0, catalogError("count sessions", err)
	}
	return count, nil
}

// normalizeSessionMemoryPolicy 为空策略补默认 private，避免违反数据库 CHECK 约束。
func normalizeSessionMemoryPolicy(policy biz.SessionMemoryPolicy) biz.SessionMemoryPolicy {
	if policy == "" {
		return biz.SessionMemoryPrivate
	}
	return policy
}

// toBizSession 将数据库行转换为领域会话。
func toBizSession(record sessionModel) biz.Session {
	return biz.Session{
		ID: record.ID, UserID: record.UserID, AgentID: record.AgentID,
		ParentSessionID: record.ParentSessionID, ParentRunID: record.ParentRunID,
		Title: record.Title, Source: biz.SessionSource(record.Source),
		MemoryPolicy: normalizeSessionMemoryPolicy(biz.SessionMemoryPolicy(record.MemoryPolicy)),
		Status:       biz.SessionStatus(record.Status), Version: record.Version, LastMessageAt: record.LastMessageAt,
		CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
	}
}

// toBizMessage 将数据库行转换为领域消息，并解码工具调用 JSON。
func toBizMessage(record messageModel) (biz.Message, error) {
	var toolCalls []map[string]any
	if err := json.Unmarshal(record.ToolCalls, &toolCalls); err != nil {
		return biz.Message{}, err
	}
	return biz.Message{
		ID: record.ID, SessionID: record.SessionID, UserID: record.UserID, Role: biz.MessageRole(record.Role), Content: record.Content,
		ToolCalls: toolCalls, ToolCallID: record.ToolCallID, ToolName: record.ToolName,
		TokenCount: record.TokenCount, ExternalMessageID: record.ExternalMessageID,
		SpeakerOpenID: record.SpeakerOpenID, RunID: record.RunID, CreatedAt: record.CreatedAt,
	}, nil
}

var _ biz.SessionRepository = (*SessionRepository)(nil)
