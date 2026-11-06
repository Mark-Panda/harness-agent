package data

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// feishuEventReceiptModel 对应 feishu_event_receipts 表：飞书事件幂等回执。
type feishuEventReceiptModel struct {
	// ID 回执主键。
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// EventID 飞书事件唯一 ID。
	EventID string
	// EventType 事件类型。
	EventType string
	// TenantKey 事件所属租户。
	TenantKey string
	// AppID 事件所属应用。
	AppID string
	// PayloadRedacted 服务端保存的原始事件载荷，仅用于重放。
	PayloadRedacted datatypes.JSON
	// UserID 事件发言人映射的系统用户；解析身份后回填。
	UserID *uuid.UUID `gorm:"type:uuid"`
	// Status 处理状态。
	Status string
	// Attempts 消费次数。
	Attempts int
	// LastError 脱敏后的错误。
	LastError string
	// ReceivedAt 接收时间。
	ReceivedAt time.Time
	// ProcessedAt 完成时间。
	ProcessedAt *time.Time
	// LastAttemptAt 最近消费时间。
	LastAttemptAt *time.Time
}

// TableName 指定事件回执表名。
func (feishuEventReceiptModel) TableName() string { return "feishu_event_receipts" }

// FeishuEventRepository 是飞书事件回执的 GORM 实现。
type FeishuEventRepository struct {
	db *gorm.DB
}

// NewFeishuEventRepository 构造事件回执仓库。
func NewFeishuEventRepository(db *gorm.DB) *FeishuEventRepository {
	return &FeishuEventRepository{db: db}
}

// RecordEvent 幂等写入事件回执；同一 event_id 重复投递返回 false。
func (r *FeishuEventRepository) RecordEvent(ctx context.Context, event biz.FeishuEventRecord) (bool, error) {
	if event.EventID == "" || event.EventType == "" {
		return false, fmt.Errorf("%w: feishu event id and type are required", biz.ErrInvalidConfig)
	}
	payload, err := json.Marshal(event.Payload)
	if err != nil {
		return false, err
	}
	record := feishuEventReceiptModel{
		ID: uuid.New(), EventID: event.EventID, EventType: event.EventType, TenantKey: event.TenantKey,
		AppID: event.AppID, PayloadRedacted: datatypes.JSON(payload), Status: biz.FeishuEventReceived,
		Attempts: event.Attempts, ReceivedAt: time.Now().UTC(),
	}
	var inserted bool
	err = WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		result := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "event_id"}}, DoNothing: true}).Create(&record)
		if result.Error != nil {
			return result.Error
		}
		inserted = result.RowsAffected == 1
		return nil
	})
	if err != nil {
		return false, catalogError("record feishu event", err)
	}
	return inserted, nil
}

// GetEvent 按事件 ID 读取回执。
func (r *FeishuEventRepository) GetEvent(ctx context.Context, eventID string) (biz.FeishuEventRecord, error) {
	var record feishuEventReceiptModel
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		return tx.First(&record, "event_id = ?", eventID).Error
	})
	if err != nil {
		return biz.FeishuEventRecord{}, catalogError("get feishu event", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(record.PayloadRedacted, &payload); err != nil {
		return biz.FeishuEventRecord{}, err
	}
	return biz.FeishuEventRecord{
		EventID: record.EventID, EventType: record.EventType, TenantKey: record.TenantKey, AppID: record.AppID,
		Payload: payload, UserID: record.UserID, Status: record.Status, Attempts: record.Attempts, LastError: record.LastError,
	}, nil
}

// BindUser 回填事件发言人对应的系统用户，供账号删除按用户清理原始载荷。
func (r *FeishuEventRepository) BindUser(ctx context.Context, eventID string, userID uuid.UUID) error {
	if eventID == "" || userID == uuid.Nil {
		return fmt.Errorf("%w: feishu event id and user id are required", biz.ErrInvalidConfig)
	}
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		return tx.Model(&feishuEventReceiptModel{}).Where("event_id = ?", eventID).
			Update("user_id", userID).Error
	})
	return catalogError("bind feishu event user", err)
}

// MarkEventStatus 更新回执状态；processing 递增消费次数，processed 记录完成时间。
func (r *FeishuEventRepository) MarkEventStatus(ctx context.Context, eventID, status, reason string, now time.Time) error {
	if len(reason) > 1024 {
		reason = reason[:1024]
	}
	updates := map[string]any{
		"status": status, "last_error": reason, "last_attempt_at": now,
	}
	if status == biz.FeishuEventProcessing {
		updates["attempts"] = gorm.Expr("attempts + 1")
	}
	if status == biz.FeishuEventProcessed || status == biz.FeishuEventIgnored {
		updates["processed_at"] = now
	}
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		return tx.Model(&feishuEventReceiptModel{}).Where("event_id = ?", eventID).Updates(updates).Error
	})
	return catalogError("mark feishu event status", err)
}

