package biz

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Mark-Panda/harness-agent/pkg/feishu"
	"github.com/google/uuid"
)

// FeishuEventJobType 是飞书事件异步消费的任务类型。
const FeishuEventJobType = "feishu.event"

// 飞书事件回执状态。
const (
	// FeishuEventReceived 事件已持久化，等待异步消费。
	FeishuEventReceived = "received"
	// FeishuEventProcessing 事件正在被 Worker 处理。
	FeishuEventProcessing = "processing"
	// FeishuEventProcessed 事件已成功处理。
	FeishuEventProcessed = "processed"
	// FeishuEventFailed 事件处理失败，可在预算内重试。
	FeishuEventFailed = "failed"
	// FeishuEventDead 事件超过重试预算，进入死信等待人工重放。
	FeishuEventDead = "dead"
	// FeishuEventIgnored 事件无需处理（非目标类型、非 @ 消息或重复消息）。
	FeishuEventIgnored = "ignored"
)

// FeishuEventRecord 是飞书事件回执的领域表示。
type FeishuEventRecord struct {
	// EventID 飞书事件唯一 ID。
	EventID string
	// EventType 事件类型。
	EventType string
	// TenantKey 事件所属租户。
	TenantKey string
	// AppID 事件所属应用。
	AppID string
	// Payload 服务端保存的原始事件载荷，仅用于重放，禁止写入日志。
	Payload map[string]any
	// UserID 事件发言人映射的系统用户；解析身份后回填，供账号删除清理原始载荷。
	UserID *uuid.UUID
	// Status 回执状态。
	Status string
	// Attempts 已消费次数。
	Attempts int
	// LastError 脱敏后的最近错误。
	LastError string
}

// FeishuEventStore 定义事件回执持久化契约。
type FeishuEventStore interface {
	// RecordEvent 幂等写入事件回执，返回是否为新事件。
	RecordEvent(ctx context.Context, event FeishuEventRecord) (bool, error)
	// GetEvent 按事件 ID 读取回执。
	GetEvent(ctx context.Context, eventID string) (FeishuEventRecord, error)
	// MarkEventStatus 更新回执状态、次数与脱敏错误。
	MarkEventStatus(ctx context.Context, eventID, status, reason string, now time.Time) error
	// BindUser 回填事件发言人对应的系统用户，供账号删除清理原始载荷。
	BindUser(ctx context.Context, eventID string, userID uuid.UUID) error
}

// FeishuEventEnqueuer 是事件持久化后的异步入队端口。
type FeishuEventEnqueuer interface {
	// EnqueueFeishuEvent 幂等入队事件消费任务。
	EnqueueFeishuEvent(ctx context.Context, eventID string, availableAt time.Time) error
}

// FeishuIdentity 是解析后的飞书身份。
type FeishuIdentity struct {
	// UserID 内部用户 ID。
	UserID uuid.UUID
	// Created 本次是否新建了专用用户；新建用户不合并任何已有账户。
	Created bool
}

// FeishuIdentityStore 定义飞书 open_id 到内部用户的映射契约。
// 实现只能按 provider + tenant + open_id 精确匹配，禁止用昵称、邮箱等可伪造字段合并账户。
type FeishuIdentityStore interface {
	// Resolve 解析或为全新 open_id 创建专用用户；冲突时返回错误等待人工处理。
	Resolve(ctx context.Context, tenantKey, openID string) (FeishuIdentity, error)
}

// FeishuConversationBinding 是 tenant/chat/thread 到内部 Session 的绑定。
type FeishuConversationBinding struct {
	// ID 绑定唯一标识。
	ID uuid.UUID
	// UserID 会话所有者，群聊话题为首个触发用户。
	UserID uuid.UUID
	// Provider 外部渠道，固定 feishu。
	Provider string
	// TenantKey 飞书租户。
	TenantKey string
	// AppID 事件所属飞书应用，用于隔离同一群聊中的多个机器人。
	AppID string
	// ChatID 飞书会话 ID。
	ChatID string
	// ThreadID 话题维度键；单聊固定 p2p。
	ThreadID string
	// RootMessageID 无话题能力时的过渡别名。
	RootMessageID string
	// SessionID 映射的内部会话。
	SessionID uuid.UUID
	// Status 绑定状态：active/closed/archived。
	Status string
	// CreatedAt 创建时间。
	CreatedAt time.Time
}

