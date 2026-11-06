package biz

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// SessionSource 会话来源渠道。
type SessionSource string

// SessionStatus 会话生命周期状态。
type SessionStatus string

// SessionMemoryPolicy 会话记忆策略，决定上下文注入与记忆写入的范围。
type SessionMemoryPolicy string

// MessageRole 消息角色，对应模型上下文中的 role。
type MessageRole string

const (
	// SessionSourceWeb Web 端发起的会话。
	SessionSourceWeb SessionSource = "web"
	// SessionSourceFeishu 飞书单聊或话题会话。
	SessionSourceFeishu SessionSource = "feishu"
	// SessionSourceAPI 外部 API 发起的会话。
	SessionSourceAPI SessionSource = "api"
	// SessionActive 会话活跃，可追加消息。
	SessionActive SessionStatus = "active"
	// SessionArchived 会话已归档，只读。
	SessionArchived SessionStatus = "archived"
	// SessionDeleted 会话已删除（软删除标记）。
	SessionDeleted SessionStatus = "deleted"
	// SessionMemoryPrivate 私有记忆：允许注入与写入个人长期记忆和结构化画像。
	SessionMemoryPrivate SessionMemoryPolicy = "private"
	// SessionMemoryGroup 群聊话题记忆：禁止注入个人长期记忆与结构化画像，也不写入个人事实。
	SessionMemoryGroup SessionMemoryPolicy = "group"
	// SessionMemoryDisabled 禁用记忆：不注入长期记忆、画像与摘要，也不触发任何记忆写入。
	SessionMemoryDisabled SessionMemoryPolicy = "disabled"
	// MessageSystem 系统提示词消息。
	MessageSystem MessageRole = "system"
	// MessageUser 用户输入消息。
	MessageUser MessageRole = "user"
	// MessageAssistant 模型回复消息。
	MessageAssistant MessageRole = "assistant"
	// MessageTool 工具返回消息。
	MessageTool MessageRole = "tool"
)

// Session 是用户私有的会话聚合，承载消息序列与上下文归属。
type Session struct {
	// ID 会话唯一标识。
	ID uuid.UUID
	// UserID 所属用户，所有查询强制携带。
	UserID uuid.UUID
	// AgentID 绑定的已发布 Agent，可为空。
	AgentID *uuid.UUID
	// ParentSessionID Supervisor 子会话的父会话，普通会话为空。
	ParentSessionID *uuid.UUID
	// ParentRunID Supervisor 子会话关联的主 Run，普通会话为空。
	ParentRunID *uuid.UUID
	// Title 会话标题。
	Title string
	// Source 来源渠道。
	Source SessionSource
	// MemoryPolicy 记忆策略：private/group/disabled，默认 private。
	MemoryPolicy SessionMemoryPolicy
	// Status 生命周期状态。
	Status SessionStatus
	// Version 乐观锁版本号，归档等写操作使用。
	Version int64
	// LastMessageAt 最近一条消息时间，用于会话列表排序。
	LastMessageAt *time.Time
	// CreatedAt 创建时间（UTC）。
	CreatedAt time.Time
	// UpdatedAt 最后更新时间（UTC）。
	UpdatedAt time.Time
}

// Message 是会话中的规范消息，工具调用仅保存元数据，不保存隐藏思维链。
type Message struct {
	// ID 消息唯一标识。
	ID uuid.UUID
	// SessionID 所属会话。
	SessionID uuid.UUID
	// UserID 所属用户，用于 RLS 隔离。
	UserID uuid.UUID
	// Role 消息角色。
	Role MessageRole
	// Content 消息正文，上限 4MB。
	Content string
	// ToolCalls 工具调用列表，仅保存协议元数据。
	ToolCalls []map[string]any
	// ToolCallID 工具结果对应的调用 ID，用于协议回放。
	ToolCallID string
	// ToolName 工具名称，用于执行树展示。
	ToolName string
	// TokenCount 消息 Token 估算，供上下文预算使用。
	TokenCount int
	// ExternalMessageID 外部渠道消息 ID，用于幂等去重。
	ExternalMessageID string
	// SpeakerOpenID 飞书群聊发言人 open_id，单聊为空。
	SpeakerOpenID string
	// RunID 产生该消息的 Run ID；用户消息为空。
	RunID *uuid.UUID
	// CreatedAt 创建时间（UTC）。
	CreatedAt time.Time
}

// SessionMemoryPolicyReader 读取会话记忆策略的窄接口，避免 Worker 依赖完整会话仓储。
type SessionMemoryPolicyReader interface {
	// GetSession 按用户与会话 ID 读取会话，跨用户查询必须返回 ErrResourceNotFound。
	GetSession(ctx context.Context, userID, sessionID uuid.UUID) (Session, error)
}