// userIdentityModel 对应 user_identities 表：外部身份到内部用户的映射。
type userIdentityModel struct {
	// ID 映射主键。
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// UserID 内部用户 ID。
	UserID uuid.UUID `gorm:"type:uuid"`
	// Provider 外部身份提供方。
	Provider string
	// TenantKey 外部租户。
	TenantKey string
	// ExternalSubject 外部主体，如飞书 open_id。
	ExternalSubject string
	// CreatedAt 创建时间。
	CreatedAt time.Time
}

// TableName 指定身份映射表名。
func (userIdentityModel) TableName() string { return "user_identities" }

// FeishuIdentityRepository 通过受限认证连接池解析飞书身份，避免应用角色跨用户读取映射。
type FeishuIdentityRepository struct {
	authDB *gorm.DB
}

// NewFeishuIdentityRepository 构造飞书身份仓库。
func NewFeishuIdentityRepository(authDB *gorm.DB) *FeishuIdentityRepository {
	return &FeishuIdentityRepository{authDB: authDB}
}

// Resolve 按 provider + tenant + open_id 精确查找身份；不存在时创建专用用户。
// 关键约束：绝不按昵称、邮箱或 union_id 合并已有账户，避免可伪造字段导致账户劫持。
func (r *FeishuIdentityRepository) Resolve(ctx context.Context, tenantKey, openID string) (biz.FeishuIdentity, error) {
	if tenantKey == "" || openID == "" {
		return biz.FeishuIdentity{}, fmt.Errorf("%w: feishu tenant and open id are required", biz.ErrInvalidConfig)
	}
	for attempt := 0; attempt < 2; attempt++ {
		identity, found, err := r.find(ctx, tenantKey, openID)
		if err != nil {
			return biz.FeishuIdentity{}, err
		}
		if found {
			return biz.FeishuIdentity{UserID: identity.UserID}, nil
		}
		created, err := r.create(ctx, tenantKey, openID)
		if err == nil {
			return created, nil
		}
		if !errors.Is(err, biz.ErrResourceConflict) {
			return biz.FeishuIdentity{}, err
		}
		// 并发首次消息可能同时创建身份，冲突后重新读取已存在的映射。
	}
	identity, found, err := r.find(ctx, tenantKey, openID)
	if err != nil {
		return biz.FeishuIdentity{}, err
	}
	if !found {
		return biz.FeishuIdentity{}, fmt.Errorf("%w: feishu identity conflict requires manual resolution", biz.ErrResourceConflict)
	}
	return biz.FeishuIdentity{UserID: identity.UserID}, nil
}

// find 使用认证连接池与身份作用域读取映射，并校验用户未被禁用。
func (r *FeishuIdentityRepository) find(ctx context.Context, tenantKey, openID string) (userIdentityModel, bool, error) {
	var identity userIdentityModel
	err := WithScope(ctx, r.authDB, Scope{
		IdentityProvider: "feishu", IdentityTenant: tenantKey, IdentitySubject: openID,
	}, func(tx *gorm.DB) error {
		result := tx.First(&identity, "provider = ? AND tenant_key = ? AND external_subject = ?", "feishu", tenantKey, openID)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil
		}
		return result.Error
	})
	if err != nil {
		return userIdentityModel{}, false, catalogError("find feishu identity", err)
	}
	if identity.UserID == uuid.Nil {
		return userIdentityModel{}, false, nil
	}
	var user userModel
	err = WithScope(ctx, r.authDB, Scope{UserID: identity.UserID}, func(tx *gorm.DB) error {
		return tx.First(&user, "id = ?", identity.UserID).Error
	})
	if err != nil {
		return userIdentityModel{}, false, catalogError("load feishu identity user", err)
	}
	if biz.UserStatus(user.Status) != biz.UserStatusActive {
		return userIdentityModel{}, false, biz.ErrForbidden
	}
	return identity, true, nil
}

