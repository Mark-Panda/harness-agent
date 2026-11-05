package biz

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Mark-Panda/harness-agent/pkg/feishu"
	"github.com/Mark-Panda/harness-agent/pkg/logging"
	"github.com/google/uuid"
)

// FeishuRunCreator 是事件 Worker 创建 Run 的端口，生产实现为 RunService。
type FeishuRunCreator interface {
	// Create 按会话所有者身份创建 Run。
	Create(ctx context.Context, principal Principal, input CreateRunInput) (Run, error)
}

// FeishuEventWorkerConfig 是事件 Worker 的轮询、租约与业务策略配置。
type FeishuEventWorkerConfig struct {
	// WorkerID 实例标识，写入任务租约。
	WorkerID string
	// Concurrency 单实例并发消费数。
	Concurrency int
	// PollInterval 无任务时的轮询间隔。
	PollInterval time.Duration
	// Lease 任务租约时长。
	Lease time.Duration
	// DefaultAgentID 飞书事件默认使用的已发布 Agent。
	DefaultAgentID *uuid.UUID
	// DefaultAgentVersion 默认 Agent 版本。
	DefaultAgentVersion *int
	// Mode 默认编排模式。
	Mode AgentMode
	// BotOpenID 机器人 open_id，用于精确判断是否被 @。
	BotOpenID string
	// BotName 机器人显示名，open_id 未配置时用于 @ 判断。
	BotName string
	// RequireTopic 群聊必须按话题建会话。
	RequireTopic bool
	// AllowRootReplyFallback 群聊无话题能力时允许按根消息创建过渡会话。
	AllowRootReplyFallback bool
	// MaxMessageBytes 单条消息正文上限。
	MaxMessageBytes int
}

// normalize 补齐事件 Worker 默认配置。
func (c FeishuEventWorkerConfig) normalize() FeishuEventWorkerConfig {
	if c.Concurrency <= 0 {
		c.Concurrency = 1
	}
	if c.PollInterval <= 0 {
		c.PollInterval = time.Second
	}
	if c.Lease <= 0 {
		c.Lease = 2 * time.Minute
	}
	if c.Mode == "" {
		c.Mode = AgentModeReact
	}
	if c.MaxMessageBytes <= 0 {
		c.MaxMessageBytes = 64 * 1024
	}
	return c
}

// FeishuEventWorkerDependencies 聚合事件 Worker 所需依赖。
type FeishuEventWorkerDependencies struct {
	// Jobs 持久化任务队列。
	Jobs JobQueue
	// Events 事件回执仓储。
	Events FeishuEventStore
	// Identities 飞书身份映射。
	Identities FeishuIdentityStore
	// Conversations 话题绑定仓储。
	Conversations FeishuConversationRepository
	// Sessions 会话读取端口，用于校验归属与状态。
	Sessions SessionRepository
	// Messages 入站消息幂等写入端口。
	Messages FeishuInboundMessageStore
	// Runs Run 创建端口。
	Runs FeishuRunCreator
	// Outbound 外发回复仓储。
	Outbound FeishuOutboundStore
	// HotMessages 会话热消息缓存；入站消息落库后清理，确保下一次 Run 能读到新消息。
	HotMessages HotMessageStore
	// Bots 机器人注册与绑定解析端口；为空时退化为配置中的默认机器人。
	Bots FeishuBotRouter
	// SpeakerFacts 群聊发言人事实提取触发器；为空表示不提取个人事实。
	SpeakerFacts FactTrigger
	// Logger 结构化日志。
	Logger *slog.Logger
	// Clock 当前时间函数。
	Clock func() time.Time
}

// FeishuEventWorker 认领事件任务并完成身份解析、话题绑定、消息入库与 Run 创建。
type FeishuEventWorker struct {
	jobs          JobQueue
	events        FeishuEventStore
	identities    FeishuIdentityStore
	conversations FeishuConversationRepository
	sessions      SessionRepository
	messages      FeishuInboundMessageStore
	runs          FeishuRunCreator
	outbound      FeishuOutboundStore
	hotMessages   HotMessageStore
	bots          FeishuBotRouter
	config        FeishuEventWorkerConfig
	clock         func() time.Time
	logger        *slog.Logger
	speakerFacts  FactTrigger
}