// SharedSessionDetector 判断会话是否为群聊等共享会话；共享会话禁止启用私有记忆注入。
type SharedSessionDetector interface {
	// IsSharedSession 返回会话是否绑定群聊话题；实现必须按 user_id 限定查询范围。
	IsSharedSession(ctx context.Context, userID, sessionID uuid.UUID) (bool, error)
}

// SessionRepository 定义会话与消息持久化契约，全部方法强制携带 userID。
type SessionRepository interface {
	// CreateSession 创建会话与所有者成员并写入审计事件。
	CreateSession(ctx context.Context, session Session, event OutboxEvent) (Session, error)
	// GetSession 按用户与会话 ID 读取。
	GetSession(ctx context.Context, userID, sessionID uuid.UUID) (Session, error)
	// ListSessions 按用户游标分页列出会话。
	ListSessions(ctx context.Context, userID uuid.UUID, limit int, cursor string) (Page[Session], error)
	// ArchiveSession 以版本号乐观锁归档会话。
	ArchiveSession(ctx context.Context, session Session, event OutboxEvent) (Session, error)
	// UpdateSessionMemoryPolicy 以版本号乐观锁更新会话记忆策略。
	UpdateSessionMemoryPolicy(ctx context.Context, session Session, event OutboxEvent) (Session, error)
	// GetAgent 读取绑定 Agent，用于校验已发布状态。
	GetAgent(ctx context.Context, userID, agentID uuid.UUID) (Agent, error)
	// AppendMessage 追加消息并更新会话最近消息时间。
	AppendMessage(ctx context.Context, message Message, event OutboxEvent) (Message, error)
	// AppendMessageIdempotent 按消息 ID 幂等追加 Agent 消息；已存在时返回原消息且不重复写审计事件。
	AppendMessageIdempotent(ctx context.Context, message Message, event OutboxEvent) (Message, error)
	// ListMessages 按会话游标分页列出消息（倒序）。
	ListMessages(ctx context.Context, userID, sessionID uuid.UUID, limit int, cursor string) (Page[Message], error)
	// CountSessions 统计当前用户会话总数，用于配额校验。
	CountSessions(ctx context.Context, userID uuid.UUID) (int64, error)
}

// HotMessageStore 是会话热消息缓存端口；Key 必须同时包含 user_id 与 session_id。
// 实现方负责 TTL、长度上限与不缓存空列表，读取失败时调用方回退 PostgreSQL。
type HotMessageStore interface {
	// Load 按时间正序读取热消息；未命中返回 false，不得用空列表伪装命中。
	Load(ctx context.Context, userID, sessionID uuid.UUID) ([]Message, bool, error)
	// Save 用完整消息列表重建缓存；空列表必须清理缓存而不是写入空值。
	Save(ctx context.Context, userID, sessionID uuid.UUID, messages []Message) error
	// Append 幂等追加单条消息并裁剪到长度上限。
	Append(ctx context.Context, userID, sessionID uuid.UUID, message Message) error
	// Invalidate 清理缓存，用于归档或检测到损坏数据。
	Invalidate(ctx context.Context, userID, sessionID uuid.UUID) error
}

// SessionServiceOption 按运行环境装配会话服务可选能力。
type SessionServiceOption func(*SessionService)

// WithHotMessages 注入会话热消息缓存；缺失时只使用 PostgreSQL。
func WithHotMessages(store HotMessageStore) SessionServiceOption {
	return func(s *SessionService) {
		s.hot = store
	}
}

// WithSummaryTrigger 注入摘要触发端口；缺失时不在消息写入路径触发摘要。
func WithSummaryTrigger(trigger SummaryTrigger) SessionServiceOption {
	return func(s *SessionService) {
		s.summary = trigger
	}
}

// WithMemoryTrigger 注入向量化触发端口；缺失时不在消息写入路径触发索引。
func WithMemoryTrigger(trigger MemoryTrigger) SessionServiceOption {
	return func(s *SessionService) {
		s.memory = trigger
	}
}

// WithFactTrigger 注入事实提取触发端口；缺失时不在消息写入路径触发事实提取。
func WithFactTrigger(trigger FactTrigger) SessionServiceOption {
	return func(s *SessionService) {
		s.facts = trigger
	}
}

// WithSharedSessionDetector 注入共享会话检测器；飞书启用时阻止群聊话题开启私有记忆。
func WithSharedSessionDetector(detector SharedSessionDetector) SessionServiceOption {
	return func(s *SessionService) {
		s.shared = detector
	}
}

// SessionService 实现会话与消息用例，保证用户隔离与状态机约束。
type SessionService struct {
	repo    SessionRepository
	limits  QuotaLimits
	hot     HotMessageStore
	summary SummaryTrigger
	memory  MemoryTrigger
	facts   FactTrigger
	shared  SharedSessionDetector
}