// create 在注册作用域内创建专用用户与身份映射。
func (r *FeishuIdentityRepository) create(ctx context.Context, tenantKey, openID string) (biz.FeishuIdentity, error) {
	userID := uuid.New()
	username := feishuUsername(tenantKey, openID)
	user := userModel{
		ID: userID, Username: username, UsernameNormalized: biz.NormalizeUsername(username),
		// 占位凭据不可用于登录；飞书用户只能通过身份映射进入系统。
		PasswordHash: "!feishu:no-local-login", Role: string(biz.RoleUser), Status: string(biz.UserStatusActive),
		CredentialVersion: 1,
	}
	identity := userIdentityModel{
		ID: uuid.New(), UserID: userID, Provider: "feishu", TenantKey: tenantKey, ExternalSubject: openID,
	}
	err := WithScope(ctx, r.authDB, Scope{
		UserID: userID, RegistrationEnabled: true,
		IdentityProvider: "feishu", IdentityTenant: tenantKey, IdentitySubject: openID,
	}, func(tx *gorm.DB) error {
		if err := tx.Create(&user).Error; err != nil {
			return err
		}
		return tx.Create(&identity).Error
	})
	if err != nil {
		return biz.FeishuIdentity{}, catalogError("create feishu identity", err)
	}
	return biz.FeishuIdentity{UserID: userID, Created: true}, nil
}

// feishuUsername 由租户与 open_id 派生稳定用户名，不含昵称等可伪造信息。
func feishuUsername(tenantKey, openID string) string {
	sum := sha256.Sum256([]byte(tenantKey + "\x00" + openID))
	return "feishu_" + hex.EncodeToString(sum[:])[:20]
}

// externalBindingModel 对应 external_conversation_bindings 表：飞书话题与内部会话映射。
type externalBindingModel struct {
	// ID 绑定主键。
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// UserID 会话所有者。
	UserID uuid.UUID `gorm:"type:uuid"`
	// Provider 外部渠道。
	Provider string
	// TenantKey 飞书租户。
	TenantKey string
	// ChatID 飞书会话。
	ChatID string
	// ThreadID 话题键。
	ThreadID *string
	// RootMessageID 过渡别名。
	RootMessageID *string
	// SessionID 内部会话。
	SessionID uuid.UUID `gorm:"type:uuid"`
	// Status 绑定状态。
	Status string
	// CreatedAt 创建时间。
	CreatedAt time.Time
	// UpdatedAt 更新时间。
	UpdatedAt time.Time
}

// TableName 指定外部会话绑定表名。
func (externalBindingModel) TableName() string { return "external_conversation_bindings" }

// FeishuConversationRepositoryImpl 实现飞书话题绑定仓储。
type FeishuConversationRepositoryImpl struct {
	db *gorm.DB
}

// NewFeishuConversationRepository 构造飞书话题绑定仓储。
func NewFeishuConversationRepository(db *gorm.DB) *FeishuConversationRepositoryImpl {
	return &FeishuConversationRepositoryImpl{db: db}
}

// FindConversation 以系统作用域按 tenant/chat 查找绑定：优先匹配话题键，未命中时按根消息别名回退。
// 已解绑（unbound）记录不参与匹配，保证解绑后同一话题的下一轮 @ 能创建全新会话。
func (r *FeishuConversationRepositoryImpl) FindConversation(ctx context.Context, tenantKey, chatID, threadID, rootMessageID string) (biz.FeishuConversationBinding, bool, error) {
	var record externalBindingModel
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		result := tx.Where("provider = ? AND tenant_key = ? AND chat_id = ? AND thread_id = ? AND status <> ?",
			"feishu", tenantKey, chatID, threadID, biz.FeishuBindingUnbound).
			First(&record)
		if result.Error == nil {
			return nil
		}
		if !errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return result.Error
		}
		// 话题键未命中时按根消息别名回退：主时间线首条 @ 以消息 ID 建会话，
		// 飞书创建真实话题后追问携带 thread_id，此时需通过 root_id 找回同一绑定。
		if rootMessageID == "" || rootMessageID == threadID {
			return nil
		}
		alias := externalBindingModel{}
		aliasResult := tx.Where("provider = ? AND tenant_key = ? AND chat_id = ? AND root_message_id = ? AND status <> ?",
			"feishu", tenantKey, chatID, rootMessageID, biz.FeishuBindingUnbound).
			First(&alias)
		if aliasResult.Error == nil {
			record = alias
			return nil
		}
		if !errors.Is(aliasResult.Error, gorm.ErrRecordNotFound) {
			return aliasResult.Error
		}
		return nil
	})
	if err != nil {
		return biz.FeishuConversationBinding{}, false, catalogError("find feishu conversation", err)
	}
	if record.ID == uuid.Nil {
		return biz.FeishuConversationBinding{}, false, nil
	}
	return toFeishuConversationBinding(record), true, nil
}