// NewFeishuEventWorker 构造事件 Worker；缺少关键依赖时返回错误。
func NewFeishuEventWorker(dependencies FeishuEventWorkerDependencies, config FeishuEventWorkerConfig) (*FeishuEventWorker, error) {
	if dependencies.Jobs == nil || dependencies.Events == nil || dependencies.Identities == nil ||
		dependencies.Conversations == nil || dependencies.Sessions == nil || dependencies.Messages == nil ||
		dependencies.Runs == nil || dependencies.Outbound == nil {
		return nil, invalid("feishu event worker requires jobs, events, identities, conversations, sessions, messages, runs and outbound")
	}
	if strings.TrimSpace(config.WorkerID) == "" {
		return nil, invalid("feishu event worker id is required")
	}
	logger := dependencies.Logger
	if logger == nil {
		logger = slog.Default()
	}
	clock := dependencies.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &FeishuEventWorker{
		jobs: dependencies.Jobs, events: dependencies.Events, identities: dependencies.Identities,
		conversations: dependencies.Conversations, sessions: dependencies.Sessions,
		messages: dependencies.Messages, runs: dependencies.Runs, outbound: dependencies.Outbound,
		hotMessages: dependencies.HotMessages,
		bots:        dependencies.Bots,
		config:      config.normalize(), clock: clock, logger: logger, speakerFacts: dependencies.SpeakerFacts,
	}, nil
}

// Start 启动配置数量的消费协程并在上下文取消后返回。
func (w *FeishuEventWorker) Start(ctx context.Context) {
	var workers sync.WaitGroup
	for index := 0; index < w.config.Concurrency; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			w.loop(ctx)
		}()
	}
	workers.Wait()
}

// loop 持续认领事件任务；无任务时按轮询间隔等待。
func (w *FeishuEventWorker) loop(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if _, err := w.ProcessOne(ctx); err != nil && ctx.Err() == nil {
			w.logger.Warn("claim feishu event job failed", "error", err)
		}
		timer.Reset(w.config.PollInterval)
	}
}

// ProcessOne 认领并处理至多一个飞书事件任务。
func (w *FeishuEventWorker) ProcessOne(ctx context.Context) (bool, error) {
	job, claimed, err := w.jobs.Claim(ctx, w.config.WorkerID, FeishuEventJobType, w.config.Lease, w.clock())
	if err != nil {
		return false, err
	}
	if !claimed {
		return false, nil
	}
	w.handle(ctx, job)
	return true, nil
}

// handle 解析任务并处理事件；业务忽略直接完成，可重试错误进入退避或死信。
func (w *FeishuEventWorker) handle(ctx context.Context, job Job) {
	finishJob := beginJob()
	jobOutcome := "success"
	defer func() { finishJob(jobOutcome) }()
	eventID, err := decodeFeishuEventJob(job)
	if err != nil {
		w.logger.Error("invalid feishu event job payload", "job_id", job.ID)
		jobOutcome = "failure"
		_ = w.jobs.Complete(context.WithoutCancel(ctx), job.ID, w.config.WorkerID, job.FencingToken)
		return
	}
	record, err := w.events.GetEvent(ctx, eventID)
	if err != nil {
		jobOutcome = "failure"
		w.failJob(ctx, job, eventID, err)
		return
	}
	if record.Status == FeishuEventProcessed || record.Status == FeishuEventIgnored || record.Status == FeishuEventDead {
		_ = w.jobs.Complete(context.WithoutCancel(ctx), job.ID, w.config.WorkerID, job.FencingToken)
		return
	}
	_ = w.events.MarkEventStatus(ctx, eventID, FeishuEventProcessing, "", w.clock())

	outcome, err := w.process(ctx, record)
	if err != nil {
		jobOutcome = "failure"
		w.failJob(ctx, job, eventID, err)
		return
	}
	if outcome != "" {
		_ = w.events.MarkEventStatus(context.WithoutCancel(ctx), eventID, outcome, "", w.clock())
	}
	_ = w.jobs.Complete(context.WithoutCancel(ctx), job.ID, w.config.WorkerID, job.FencingToken)
}