// 飞书会话绑定状态。
const (
	// FeishuBindingActive 绑定活跃，可继续对话。
	FeishuBindingActive = "active"
	// FeishuBindingClosed 绑定已关闭，暂停自动回复，可通过恢复重新启用。
	FeishuBindingClosed = "closed"
	// FeishuBindingArchived 绑定已归档，只读，需显式恢复后才能继续对话。
	FeishuBindingArchived = "archived"
	// FeishuBindingUnbound 绑定已解绑，查找时跳过；同一话题的下一条 @ 消息会创建新会话。
	FeishuBindingUnbound = "unbound"
)

// SessionAgentBinder 是可选的会话 Agent 绑定能力。
// 飞书话题首次创建时可能尚未解析出绑定机器人；后续事件解析出 Agent 后补齐，
// 保证 Web 端打开飞书会话时也能继续对话。
type SessionAgentBinder interface {
	// BindSessionAgent 把活跃会话绑定到指定 Agent。
	BindSessionAgent(ctx context.Context, userID, sessionID, agentID uuid.UUID, now time.Time) error
}

// FeishuConversationAppScopedRepository 是按飞书应用隔离话题绑定的可选能力。
// 同一群聊可能同时安装多个机器人，绑定必须按 app_id 区分，避免不同机器人的
// 会话、上下文与回复互相串台。
type FeishuConversationAppScopedRepository interface {
	// FindConversationForApp 按 app/tenant/chat 查找绑定。
	FindConversationForApp(ctx context.Context, appID, tenantKey, chatID, threadID, rootMessageID string) (FeishuConversationBinding, bool, error)
}

// FeishuConversationRepository 定义飞书话题绑定契约。
type FeishuConversationRepository interface {
	// FindConversation 按 tenant/chat 查找绑定：优先匹配话题键，未命中时按根消息别名回退，
	// 保证主时间线 @ 机器人创建话题后，话题内追问仍复用同一 Session；已解绑记录不参与匹配。
	FindConversation(ctx context.Context, tenantKey, chatID, threadID, rootMessageID string) (FeishuConversationBinding, bool, error)
	// EnsureConversation 在同一事务创建 Session、所有者成员与绑定；唯一冲突时返回既有绑定。
	EnsureConversation(ctx context.Context, binding FeishuConversationBinding, session Session, event OutboxEvent) (FeishuConversationBinding, Session, error)
	// ReleaseConversation 关闭指定会话下活跃的飞书绑定，返回受影响绑定数；必须按 user_id 限定。
	ReleaseConversation(ctx context.Context, userID, sessionID uuid.UUID, status string, event OutboxEvent) (int, error)
	// RestoreConversation 将已关闭或已归档的绑定恢复为活跃，返回受影响绑定数；必须按 user_id 限定。
	RestoreConversation(ctx context.Context, userID, sessionID uuid.UUID, event OutboxEvent) (int, error)
	// PromoteConversationThread 把主时间线根消息别名升级为飞书真实话题 ID，返回受影响绑定数。
	PromoteConversationThread(ctx context.Context, userID, sessionID uuid.UUID, legacyThreadID, threadID string) (int, error)
}

// FeishuBindingService 提供飞书话题关闭、归档、恢复与解绑用例。
// 授权策略：仅会话所有者可操作，管理员角色同样不得跨用户读取或修改他人绑定；
// 数据隔离由仓储层的 user_id 条件强制，避免管理员越权访问个人会话。
type FeishuBindingService struct {
	conversations FeishuConversationRepository
	sessions      SessionRepository
}

// NewFeishuBindingService 构造飞书绑定服务。
func NewFeishuBindingService(conversations FeishuConversationRepository, sessions SessionRepository) (*FeishuBindingService, error) {
	if conversations == nil || sessions == nil {
		return nil, invalid("feishu binding service requires conversations and sessions")
	}
	return &FeishuBindingService{conversations: conversations, sessions: sessions}, nil
}