// NewSessionService 构造会话服务，并应用用户级配额。
func NewSessionService(repo SessionRepository, limits QuotaLimits, options ...SessionServiceOption) (*SessionService, error) {
	if repo == nil {
		return nil, invalid("session repository is required")
	}
	service := &SessionService{repo: repo, limits: limits.Normalize()}
	for _, option := range options {
		option(service)
	}
	return service, nil
}

// checkQuota 校验用户会话总数是否达到上限。
func (s *SessionService) checkQuota(ctx context.Context, userID uuid.UUID) error {
	count, err := s.repo.CountSessions(ctx, userID)
	if err != nil {
		return err
	}
	if count >= int64(s.limits.MaxSessionsPerUser) {
		return ErrQuotaExceeded
	}
	return nil
}

// Create 创建会话；绑定 Agent 时必须为当前用户的已发布版本。
func (s *SessionService) Create(ctx context.Context, principal Principal, session Session) (Session, error) {
	if principal.UserID == uuid.Nil {
		return Session{}, ErrForbidden
	}
	if err := s.checkQuota(ctx, principal.UserID); err != nil {
		return Session{}, err
	}
	session.ID = uuid.New()
	session.UserID = principal.UserID
	session.Status = SessionActive
	session.Version = 1
	if session.Source == "" {
		session.Source = SessionSourceWeb
	}
	if session.MemoryPolicy == "" {
		// 默认使用私有记忆策略，群聊话题由飞书适配层显式改为 group。
		session.MemoryPolicy = SessionMemoryPrivate
	}
	if err := s.validateSession(session); err != nil {
		return Session{}, err
	}
	if session.AgentID != nil {
		agent, err := s.repo.GetAgent(ctx, principal.UserID, *session.AgentID)
		if err != nil {
			return Session{}, err
		}
		if agent.Status != AgentPublished {
			return Session{}, invalid("sessions require a published agent")
		}
	}
	return s.repo.CreateSession(ctx, session, mutationEvent(principal.UserID, "session.created", "session", session.ID))
}

// Get 读取当前用户的会话。
func (s *SessionService) Get(ctx context.Context, principal Principal, sessionID uuid.UUID) (Session, error) {
	if principal.UserID == uuid.Nil {
		return Session{}, ErrForbidden
	}
	return s.repo.GetSession(ctx, principal.UserID, sessionID)
}

// List 按游标分页列出当前用户的会话。
func (s *SessionService) List(ctx context.Context, principal Principal, limit int, cursor string) (Page[Session], error) {
	if principal.UserID == uuid.Nil {
		return Page[Session]{}, ErrForbidden
	}
	return s.repo.ListSessions(ctx, principal.UserID, limit, cursor)
}

// Archive 归档活跃会话；版本号递增用于并发冲突检测。
func (s *SessionService) Archive(ctx context.Context, principal Principal, sessionID uuid.UUID) (Session, error) {
	if principal.UserID == uuid.Nil {
		return Session{}, ErrForbidden
	}
	session, err := s.repo.GetSession(ctx, principal.UserID, sessionID)
	if err != nil {
		return Session{}, err
	}
	if session.Status != SessionActive {
		return Session{}, fmt.Errorf("%w: only active sessions can be archived", ErrResourceConflict)
	}
	session.Status = SessionArchived
	session.Version++
	archived, err := s.repo.ArchiveSession(ctx, session, mutationEvent(principal.UserID, "session.archived", "session", session.ID))
	if err != nil {
		return Session{}, err
	}
	// 归档后不再有活跃对话，清理热缓存避免继续占用 Redis 内存。
	if s.hot != nil {
		_ = s.hot.Invalidate(ctx, principal.UserID, session.ID)
	}
	return archived, nil
}

// SetMemoryPolicy 更新会话记忆策略；允许会话所有者关闭个人长期记忆注入。
func (s *SessionService) SetMemoryPolicy(ctx context.Context, principal Principal, sessionID uuid.UUID, policy SessionMemoryPolicy) (Session, error) {
	if principal.UserID == uuid.Nil {
		return Session{}, ErrForbidden
	}
	if !validSessionMemoryPolicy(policy) {
		return Session{}, invalid("unsupported session memory policy")
	}
	session, err := s.repo.GetSession(ctx, principal.UserID, sessionID)
	if err != nil {
		return Session{}, err
	}
	if session.Status != SessionActive {
		return Session{}, fmt.Errorf("%w: only active sessions can change memory policy", ErrResourceConflict)
	}
	// 群聊话题不得启用私有记忆，避免所有者把个人画像与长期记忆注入共享上下文。
	if policy == SessionMemoryPrivate && s.shared != nil {
		shared, err := s.shared.IsSharedSession(ctx, principal.UserID, sessionID)
		if err != nil {
			return Session{}, err
		}
		if shared {
			return Session{}, invalid("group sessions cannot enable private memory")
		}
	}
	if session.MemoryPolicy == policy {
		return session, nil
	}
	session.MemoryPolicy = policy
	session.Version++
	updated, err := s.repo.UpdateSessionMemoryPolicy(ctx, session, mutationEvent(principal.UserID, "session.memory_policy_updated", "session", session.ID))
	if err != nil {
		return Session{}, err
	}
	return updated, nil
}