// EnsureConversation 在同一事务创建 Session、所有者成员、绑定与审计事件。
// 唯一约束冲突说明其他并发事件已创建话题，此时返回既有绑定，禁止创建群级共享会话。
func (r *FeishuConversationRepositoryImpl) EnsureConversation(ctx context.Context, binding biz.FeishuConversationBinding, session biz.Session, event biz.OutboxEvent) (biz.FeishuConversationBinding, biz.Session, error) {
	threadID := binding.ThreadID
	rootMessageID := binding.RootMessageID
	record := externalBindingModel{
		ID: binding.ID, UserID: binding.UserID, Provider: binding.Provider, TenantKey: binding.TenantKey,
		ChatID: binding.ChatID, ThreadID: &threadID, SessionID: binding.SessionID, Status: biz.FeishuBindingActive,
	}
	if rootMessageID != "" {
		record.RootMessageID = &rootMessageID
	}
	sessionRecord := sessionModel{
		ID: session.ID, UserID: session.UserID, AgentID: session.AgentID, Title: session.Title,
		ParentSessionID: session.ParentSessionID, ParentRunID: session.ParentRunID,
		Source: string(session.Source), MemoryPolicy: string(normalizeSessionMemoryPolicy(session.MemoryPolicy)),
		Status: string(session.Status), Version: session.Version,
	}
	member := sessionMemberModel{SessionID: session.ID, UserID: session.UserID, MemberRole: "owner"}
	err := WithScope(ctx, r.db, Scope{UserID: binding.UserID}, func(tx *gorm.DB) error {
		if err := tx.Create(&sessionRecord).Error; err != nil {
			return err
		}
		if err := tx.Create(&member).Error; err != nil {
			return err
		}
		if err := tx.Create(&record).Error; err != nil {
			return err
		}
		return appendOutbox(tx, event)
	})
	if err == nil {
		return toFeishuConversationBinding(record), toBizSession(sessionRecord), nil
	}
	if !errors.Is(err, gorm.ErrDuplicatedKey) {
		return biz.FeishuConversationBinding{}, biz.Session{}, catalogError("ensure feishu conversation", err)
	}
	existing, found, findErr := r.FindConversation(ctx, binding.TenantKey, binding.ChatID, binding.ThreadID, rootMessageID)
	if findErr != nil {
		return biz.FeishuConversationBinding{}, biz.Session{}, findErr
	}
	if !found {
		return biz.FeishuConversationBinding{}, biz.Session{}, fmt.Errorf("%w: feishu conversation conflict", biz.ErrResourceConflict)
	}
	var sessionRow sessionModel
	err = WithScope(ctx, r.db, Scope{UserID: existing.UserID}, func(tx *gorm.DB) error {
		return tx.First(&sessionRow, "id = ? AND user_id = ?", existing.SessionID, existing.UserID).Error
	})
	if err != nil {
		return biz.FeishuConversationBinding{}, biz.Session{}, catalogError("load feishu conversation session", err)
	}
	return existing, toBizSession(sessionRow), nil
}

// ReleaseConversation 关闭指定会话下活跃的飞书绑定，并追加审计事件。
// 关键步骤：更新条件必须同时包含 user_id 与 session_id，避免跨用户解绑他人话题。
func (r *FeishuConversationRepositoryImpl) ReleaseConversation(ctx context.Context, userID, sessionID uuid.UUID, status string, event biz.OutboxEvent) (int, error) {
	if userID == uuid.Nil || sessionID == uuid.Nil || status == "" {
		return 0, fmt.Errorf("%w: release requires user, session and status", biz.ErrInvalidConfig)
	}
	var affected int64
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		result := tx.Model(&externalBindingModel{}).
			Where("user_id = ? AND session_id = ? AND status = ?", userID, sessionID, biz.FeishuBindingActive).
			Updates(map[string]any{"status": status, "updated_at": time.Now().UTC()})
		if result.Error != nil {
			return result.Error
		}
		affected = result.RowsAffected
		// 幂等重复解绑不产生新的审计事件，避免审计噪声。
		if affected == 0 {
			return nil
		}
		return appendOutbox(tx, event)
	})
	if err != nil {
		return 0, catalogError("release feishu conversation", err)
	}
	return int(affected), nil
}

// RestoreConversation 将已关闭或已归档的绑定恢复为活跃，并追加审计事件。
// 关键步骤：更新条件必须同时包含 user_id 与 session_id；已活跃绑定幂等返回 0。
func (r *FeishuConversationRepositoryImpl) RestoreConversation(ctx context.Context, userID, sessionID uuid.UUID, event biz.OutboxEvent) (int, error) {
	if userID == uuid.Nil || sessionID == uuid.Nil {
		return 0, fmt.Errorf("%w: restore requires user and session", biz.ErrInvalidConfig)
	}
	var affected int64
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		result := tx.Model(&externalBindingModel{}).
			Where("user_id = ? AND session_id = ? AND status IN ?", userID, sessionID, []string{biz.FeishuBindingClosed, biz.FeishuBindingArchived}).
			Updates(map[string]any{"status": biz.FeishuBindingActive, "updated_at": time.Now().UTC()})
		if result.Error != nil {
			return result.Error
		}
		affected = result.RowsAffected
		// 幂等重复恢复不产生新的审计事件，避免审计噪声。
		if affected == 0 {
			return nil
		}
		return appendOutbox(tx, event)
	})
	if err != nil {
		return 0, catalogError("restore feishu conversation", err)
	}
	return int(affected), nil
}