// Close 关闭指定会话下所有活跃飞书绑定，暂停自动回复；重复关闭幂等返回 0。
func (s *FeishuBindingService) Close(ctx context.Context, principal Principal, sessionID uuid.UUID) (int, error) {
	return s.release(ctx, principal, sessionID, FeishuBindingClosed, "feishu.binding_closed")
}

// Archive 归档指定会话下所有活跃飞书绑定，使其进入只读状态；重复归档幂等返回 0。
func (s *FeishuBindingService) Archive(ctx context.Context, principal Principal, sessionID uuid.UUID) (int, error) {
	return s.release(ctx, principal, sessionID, FeishuBindingArchived, "feishu.binding_archived")
}

// Unbind 解绑指定会话下所有活跃飞书绑定，返回受影响绑定数；重复解绑幂等返回 0。
// 解绑后同一话题的下一条 @ 消息会创建全新会话，避免旧上下文继续影响新对话。
func (s *FeishuBindingService) Unbind(ctx context.Context, principal Principal, sessionID uuid.UUID) (int, error) {
	return s.release(ctx, principal, sessionID, FeishuBindingUnbound, "feishu.binding_unbound")
}

// Restore 将已关闭或已归档的飞书绑定恢复为活跃，返回受影响绑定数；已活跃时幂等返回 0。
func (s *FeishuBindingService) Restore(ctx context.Context, principal Principal, sessionID uuid.UUID) (int, error) {
	if principal.UserID == uuid.Nil {
		return 0, ErrForbidden
	}
	if sessionID == uuid.Nil {
		return 0, invalid("session id is required")
	}
	// 先校验会话归属，确保非所有者即使猜到会话 ID 也得到 NotFound 而不是成功。
	if _, err := s.sessions.GetSession(ctx, principal.UserID, sessionID); err != nil {
		return 0, err
	}
	return s.conversations.RestoreConversation(ctx, principal.UserID, sessionID,
		mutationEvent(principal.UserID, "feishu.binding_restored", "session", sessionID))
}

// release 校验会话归属后按目标状态释放绑定，统一处理幂等与审计事件。
func (s *FeishuBindingService) release(ctx context.Context, principal Principal, sessionID uuid.UUID, status, eventType string) (int, error) {
	if principal.UserID == uuid.Nil {
		return 0, ErrForbidden
	}
	if sessionID == uuid.Nil {
		return 0, invalid("session id is required")
	}
	// 先校验会话归属，确保非所有者即使猜到会话 ID 也得到 NotFound 而不是成功。
	if _, err := s.sessions.GetSession(ctx, principal.UserID, sessionID); err != nil {
		return 0, err
	}
	return s.conversations.ReleaseConversation(ctx, principal.UserID, sessionID, status,
		mutationEvent(principal.UserID, eventType, "session", sessionID))
}

// FeishuInboundMessageStore 定义飞书入站消息幂等写入契约。
type FeishuInboundMessageStore interface {
	// AppendInbound 按 external_message_id 幂等追加用户消息，返回是否新写入。
	AppendInbound(ctx context.Context, message Message, event OutboxEvent) (Message, bool, error)
}

// FeishuOutbound 是外发回复记录。
type FeishuOutbound struct {
	// ID 外发记录主键。
	ID uuid.UUID
	// UserID 会话所有者用户。
	UserID uuid.UUID
	// SessionID 关联会话。
	SessionID uuid.UUID
	// RunID 关联 Run。
	RunID uuid.UUID
	// AppID 接收消息的飞书应用，回复 Worker 据此选择机器人客户端。
	AppID string
	// ChatID 目标飞书会话。
	ChatID string
	// ThreadID 目标话题。
	ThreadID string
	// ReplyToMessageID 触发回复的原始消息。
	ReplyToMessageID string
	// MessageID 已发送的飞书消息 ID。
	MessageID string
	// Kind 回复形态：card/text。
	Kind string
	// Status 投递状态。
	Status string
	// Attempts 已尝试次数。
	Attempts int
	// MaxAttempts 最大尝试次数。
	MaxAttempts int
	// DedupeKey 业务去重键。
	DedupeKey string
	// CardUpdatedAt 最近一次卡片成功更新的时间；零值表示尚未更新过卡片。
	CardUpdatedAt time.Time
	// CardFingerprint 最近一次成功写入卡片的标题与正文指纹，用于避免重复更新。
	CardFingerprint string
	// CreatedAt 创建时间。
	CreatedAt time.Time
}