// process 返回 processed/ignored 表示终态；返回错误表示可重试失败。
func (w *FeishuEventWorker) process(ctx context.Context, record FeishuEventRecord) (string, error) {
	body, _ := record.Payload["body"].(string)
	if body == "" {
		return FeishuEventIgnored, nil
	}
	envelope, err := feishu.DecodeEnvelope([]byte(body))
	if err != nil {
		return "", err
	}
	switch envelope.Header.EventType {
	case feishu.EventTypeMessageRecalled, feishu.EventTypeMessageUpdated:
		// 撤回与编辑不作为新的对话输入，避免重复触发 Run 或产生乱序上下文。
		return FeishuEventIgnored, nil
	case feishu.EventTypeMessageReceive:
	default:
		return FeishuEventIgnored, nil
	}
	messageEvent, err := feishu.ParseMessageEvent(envelope.Event)
	if err != nil {
		return "", err
	}
	if messageEvent.SenderType != "" && messageEvent.SenderType != "user" {
		return FeishuEventIgnored, nil
	}
	if messageEvent.SenderOpenID == "" {
		return FeishuEventIgnored, nil
	}
	// 已登记的机器人按应用 App ID 解析；未登记时保持原有默认应用行为。
	botOpenID := w.config.BotOpenID
	botName := w.config.BotName
	var routedBot *FeishuBot
	if w.bots != nil && record.AppID != "" {
		bot, found, err := w.bots.LookupFeishuBot(ctx, record.AppID)
		if err != nil {
			return "", err
		}
		if found {
			if bot.Status != FeishuBotEnabled {
				return FeishuEventIgnored, nil
			}
			routedBot = &bot
			botOpenID = bot.BotOpenID
			botName = bot.Name
		}
	}
	// 群聊只处理明确 @ 机器人的消息；单聊始终响应。
	if messageEvent.ChatType == feishu.ChatTypeGroup && !messageEvent.Triggered(botOpenID, botName) {
		return FeishuEventIgnored, nil
	}
	text := messageEvent.Text()
	if text == "" {
		return FeishuEventIgnored, nil
	}
	if len(text) > w.config.MaxMessageBytes {
		text = text[:w.config.MaxMessageBytes]
	}
	threadID, rootMessageID, ignored := w.threadKey(messageEvent)
	if ignored {
		return FeishuEventIgnored, nil
	}
	identity, err := w.identities.Resolve(ctx, record.TenantKey, messageEvent.SenderOpenID)
	if err != nil {
		return "", err
	}
	// 回填事件归属用户；账号删除据此清理可能包含正文的原始事件载荷。
	if err := w.events.BindUser(ctx, record.EventID, identity.UserID); err != nil {
		return "", err
	}
	// 机器人绑定只服务助手所属用户本人；其他飞书用户的消息直接忽略。
	var routeAgentID *uuid.UUID
	var routeAgentVersion *int
	routeMode := w.config.Mode
	if routedBot != nil {
		binding, found, err := w.bots.FindFeishuBotBinding(ctx, routedBot.ID)
		if err != nil {
			return "", err
		}
		if !found {
			return FeishuEventIgnored, nil
		}
		if binding.UserID != identity.UserID {
			return FeishuEventIgnored, nil
		}
		// 助手取消发布或归档后绑定会被解除；此处兜底避免旧绑定继续执行。
		if binding.AgentStatus != AgentPublished {
			return FeishuEventIgnored, nil
		}
		agentID, agentVersion := binding.AgentID, binding.AgentVersion
		routeAgentID, routeAgentVersion = &agentID, &agentVersion
		routeMode = binding.AgentMode
	}
	binding, found, err := w.conversations.FindConversation(ctx, record.TenantKey, messageEvent.ChatID, threadID, rootMessageID)
	if err != nil {
		return "", err
	}
	if !found {
		binding, err = w.createConversation(ctx, identity.UserID, record.TenantKey, messageEvent, threadID, rootMessageID)
		if err != nil {
			return "", err
		}
	}
	// closed/archived 绑定不再触发自动回复，需通过显式恢复接口重新启用；
	// unbound 绑定不会出现在查询结果中，同一话题的下一轮 @ 会创建全新 Session。
	if binding.Status != FeishuBindingActive {
		return FeishuEventIgnored, nil
	}
	session, err := w.sessions.GetSession(ctx, binding.UserID, binding.SessionID)
	if err != nil {
		return "", err
	}
	if session.Status != SessionActive {
		return FeishuEventIgnored, nil
	}
	// 乱序保护：消息时间取飞书 create_time，而不是任务实际处理时间；
	// 这样旧事件晚到时仍按发生时间落库，上下文与摘要顺序不会被投递顺序打乱。
	occurredAt := messageEvent.CreatedAt(w.clock())
	inbound := Message{
		ID: uuid.New(), SessionID: session.ID, UserID: binding.UserID, Role: MessageUser,
		Content: text, ExternalMessageID: messageEvent.MessageID, SpeakerOpenID: messageEvent.SenderOpenID,
		CreatedAt: occurredAt,
	}
	_, created, err := w.messages.AppendInbound(ctx, inbound, mutationEvent(binding.UserID, "message.created", "message", uuid.New()))
	if err != nil {
		return "", err
	}
	if !created {
		// 飞书重推同一消息时，消息已存在即视为处理成功，不重复创建 Run。
		return FeishuEventProcessed, nil
	}
	// 入站消息只写 PostgreSQL；必须清理 Redis 热窗口，否则下一次 Run 会命中旧缓存而看不到本轮问题。
	// 冷缓存或 Redis 重启后，清理也能让下一次 Run 从数据库重建完整历史。
	if w.hotMessages != nil {
		if err := w.hotMessages.Invalidate(ctx, binding.UserID, session.ID); err != nil {
			w.logger.Warn("invalidate hot messages after inbound failed", "session_id", session.ID, "error", err)
		}
	}
	// 群聊话题中个人事实只写入发言人私有记忆：消息归属会话所有者，但提取与写入身份切换为发言人本人。
	// 失败不影响回复链路，仅记录日志等待下一轮重试。
	if messageEvent.ChatType == feishu.ChatTypeGroup && w.speakerFacts != nil {
		if err := w.speakerFacts.MaybeEnqueueSpeaker(ctx, binding.UserID, session.ID, identity.UserID, messageEvent.SenderOpenID); err != nil {
			w.logger.Warn("enqueue speaker fact extraction failed", "session_id", session.ID, "speaker_open_id", messageEvent.SenderOpenID, "error", err)
		}
	}
	if routedBot == nil {
		if w.config.DefaultAgentID == nil || w.config.DefaultAgentVersion == nil {
			// 未登记机器人且未配置默认助手时直接忽略，避免事件无限重试进入死信。
			return FeishuEventIgnored, nil
		}
		routeAgentID, routeAgentVersion = w.config.DefaultAgentID, w.config.DefaultAgentVersion
	}
	if routeAgentID == nil || routeAgentVersion == nil {
		return FeishuEventIgnored, nil
	}
	principal, err := NewPrincipal(binding.UserID, RoleUser)
	if err != nil {
		return "", err
	}
	run, err := w.runs.Create(ctx, principal, CreateRunInput{
		SessionID: session.ID, AgentID: routeAgentID, AgentVersion: routeAgentVersion,
		Mode: routeMode, IdempotencyKey: "feishu:" + messageEvent.MessageID,
	})
	if err != nil {
		return "", err
	}
	ctx = logging.WithRunID(ctx, run.ID.String())
	// 先创建外发记录占位，Run 完成后由回复 Worker 增量更新卡片。
	err = w.outbound.CreateOutbound(ctx, FeishuOutbound{
		ID: uuid.New(), UserID: binding.UserID, SessionID: session.ID, RunID: run.ID, AppID: record.AppID,
		ChatID: messageEvent.ChatID, ThreadID: threadID, ReplyToMessageID: messageEvent.MessageID,
		Kind: "card", Status: "pending", DedupeKey: "feishu.run:" + run.ID.String(),
	})
	if err != nil {
		return "", err
	}
	return FeishuEventProcessed, nil
}