// PromoteConversationThread 把主时间线根消息过渡别名升级为飞书真实话题 ID。
// 升级后话题内追问可直接按 thread_id 命中绑定，无需依赖 root_id 回退。
func (r *FeishuConversationRepositoryImpl) PromoteConversationThread(ctx context.Context, userID, sessionID uuid.UUID, legacyThreadID, threadID string) (int, error) {
	if userID == uuid.Nil || sessionID == uuid.Nil || legacyThreadID == "" || threadID == "" {
		return 0, fmt.Errorf("%w: promote requires user, session and thread ids", biz.ErrInvalidConfig)
	}
	if legacyThreadID == threadID {
		return 0, nil
	}
	var affected int64
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		result := tx.Model(&externalBindingModel{}).
			Where("user_id = ? AND session_id = ? AND thread_id = ? AND status = ?", userID, sessionID, legacyThreadID, biz.FeishuBindingActive).
			Update("thread_id", threadID)
		if result.Error != nil && errors.Is(result.Error, gorm.ErrDuplicatedKey) {
			// 其他副本已完成升级并写入了真实话题 ID，视为幂等成功。
			affected = 0
			return nil
		}
		if result.Error != nil {
			return result.Error
		}
		affected = result.RowsAffected
		return nil
	})
	if err != nil {
		return 0, catalogError("promote feishu conversation thread", err)
	}
	return int(affected), nil
}

// IsSharedSession 判断会话是否存在群聊话题绑定；查询必须按 user_id 与会话 ID 限定。
// 关键步骤：单聊绑定固定使用 p2p 话题键，任何非 p2p 的历史绑定都视为群聊共享会话。
func (r *FeishuConversationRepositoryImpl) IsSharedSession(ctx context.Context, userID, sessionID uuid.UUID) (bool, error) {
	if userID == uuid.Nil || sessionID == uuid.Nil {
		return false, fmt.Errorf("%w: shared session requires user and session", biz.ErrInvalidConfig)
	}
	var count int64
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Model(&externalBindingModel{}).
			Where("user_id = ? AND session_id = ? AND thread_id IS NOT NULL AND thread_id <> ?", userID, sessionID, "p2p").
			Count(&count).Error
	})
	if err != nil {
		return false, catalogError("detect shared session", err)
	}
	return count > 0, nil
}

// toFeishuConversationBinding 将数据库行转换为领域绑定。
func toFeishuConversationBinding(record externalBindingModel) biz.FeishuConversationBinding {
	threadID, rootMessageID := "", ""
	if record.ThreadID != nil {
		threadID = *record.ThreadID
	}
	if record.RootMessageID != nil {
		rootMessageID = *record.RootMessageID
	}
	return biz.FeishuConversationBinding{
		ID: record.ID, UserID: record.UserID, Provider: record.Provider, TenantKey: record.TenantKey,
		ChatID: record.ChatID, ThreadID: threadID, RootMessageID: rootMessageID,
		SessionID: record.SessionID, Status: record.Status, CreatedAt: record.CreatedAt,
	}
}

// FeishuMessageRepository 实现飞书入站消息幂等写入。
type FeishuMessageRepository struct {
	db *gorm.DB
}

// NewFeishuMessageRepository 构造飞书消息仓库。
func NewFeishuMessageRepository(db *gorm.DB) *FeishuMessageRepository {
	return &FeishuMessageRepository{db: db}
}