// FeishuOutboundStore 定义外发回复的持久化与投递状态契约。
type FeishuOutboundStore interface {
	// CreateOutbound 幂等创建外发记录。
	CreateOutbound(ctx context.Context, outbound FeishuOutbound) error
	// ClaimOutbound 领取一个到期外发记录。
	ClaimOutbound(ctx context.Context, workerID string, lease time.Duration, now time.Time) (FeishuOutbound, bool, error)
	// UpdateOutboundMessageID 保存飞书返回的消息 ID。
	UpdateOutboundMessageID(ctx context.Context, id uuid.UUID, messageID string, now time.Time) error
	// MarkOutboundCardUpdated 记录卡片成功更新时间与内容指纹，用于跨副本限制更新频率并跳过重复内容。
	MarkOutboundCardUpdated(ctx context.Context, id uuid.UUID, fingerprint string, now time.Time) error
	// DeferOutbound 在等待 Run 完成时延后再次处理，不消耗失败预算。
	DeferOutbound(ctx context.Context, id uuid.UUID, retryAt, now time.Time) error
	// CompleteOutbound 标记外发成功。
	CompleteOutbound(ctx context.Context, id uuid.UUID, now time.Time) error
	// FailOutbound 记录失败并在预算内重试，超出预算进入死信。
	FailOutbound(ctx context.Context, id uuid.UUID, reason string, retryAt, now time.Time) error
	// LatestAssistantMessage 读取指定 Run 产生的助手消息，用于生成最终回复；runID 为空时退回会话最近一条。
	LatestAssistantMessage(ctx context.Context, userID, sessionID uuid.UUID, runID *uuid.UUID) (Message, error)
}

// FeishuThreadedMessage 复用飞书客户端的话题回复结果，避免在 Biz 层重复定义结构。
// MessageID 用于后续卡片更新，ThreadID 是飞书在主时间线创建话题后返回的真实话题 ID。
type FeishuThreadedMessage = feishu.ThreadReply

// FeishuReplySender 是外发回复的端口，生产实现调用飞书开放平台。
type FeishuReplySender interface {
	// ReplyCard 以可更新卡片回复指定消息。
	ReplyCard(ctx context.Context, replyTo string, card any) (string, error)
	// ReplyCardInThread 在指定消息下创建或复用话题并回复卡片，用于主时间线 @ 消息建立话题。
	ReplyCardInThread(ctx context.Context, replyTo string, card any) (FeishuThreadedMessage, error)
	// UpdateCard 更新已发送卡片。
	UpdateCard(ctx context.Context, messageID string, card any) error
	// ReplyText 以纯文本回复指定消息。
	ReplyText(ctx context.Context, replyTo, text string) (string, error)
	// ReplyTextInThread 在话题内回复纯文本，作为卡片降级方案。
	ReplyTextInThread(ctx context.Context, replyTo, text string) (FeishuThreadedMessage, error)
}

// FeishuIngestConfig 是事件接入的安全参数。
type FeishuIngestConfig struct {
	// MaxBodyBytes 允许的最大请求体字节数。
	MaxBodyBytes int64
}

// FeishuIngestService 负责验签、解密、幂等入库与快速 ACK。
type FeishuIngestService struct {
	verifier     *feishu.Verifier
	events       FeishuEventStore
	enqueuer     FeishuEventEnqueuer
	maxBodyBytes int64
	now          func() time.Time
	// botCredentials 与 opener 用于按机器人 ID 动态构造验签器；为空时只支持默认应用回调。
	botCredentials FeishuBotCredentialReader
	opener         CredentialOpener
}

// FeishuIngestResult 是回调处理结果。
type FeishuIngestResult struct {
	// Challenge URL 验证需要原样返回的挑战串。
	Challenge string
	// Accepted 事件已持久化或已忽略，可返回 200 快速 ACK。
	Accepted bool
}