// threadKey 计算话题键与根消息别名；单聊固定 p2p，群聊无话题时按策略忽略或使用根消息过渡别名。
// 根消息别名同时用于话题创建后的追问回退查找，保证主时间线 @ 与话题内追问复用同一 Session。
func (w *FeishuEventWorker) threadKey(event feishu.MessageEvent) (string, string, bool) {
	if event.ChatType != feishu.ChatTypeGroup {
		return "p2p", "", false
	}
	if key := event.ThreadKey(); key != "" {
		rootAlias := event.RootID
		if event.ThreadID == "" && event.RootID == "" {
			// parent_id 只能作为过渡别名，仍需记录 root_message_id 便于后续合并。
			rootAlias = event.ParentID
		}
		return key, rootAlias, false
	}
	if !w.config.AllowRootReplyFallback {
		// 禁止回退为群级共享 Session；没有话题键时直接忽略并等待用户在新话题中 @ 机器人。
		return "", "", true
	}
	// 主时间线 @ 机器人：先以根消息 ID 作为过渡话题键建会话，首条卡片回复再由飞书创建真实话题。
	return event.MessageID, event.MessageID, false
}

// createConversation 在同一事务创建 Session、所有者成员与话题绑定。
func (w *FeishuEventWorker) createConversation(ctx context.Context, userID uuid.UUID, tenantKey string, event feishu.MessageEvent, threadID, rootMessageID string) (FeishuConversationBinding, error) {
	sessionID := uuid.New()
	// 群聊话题禁止注入个人长期记忆与画像，单聊保持私有记忆策略。
	memoryPolicy := SessionMemoryPrivate
	if event.ChatType == feishu.ChatTypeGroup {
		memoryPolicy = SessionMemoryGroup
	}
	session := Session{
		ID: sessionID, UserID: userID, Title: feishuTitle(event.ChatID), Source: SessionSourceFeishu,
		MemoryPolicy: memoryPolicy, Status: SessionActive, Version: 1,
	}
	binding := FeishuConversationBinding{
		ID: uuid.New(), UserID: userID, Provider: "feishu", TenantKey: tenantKey,
		ChatID: event.ChatID, ThreadID: threadID, RootMessageID: rootMessageID,
		SessionID: sessionID, Status: FeishuBindingActive,
	}
	created, createdSession, err := w.conversations.EnsureConversation(ctx, binding, session,
		mutationEvent(userID, "session.created", "session", sessionID))
	if err != nil {
		return FeishuConversationBinding{}, err
	}
	_ = createdSession
	return created, nil
}