// AppendInbound 按 external_message_id 幂等追加用户消息，并推进会话最近消息时间。
func (r *FeishuMessageRepository) AppendInbound(ctx context.Context, message biz.Message, event biz.OutboxEvent) (biz.Message, bool, error) {
	if message.SessionID == uuid.Nil || message.UserID == uuid.Nil || message.ExternalMessageID == "" {
		return biz.Message{}, false, fmt.Errorf("%w: inbound message requires session, user and external id", biz.ErrInvalidConfig)
	}
	toolCalls, err := json.Marshal(message.ToolCalls)
	if message.ToolCalls == nil {
		toolCalls = []byte("[]")
	} else if err != nil {
		return biz.Message{}, false, err
	}
	now := time.Now().UTC()
	// 乱序保护：优先使用调用方传入的事件发生时间，缺失时才回退到本地接收时间。
	occurredAt := message.CreatedAt
	if occurredAt.IsZero() {
		occurredAt = now
	}
	record := messageModel{
		ID: message.ID, SessionID: message.SessionID, UserID: message.UserID, Role: string(biz.MessageUser),
		Content: message.Content, ToolCalls: datatypes.JSON(toolCalls), ExternalMessageID: message.ExternalMessageID,
		SpeakerOpenID: message.SpeakerOpenID, CreatedAt: occurredAt,
	}
	var created bool
	var existing messageModel
	err = WithScope(ctx, r.db, Scope{UserID: message.UserID}, func(tx *gorm.DB) error {
		var session sessionModel
		if err := tx.First(&session, "id = ? AND user_id = ?", message.SessionID, message.UserID).Error; err != nil {
			return err
		}
		if biz.SessionStatus(session.Status) != biz.SessionActive {
			return fmt.Errorf("%w: session is not active", biz.ErrResourceConflict)
		}
		result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&record)
		if result.Error != nil {
			return result.Error
		}
		created = result.RowsAffected == 1
		if !created {
			return tx.First(&existing, "session_id = ? AND external_message_id = ?", message.SessionID, message.ExternalMessageID).Error
		}
		// 会话最近消息时间必须单调不减：旧事件晚到时不得把 last_message_at 回退到更早时间，
		// 否则会话列表与后续上下文裁剪会误判最新消息。
		update := tx.Model(&sessionModel{}).Where("id = ? AND user_id = ?", message.SessionID, message.UserID).
			Updates(map[string]any{
				"last_message_at": gorm.Expr("GREATEST(last_message_at, ?)", occurredAt),
				"updated_at":      now,
				"version":         gorm.Expr("version + 1"),
			})
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return fmt.Errorf("%w: session is missing", biz.ErrResourceConflict)
		}
		return appendOutbox(tx, event)
	})
	if err != nil {
		return biz.Message{}, false, catalogError("append feishu inbound message", err)
	}
	if !created {
		existingMessage, err := toBizMessage(existing)
		if err != nil {
			return biz.Message{}, false, err
		}
		return existingMessage, false, nil
	}
	createdMessage, err := toBizMessage(record)
	if err != nil {
		return biz.Message{}, false, err
	}
	return createdMessage, true, nil
}

// feishuOutboundModel 对应 feishu_outbound_messages 表。
type feishuOutboundModel struct {
	// ID 外发记录主键。
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// UserID 会话所有者。
	UserID uuid.UUID `gorm:"type:uuid"`
	// SessionID 关联会话。
	SessionID uuid.UUID `gorm:"type:uuid"`
	// RunID 关联 Run。
	RunID uuid.UUID `gorm:"type:uuid"`
	// AppID 接收消息的飞书应用。
	AppID string
	// ChatID 目标会话。
	ChatID string
	// ThreadID 目标话题。
	ThreadID string
	// ReplyToMessageID 触发消息。
	ReplyToMessageID string
	// MessageID 已发送消息 ID。
	MessageID string
	// Kind 回复形态。
	Kind string
	// Status 投递状态。
	Status string
	// Attempts 已尝试次数。
	Attempts int
	// MaxAttempts 最大尝试次数。
	MaxAttempts int
	// AvailableAt 下次可投递时间。
	AvailableAt time.Time
	// LeaseOwner 租约持有者。
	LeaseOwner string
	// LeaseExpiresAt 租约到期时间。
	LeaseExpiresAt *time.Time
	// DedupeKey 业务去重键。
	DedupeKey string
	// CardUpdatedAt 最近一次卡片成功更新时间，用于限制更新频率。
	CardUpdatedAt *time.Time
	// CardFingerprint 最近一次成功写入卡片的标题与正文指纹。
	CardFingerprint string
	// LastError 脱敏错误。
	LastError string
	// CreatedAt 创建时间。
	CreatedAt time.Time
	// UpdatedAt 更新时间。
	UpdatedAt time.Time
	// CompletedAt 完成时间。
	CompletedAt *time.Time
}

// TableName 指定外发表名。
func (feishuOutboundModel) TableName() string { return "feishu_outbound_messages" }

// FeishuOutboundRepository 是外发回复的 GORM 实现。
type FeishuOutboundRepository struct {
	db *gorm.DB
}

// NewFeishuOutboundRepository 构造外发回复仓库。
func NewFeishuOutboundRepository(db *gorm.DB) *FeishuOutboundRepository {
	return &FeishuOutboundRepository{db: db}
}