// AppendMessage 追加用户消息：强制 user 角色、服务端生成 ID 与 Token 计数，仅活跃会话可写。
func (s *SessionService) AppendMessage(ctx context.Context, principal Principal, message Message) (Message, error) {
	if principal.UserID == uuid.Nil {
		return Message{}, ErrForbidden
	}
	session, err := s.repo.GetSession(ctx, principal.UserID, message.SessionID)
	if err != nil {
		return Message{}, err
	}
	if session.Status != SessionActive {
		return Message{}, fmt.Errorf("%w: cannot append messages to an inactive session", ErrResourceConflict)
	}
	message.ID = uuid.New()
	message.UserID = principal.UserID
	message.Role = MessageUser
	message.TokenCount = 0
	if strings.TrimSpace(message.Content) == "" || len(message.Content) > 4*1024*1024 {
		return Message{}, invalid("message content must contain between 1 and 4194304 bytes")
	}
	if len(message.ToolCalls) > 100 {
		return Message{}, invalid("message tool calls exceed the limit")
	}
	if err := validateJSONSize(message.ToolCalls, 256*1024); err != nil {
		return Message{}, err
	}
	if len(message.ExternalMessageID) > 256 || len(message.SpeakerOpenID) > 256 {
		return Message{}, invalid("external message identifiers are too long")
	}
	created, err := s.repo.AppendMessage(ctx, message, mutationEvent(principal.UserID, "message.created", "message", message.ID))
	if err != nil {
		return Message{}, err
	}
	// 热缓存是加速层：写入失败时清理缓存，让下次读取回退 PostgreSQL，不能返回陈旧上下文。
	if s.hot != nil {
		if err := s.hot.Append(ctx, principal.UserID, message.SessionID, created); err != nil {
			_ = s.hot.Invalidate(ctx, principal.UserID, message.SessionID)
		}
	}
	// 摘要触发是尽力而为的异步动作，失败不得影响消息写入；disabled 策略跳过摘要写入。
	if s.summary != nil && session.MemoryPolicy != SessionMemoryDisabled {
		_ = s.summary.MaybeEnqueue(context.WithoutCancel(ctx), principal.UserID, message.SessionID)
	}
	// 向量化与事实提取只属于私有记忆；群聊话题与禁用记忆的会话不得写入个人长期记忆。
	if session.MemoryPolicy == SessionMemoryPrivate {
		// 向量化触发同样异步执行，失败不影响消息写入，后台维护任务会补足缺失向量。
		if s.memory != nil {
			_ = s.memory.MaybeEnqueue(context.WithoutCancel(ctx), principal.UserID, message.SessionID)
		}
		// 事实提取触发同样异步执行，失败不影响消息写入。
		if s.facts != nil {
			_ = s.facts.MaybeEnqueue(context.WithoutCancel(ctx), principal.UserID, message.SessionID)
		}
	}
	return created, nil
}

// ListMessages 列出会话消息；先校验会话归属，再按游标分页。
func (s *SessionService) ListMessages(ctx context.Context, principal Principal, sessionID uuid.UUID, limit int, cursor string) (Page[Message], error) {
	if principal.UserID == uuid.Nil {
		return Page[Message]{}, ErrForbidden
	}
	if _, err := s.repo.GetSession(ctx, principal.UserID, sessionID); err != nil {
		return Page[Message]{}, err
	}
	return s.repo.ListMessages(ctx, principal.UserID, sessionID, limit, cursor)
}

// validateSession 校验会话身份、标题长度与来源渠道。
func (s *SessionService) validateSession(session Session) error {
	if session.ID == uuid.Nil || session.UserID == uuid.Nil {
		return invalid("session and user ids are required")
	}
	if len(session.Title) > 200 {
		return invalid("session title exceeds 200 bytes")
	}
	if session.Source != SessionSourceWeb && session.Source != SessionSourceFeishu && session.Source != SessionSourceAPI {
		return invalid("unsupported session source")
	}
	if !validSessionMemoryPolicy(session.MemoryPolicy) {
		return invalid("unsupported session memory policy")
	}
	return nil
}

// validSessionMemoryPolicy 校验记忆策略取值，避免写入未知策略导致记忆泄露。
func validSessionMemoryPolicy(policy SessionMemoryPolicy) bool {
	switch policy {
	case SessionMemoryPrivate, SessionMemoryGroup, SessionMemoryDisabled:
		return true
	default:
		return false
	}
}