// NewFeishuIngestService 构造事件接入服务。
func NewFeishuIngestService(verifier *feishu.Verifier, events FeishuEventStore, enqueuer FeishuEventEnqueuer, config FeishuIngestConfig) (*FeishuIngestService, error) {
	if verifier == nil || events == nil || enqueuer == nil {
		return nil, invalid("feishu ingest requires verifier, event store and enqueuer")
	}
	if config.MaxBodyBytes <= 0 {
		config.MaxBodyBytes = 1 << 20
	}
	return &FeishuIngestService{
		verifier: verifier, events: events, enqueuer: enqueuer,
		maxBodyBytes: config.MaxBodyBytes, now: func() time.Time { return time.Now().UTC() },
	}, nil
}

// MaxBodyBytes 返回允许的最大回调请求体字节数，供 HTTP 适配层限制读取。
func (s *FeishuIngestService) MaxBodyBytes() int64 {
	return s.maxBodyBytes
}

// WithBotVerification 启用按机器人 ID 读取独立应用凭据的验签能力。
func (s *FeishuIngestService) WithBotVerification(credentials FeishuBotCredentialReader, opener CredentialOpener) *FeishuIngestService {
	s.botCredentials = credentials
	s.opener = opener
	return s
}

// Handle 校验并持久化事件，成功后仅入队异步任务，不在请求内调用模型。
func (s *FeishuIngestService) Handle(ctx context.Context, request feishu.Request) (FeishuIngestResult, error) {
	return s.handleWithVerifier(ctx, s.verifier, request)
}

// HandleForBot 使用管理员登记的机器人应用凭据验签并处理回调。
func (s *FeishuIngestService) HandleForBot(ctx context.Context, botID uuid.UUID, request feishu.Request) (FeishuIngestResult, error) {
	if botID == uuid.Nil {
		return FeishuIngestResult{}, invalid("feishu bot id is required")
	}
	if s.botCredentials == nil || s.opener == nil {
		return FeishuIngestResult{}, fmt.Errorf("%w: feishu bot verification is not configured", ErrInvalidConfig)
	}
	bot, credential, err := s.botCredentials.GetFeishuBotCredential(ctx, botID)
	if err != nil {
		return FeishuIngestResult{}, err
	}
	if bot.Status != FeishuBotEnabled {
		return FeishuIngestResult{}, fmt.Errorf("%w: feishu bot is disabled", ErrResourceNotFound)
	}
	plaintext, err := s.opener.Open(credential.KeyVersion, credential.Encrypted)
	if err != nil {
		return FeishuIngestResult{}, err
	}
	var secret FeishuBotSecret
	if err := json.Unmarshal(plaintext, &secret); err != nil {
		return FeishuIngestResult{}, fmt.Errorf("%w: feishu bot credentials are invalid", ErrInvalidConfig)
	}
	if strings.TrimSpace(secret.AppSecret) == "" {
		return FeishuIngestResult{}, invalid("feishu bot app secret is missing")
	}
	verifierConfig := feishu.VerifierConfig{VerificationToken: secret.VerificationToken}
	if key := strings.TrimSpace(secret.EncryptKey); key != "" {
		verifierConfig.EncryptKeys = []feishu.EncryptionKey{{ID: botID.String(), Secret: key}}
	}
	verifier, err := feishu.NewVerifier(verifierConfig)
	if err != nil {
		return FeishuIngestResult{}, fmt.Errorf("%w: feishu bot verifier is invalid", ErrInvalidConfig)
	}
	return s.handleWithVerifier(ctx, verifier, request)
}

// AcceptTrusted 接收已由飞书长连接认证的事件载荷；连接本身由应用凭据建立，因此跳过 HTTP 签名与 Token 校验。
func (s *FeishuIngestService) AcceptTrusted(ctx context.Context, botID uuid.UUID, appID string, payload []byte) (FeishuIngestResult, error) {
	if botID == uuid.Nil {
		return FeishuIngestResult{}, invalid("feishu bot id is required")
	}
	if len(payload) == 0 {
		return FeishuIngestResult{}, invalid("feishu event body is empty")
	}
	if int64(len(payload)) > s.maxBodyBytes {
		return FeishuIngestResult{}, invalid("feishu event body exceeds the limit")
	}
	envelope, err := feishu.DecodeEnvelope(payload)
	if err != nil {
		return FeishuIngestResult{}, err
	}
	// 长连接按应用建立，事件头中的 app_id 必须与连接使用的应用一致，防止事件串应用。
	if expected := strings.TrimSpace(appID); expected != "" && strings.TrimSpace(envelope.Header.AppID) != "" && envelope.Header.AppID != expected {
		return FeishuIngestResult{}, invalid("feishu event app id mismatch")
	}
	return s.persistEnvelope(ctx, envelope, payload)
}