// CreateOutbound 以 dedupe_key 幂等创建外发记录。
func (r *FeishuOutboundRepository) CreateOutbound(ctx context.Context, outbound biz.FeishuOutbound) error {
	if outbound.DedupeKey == "" || outbound.ChatID == "" || outbound.RunID == uuid.Nil {
		return fmt.Errorf("%w: outbound requires dedupe key, chat and run", biz.ErrInvalidConfig)
	}
	record := feishuOutboundModel{
		ID: outbound.ID, UserID: outbound.UserID, SessionID: outbound.SessionID, RunID: outbound.RunID, AppID: outbound.AppID,
		ChatID: outbound.ChatID, ThreadID: outbound.ThreadID, ReplyToMessageID: outbound.ReplyToMessageID,
		Kind: outbound.Kind, Status: "pending", MaxAttempts: 8, AvailableAt: time.Now().UTC(),
		DedupeKey: outbound.DedupeKey, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "dedupe_key"}}, DoNothing: true}).Create(&record).Error
	})
	return catalogError("create feishu outbound", err)
}

// ClaimOutbound 领取一条到期外发记录；sending 且租约过期的记录允许恢复。
func (r *FeishuOutboundRepository) ClaimOutbound(ctx context.Context, workerID string, lease time.Duration, now time.Time) (biz.FeishuOutbound, bool, error) {
	if workerID == "" || lease <= 0 {
		return biz.FeishuOutbound{}, false, fmt.Errorf("%w: worker and lease are required", biz.ErrInvalidConfig)
	}
	type claimed struct {
		ID               uuid.UUID
		UserID           uuid.UUID
		SessionID        uuid.UUID
		RunID            uuid.UUID
		AppID            string
		ChatID           string
		ThreadID         string
		ReplyToMessageID string
		MessageID        string
		Kind             string
		Status           string
		Attempts         int
		MaxAttempts      int
		DedupeKey        string
		CardUpdatedAt    *time.Time
		CardFingerprint  string
		CreatedAt        time.Time
	}
	var record claimed
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		query := `
WITH candidate AS (
  SELECT id FROM feishu_outbound_messages
  WHERE (
    (status IN ('pending', 'retrying') AND available_at <= ?)
    OR (status = 'sending' AND (lease_expires_at IS NULL OR lease_expires_at < ?))
  )
  ORDER BY available_at, created_at, id
  FOR UPDATE SKIP LOCKED
  LIMIT 1
)
UPDATE feishu_outbound_messages SET status = 'sending', attempts = attempts + 1,
  lease_owner = ?, lease_expires_at = ?, updated_at = ?
FROM candidate WHERE feishu_outbound_messages.id = candidate.id
RETURNING feishu_outbound_messages.id, feishu_outbound_messages.user_id, feishu_outbound_messages.session_id,
  feishu_outbound_messages.run_id, feishu_outbound_messages.app_id, feishu_outbound_messages.chat_id, feishu_outbound_messages.thread_id,
  feishu_outbound_messages.reply_to_message_id, feishu_outbound_messages.message_id, feishu_outbound_messages.kind,
  feishu_outbound_messages.status, feishu_outbound_messages.attempts, feishu_outbound_messages.max_attempts,
  feishu_outbound_messages.dedupe_key, feishu_outbound_messages.card_updated_at,
  feishu_outbound_messages.card_fingerprint, feishu_outbound_messages.created_at`
		return tx.Raw(query, now, now, workerID, now.Add(lease), now).Scan(&record).Error
	})
	if err != nil {
		return biz.FeishuOutbound{}, false, catalogError("claim feishu outbound", err)
	}
	if record.ID == uuid.Nil {
		return biz.FeishuOutbound{}, false, nil
	}
	var cardUpdatedAt time.Time
	if record.CardUpdatedAt != nil {
		cardUpdatedAt = *record.CardUpdatedAt
	}
	return biz.FeishuOutbound{
		ID: record.ID, UserID: record.UserID, SessionID: record.SessionID, RunID: record.RunID, AppID: record.AppID,
		ChatID: record.ChatID, ThreadID: record.ThreadID, ReplyToMessageID: record.ReplyToMessageID,
		MessageID: record.MessageID, Kind: record.Kind, Status: record.Status, Attempts: record.Attempts,
		MaxAttempts: record.MaxAttempts, DedupeKey: record.DedupeKey, CardUpdatedAt: cardUpdatedAt,
		CardFingerprint: record.CardFingerprint, CreatedAt: record.CreatedAt,
	}, true, nil
}