// failJob 记录回执失败并让 Job 退避重试或进入死信。
func (w *FeishuEventWorker) failJob(ctx context.Context, job Job, eventID string, err error) {
	reason := feishuErrorCode(err)
	w.logger.Warn("feishu event processing failed", "job_id", job.ID, "event_id", eventID, "error", reason)
	status := FeishuEventFailed
	if job.Attempts >= job.MaxAttempts {
		status = FeishuEventDead
	}
	_ = w.events.MarkEventStatus(context.WithoutCancel(ctx), eventID, status, reason, w.clock())
	_ = w.jobs.Fail(context.WithoutCancel(ctx), job.ID, w.config.WorkerID, job.FencingToken,
		w.clock().Add(feishuBackoff(job.Attempts)), reason, w.clock())
}

// feishuBackoff 计算指数退避时间，最大 10 分钟。
func feishuBackoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	delay := time.Duration(1<<min(attempts, 9)) * time.Second
	if delay > 10*time.Minute {
		delay = 10 * time.Minute
	}
	return delay
}

// decodeFeishuEventJob 从任务载荷读取事件 ID。
func decodeFeishuEventJob(job Job) (string, error) {
	value, _ := job.Payload["event_id"].(string)
	if value == "" {
		return "", invalid("feishu event job is missing event id")
	}
	return value, nil
}

// feishuTitle 生成会话标题，避免把消息正文写入标题。
func feishuTitle(chatID string) string {
	if len(chatID) > 60 {
		chatID = chatID[:60]
	}
	return "飞书会话 " + chatID
}