// persistEnvelope 完成事件格式校验、幂等入库与异步任务入队。
func (s *FeishuIngestService) persistEnvelope(ctx context.Context, envelope feishu.Envelope, plaintext []byte) (FeishuIngestResult, error) {
	if envelope.Header.EventID == "" || envelope.Header.EventType == "" {
		return FeishuIngestResult{}, invalid("feishu event header is incomplete")
	}
	record := FeishuEventRecord{
		EventID: envelope.Header.EventID, EventType: envelope.Header.EventType,
		TenantKey: envelope.Header.TenantKey, AppID: envelope.Header.AppID,
		// 原始事件只保存在受数据库权限保护的内部回执中，日志与错误只使用事件 ID。
		Payload: map[string]any{"body": string(plaintext)},
		Status:  FeishuEventReceived,
	}
	inserted, err := s.events.RecordEvent(ctx, record)
	if err != nil {
		return FeishuIngestResult{}, err
	}
	// 即便事件已存在也重新入队，修复上次入库成功但入队失败导致的丢任务；
	// Job 去重键保证同一事件只会有一个消费任务。
	if err := s.enqueuer.EnqueueFeishuEvent(ctx, envelope.Header.EventID, s.now()); err != nil {
		_ = s.events.MarkEventStatus(ctx, envelope.Header.EventID, FeishuEventFailed, "enqueue feishu event failed", s.now())
		return FeishuIngestResult{}, err
	}
	_ = inserted
	return FeishuIngestResult{Accepted: true}, nil
}

// handle 执行验签、解密、幂等入库与任务入队。
func (s *FeishuIngestService) handleWithVerifier(ctx context.Context, verifier *feishu.Verifier, request feishu.Request) (FeishuIngestResult, error) {
	if verifier == nil {
		return FeishuIngestResult{}, fmt.Errorf("%w: feishu verifier is not configured", ErrInvalidConfig)
	}
	return s.handle(ctx, verifier, request)
}

// handle 使用指定验签器处理请求。
func (s *FeishuIngestService) handle(ctx context.Context, verifier *feishu.Verifier, request feishu.Request) (FeishuIngestResult, error) {
	if len(request.Body) == 0 {
		return FeishuIngestResult{}, invalid("feishu event body is empty")
	}
	if int64(len(request.Body)) > s.maxBodyBytes {
		return FeishuIngestResult{}, invalid("feishu event body exceeds the limit")
	}
	plaintext, err := verifier.VerifyAndDecrypt(request)
	if err != nil {
		return FeishuIngestResult{}, err
	}
	envelope, err := feishu.DecodeEnvelope(plaintext)
	if err != nil {
		return FeishuIngestResult{}, err
	}
	// URL 验证请求必须在验签后校验 Verification Token，再原样返回 challenge。
	if envelope.IsURLVerification() {
		if err := verifier.VerifyToken(envelope.Token); err != nil {
			return FeishuIngestResult{}, err
		}
		if strings.TrimSpace(envelope.Challenge) == "" {
			return FeishuIngestResult{}, invalid("feishu url verification challenge is empty")
		}
		return FeishuIngestResult{Challenge: envelope.Challenge, Accepted: true}, nil
	}
	if err := verifier.VerifyToken(envelope.Header.Token); err != nil {
		return FeishuIngestResult{}, err
	}
	return s.persistEnvelope(ctx, envelope, plaintext)
}

// feishuErrorCode 返回可安全写入回执与日志的稳定错误分类，不包含事件正文。
func feishuErrorCode(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("feishu processing failed: %s", truncateError(err.Error(), 512))
}

// truncateError 截断错误文本，避免把上游长响应写入数据库。
func truncateError(text string, limit int) string {
	return truncateUTF8Bytes(text, limit)
}