// UpdateOutboundMessageID 保存飞书回复消息 ID。
func (r *FeishuOutboundRepository) UpdateOutboundMessageID(ctx context.Context, id uuid.UUID, messageID string, now time.Time) error {
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		return tx.Model(&feishuOutboundModel{}).Where("id = ?", id).
			Updates(map[string]any{"message_id": messageID, "updated_at": now}).Error
	})
	return catalogError("update feishu outbound message id", err)
}

// MarkOutboundCardUpdated 记录卡片成功更新时间与内容指纹，供跨副本更新节流与去重。
func (r *FeishuOutboundRepository) MarkOutboundCardUpdated(ctx context.Context, id uuid.UUID, fingerprint string, now time.Time) error {
	if fingerprint == "" {
		return fmt.Errorf("%w: card fingerprint is required", biz.ErrInvalidConfig)
	}
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		return tx.Model(&feishuOutboundModel{}).Where("id = ?", id).
			Updates(map[string]any{"card_updated_at": now, "card_fingerprint": fingerprint, "updated_at": now}).Error
	})
	return catalogError("mark feishu card updated", err)
}

// DeferOutbound 延后检查且回退本次领取计数，避免轮询耗尽失败预算。
func (r *FeishuOutboundRepository) DeferOutbound(ctx context.Context, id uuid.UUID, retryAt, now time.Time) error {
	if retryAt.IsZero() {
		retryAt = now
	}
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		return tx.Model(&feishuOutboundModel{}).Where("id = ? AND status = 'sending'", id).
			Updates(map[string]any{
				"status": "pending", "attempts": gorm.Expr("GREATEST(attempts - 1, 0)"),
				"available_at": retryAt, "lease_owner": "", "lease_expires_at": nil, "updated_at": now,
			}).Error
	})
	return catalogError("defer feishu outbound", err)
}

// CompleteOutbound 标记外发成功。
func (r *FeishuOutboundRepository) CompleteOutbound(ctx context.Context, id uuid.UUID, now time.Time) error {
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		return tx.Model(&feishuOutboundModel{}).Where("id = ? AND status = 'sending'", id).
			Updates(map[string]any{
				"status": "completed", "completed_at": now, "lease_owner": "", "lease_expires_at": nil, "updated_at": now,
			}).Error
	})
	return catalogError("complete feishu outbound", err)
}

// FailOutbound 记录失败并在预算内退避，超出预算进入死信。
func (r *FeishuOutboundRepository) FailOutbound(ctx context.Context, id uuid.UUID, reason string, retryAt, now time.Time) error {
	if len(reason) > 1024 {
		reason = reason[:1024]
	}
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		var record feishuOutboundModel
		if err := tx.First(&record, "id = ? AND status = 'sending'", id).Error; err != nil {
			return err
		}
		status := "retrying"
		updates := map[string]any{
			"status": status, "last_error": reason, "lease_owner": "", "lease_expires_at": nil, "updated_at": now,
		}
		if record.Attempts >= record.MaxAttempts {
			updates["status"] = "dead"
			updates["completed_at"] = now
		} else {
			if retryAt.IsZero() {
				retryAt = now
			}
			updates["available_at"] = retryAt
		}
		return tx.Model(&feishuOutboundModel{}).Where("id = ?", id).Updates(updates).Error
	})
	return catalogError("fail feishu outbound", err)
}

// LatestAssistantMessage 读取指定 Run 产生的助手消息；runID 为空时退回会话最近一条助手消息。
// 按 run_id 过滤可避免并发 Run 的回复互相串台。
func (r *FeishuOutboundRepository) LatestAssistantMessage(ctx context.Context, userID, sessionID uuid.UUID, runID *uuid.UUID) (biz.Message, error) {
	var record messageModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		query := tx.Where("user_id = ? AND session_id = ? AND role = ? AND deleted_at IS NULL", userID, sessionID, biz.MessageAssistant)
		if runID != nil && *runID != uuid.Nil {
			query = query.Where("run_id = ?", *runID)
		}
		return query.Order("created_at DESC, id DESC").First(&record).Error
	})
	if err != nil {
		return biz.Message{}, catalogError("latest feishu assistant message", err)
	}
	return toBizMessage(record)
}

// 编译期断言接口实现。
var (
	_ biz.FeishuEventStore             = (*FeishuEventRepository)(nil)
	_ biz.FeishuIdentityStore          = (*FeishuIdentityRepository)(nil)
	_ biz.FeishuConversationRepository = (*FeishuConversationRepositoryImpl)(nil)
	_ biz.FeishuInboundMessageStore    = (*FeishuMessageRepository)(nil)
	_ biz.FeishuOutboundStore          = (*FeishuOutboundRepository)(nil)
)
