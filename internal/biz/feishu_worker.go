package biz

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
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

type feishuGroupRunCreator interface {
	CreateFeishuGroup(ctx context.Context, principal Principal, policyID uuid.UUID, input CreateRunInput) (Run, error)
}

// FeishuSharedChat 显式授权某个飞书应用在指定租户和群中为群成员执行模型对话。
type FeishuSharedChat struct {
	AppID, TenantKey, ChatID string
	PolicyID                 uuid.UUID
}

type feishuChatScope struct{ appID, tenantKey, chatID string }

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
	// DefaultAgentOwnerID 默认助手所属用户，只有该用户可以使用默认助手的执行配置。
	DefaultAgentOwnerID *uuid.UUID
	// DefaultAppID 将默认应用的群聊授权绑定到真实 App ID。
	DefaultAppID string
	// SharedChats 是管理员授权的共享群聊范围。
	SharedChats []FeishuSharedChat
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
	Messages      FeishuInboundMessageStore
	GroupMessages FeishuGroupMessageStore
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
	groupMessages FeishuGroupMessageStore
	groupRuns     feishuGroupRunCreator
	sharedChats   map[feishuChatScope]uuid.UUID
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
	groupRuns, groupRunsAvailable := dependencies.Runs.(feishuGroupRunCreator)
	if len(config.SharedChats) > 0 {
		_, appScoped := dependencies.Conversations.(FeishuConversationAppScopedRepository)
		if dependencies.GroupMessages == nil || !groupRunsAvailable || !appScoped {
			return nil, invalid("shared feishu chats require group projection, group runs and app-scoped conversations")
		}
	}
	sharedChats := make(map[feishuChatScope]uuid.UUID, len(config.SharedChats))
	for _, chat := range config.SharedChats {
		scope := feishuChatScope{strings.TrimSpace(chat.AppID), strings.TrimSpace(chat.TenantKey), strings.TrimSpace(chat.ChatID)}
		if scope.appID == "" || scope.tenantKey == "" || scope.chatID == "" || chat.PolicyID == uuid.Nil {
			return nil, invalid("shared feishu chats require app, tenant, chat and policy IDs")
		}
		if _, exists := sharedChats[scope]; exists {
			return nil, invalid("duplicate shared feishu chat")
		}
		sharedChats[scope] = chat.PolicyID
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
		groupMessages: dependencies.GroupMessages, groupRuns: groupRuns, sharedChats: sharedChats,
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
		if _, err := func() (bool, error) {
			// 单次迭代 panic 不得终止消费协程：记录堆栈后继续处理下一个任务。
			defer func() {
				if recovered := recover(); recovered != nil {
					w.logger.Error("feishu event worker iteration panicked",
						"panic", fmt.Sprint(recovered), "stack", string(debug.Stack()))
				}
			}()
			return w.ProcessOne(ctx)
		}(); err != nil && ctx.Err() == nil {
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
	// 单次 panic 不得终止进程：把任务退回重试（耗尽预算后进入死信），避免事件停留在租约中。
	defer func() {
		if recovered := recover(); recovered != nil {
			jobOutcome = "failure"
			w.logger.Error("feishu event handling panicked", "job_id", job.ID,
				"panic", fmt.Sprint(recovered), "stack", string(debug.Stack()))
			retryAt := w.clock().Add(w.config.PollInterval)
			if err := w.jobs.Fail(context.WithoutCancel(ctx), job.ID, w.config.WorkerID, job.FencingToken,
				retryAt, "feishu event handling panicked", w.clock()); err != nil {
				w.logger.Warn("fail feishu event job after panic failed", "job_id", job.ID, "error", err)
			}
		}
	}()
	eventID, err := decodeFeishuEventJob(job)
	if err != nil {
		// 载荷损坏无法定位事件：不能直接完成丢弃，否则事件永久消失；
		// 交给失败重试并在预算耗尽后进入死信，保留现场供排查。
		jobOutcome = "failure"
		w.logger.Error("invalid feishu event job payload", "job_id", job.ID, "error", err)
		retryAt := w.clock().Add(w.config.PollInterval)
		if failErr := w.jobs.Fail(context.WithoutCancel(ctx), job.ID, w.config.WorkerID, job.FencingToken,
			retryAt, "invalid feishu event job payload: "+err.Error(), w.clock()); failErr != nil {
			w.logger.Warn("fail invalid feishu event job failed", "job_id", job.ID, "error", failErr)
		}
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
	if err := w.events.MarkEventStatus(ctx, eventID, FeishuEventProcessing, "", w.clock()); err != nil {
		// 状态未落库时继续处理会在后续写入失败时丢失进度，交给任务重试。
		jobOutcome = "failure"
		w.failJob(ctx, job, eventID, err)
		return
	}

	outcome, err := w.process(ctx, record)
	if err != nil {
		jobOutcome = "failure"
		w.failJob(ctx, job, eventID, err)
		return
	}
	if outcome != "" {
		if err := w.events.MarkEventStatus(context.WithoutCancel(ctx), eventID, outcome, "", w.clock()); err != nil {
			// 终态未落库时任务不能完成，否则事件会停留在 processing 且无法再被去重。
			jobOutcome = "failure"
			w.failJob(context.WithoutCancel(ctx), job, eventID, err)
			return
		}
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
	groupPolicyID := uuid.Nil
	if messageEvent.ChatType == feishu.ChatTypeGroup {
		groupPolicyID = w.sharedChats[feishuChatScope{record.AppID, record.TenantKey, messageEvent.ChatID}]
		if groupPolicyID != uuid.Nil {
			// 共享群聊只信任准确的机器人标识，拒绝未知应用和同名 mention。
			if (routedBot == nil && record.AppID != w.config.DefaultAppID) ||
				botOpenID == "" || !messageEvent.Triggered(botOpenID, "") {
				return FeishuEventIgnored, nil
			}
		} else if !messageEvent.Triggered(botOpenID, botName) {
			return FeishuEventIgnored, nil
		}
	}
	text := messageEvent.Text()
	if text == "" {
		return FeishuEventIgnored, nil
	}
	if len(text) > w.config.MaxMessageBytes {
		text = truncateUTF8Bytes(text, w.config.MaxMessageBytes)
	}
	threadID, rootMessageID, ignored := w.threadKey(messageEvent)
	if ignored {
		return FeishuEventIgnored, nil
	}
	identity, err := w.identities.Resolve(ctx, record.TenantKey, messageEvent.SenderOpenID)
	if err != nil {
		if errors.Is(err, ErrForbidden) && groupPolicyID != uuid.Nil {
			return FeishuEventIgnored, nil
		}
		return "", err
	}
	// 回填事件归属用户；账号删除据此清理可能包含正文的原始事件载荷。
	if err := w.events.BindUser(ctx, record.EventID, identity.UserID); err != nil {
		return "", err
	}
	var routeAgentID *uuid.UUID
	var routeAgentVersion *int
	var routeOwnerID uuid.UUID
	routeMode := w.config.Mode
	if routedBot != nil {
		binding, found, err := w.bots.FindFeishuBotBinding(ctx, routedBot.ID)
		if err != nil {
			return "", err
		}
		if !found {
			return FeishuEventIgnored, nil
		}
		if groupPolicyID == uuid.Nil && binding.UserID != identity.UserID {
			return FeishuEventIgnored, nil
		}
		// 助手取消发布或归档后绑定会被解除；此处兜底避免旧绑定继续执行。
		if binding.AgentStatus != AgentPublished {
			return FeishuEventIgnored, nil
		}
		agentID, agentVersion := binding.AgentID, binding.AgentVersion
		routeAgentID, routeAgentVersion = &agentID, &agentVersion
		routeMode = binding.AgentMode
		routeOwnerID = binding.UserID
	} else {
		if w.config.DefaultAgentID == nil || w.config.DefaultAgentVersion == nil {
			// 未登记机器人且未配置默认助手时直接忽略，避免事件无限重试进入死信。
			return FeishuEventIgnored, nil
		}
		// 默认助手只对属主开放，授权共享群聊使用独立的模型策略。
		if w.config.DefaultAgentOwnerID == nil || (groupPolicyID == uuid.Nil && *w.config.DefaultAgentOwnerID != identity.UserID) {
			return FeishuEventIgnored, nil
		}
		routeAgentID, routeAgentVersion = w.config.DefaultAgentID, w.config.DefaultAgentVersion
		routeOwnerID = *w.config.DefaultAgentOwnerID
	}
	if routeAgentID == nil || routeAgentVersion == nil {
		return FeishuEventIgnored, nil
	}
	if groupPolicyID != uuid.Nil {
		return w.processSharedChat(ctx, record, messageEvent, identity.UserID, routeOwnerID, groupPolicyID, text, threadID, rootMessageID, routedBot == nil)
	}
	// 未登记机器人使用默认应用，沿用迁移前的空 app_id 绑定，避免历史会话被拆成新话题。
	conversationAppID := record.AppID
	if routedBot == nil {
		conversationAppID = ""
	}
	binding, found, err := w.findConversation(ctx, conversationAppID, record.TenantKey, messageEvent.ChatID, threadID, rootMessageID)
	if err != nil {
		return "", err
	}
	if !found {
		binding, err = w.createConversation(ctx, identity.UserID, conversationAppID, record.TenantKey, routeAgentID, messageEvent, threadID, rootMessageID)
		if err != nil {
			return "", err
		}
	}
	// 会话归属校验：话题会话的所有者是首次绑定的用户，默认应用（未登记机器人）同样适用。
	// 群聊中其他成员不得以会话所有者身份驱动 Agent、工具与预算。
	if binding.UserID != identity.UserID {
		w.logger.Warn("feishu sender is not conversation owner",
			"session_id", binding.SessionID, "chat_id", messageEvent.ChatID)
		return FeishuEventIgnored, nil
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
	// 旧话题可能创建于机器人绑定解析完成前；补齐 Agent 绑定后 Web 端才能继续该会话。
	if session.AgentID == nil && routeAgentID != nil {
		if binder, ok := w.sessions.(SessionAgentBinder); ok {
			if err := binder.BindSessionAgent(ctx, session.UserID, session.ID, *routeAgentID, w.clock()); err != nil {
				w.logger.Warn("bind feishu session agent failed", "session_id", session.ID, "error", err)
			} else {
				session.AgentID = routeAgentID
			}
		}
	}
	// 乱序保护：消息时间取飞书 create_time，而不是任务实际处理时间；
	// 这样旧事件晚到时仍按发生时间落库，上下文与摘要顺序不会被投递顺序打乱。
	occurredAt := messageEvent.CreatedAt(w.clock())
	inbound := Message{
		ID: uuid.New(), SessionID: session.ID, UserID: binding.UserID, Role: MessageUser,
		Content: text, ExternalMessageID: messageEvent.MessageID, SpeakerOpenID: messageEvent.SenderOpenID,
		CreatedAt: occurredAt,
	}
	stored, created, err := w.messages.AppendInbound(ctx, inbound, mutationEvent(binding.UserID, "message.created", "message", uuid.New()))
	if err != nil {
		return "", err
	}
	// 飞书重推同一消息时消息已存在，但首次处理可能在上一步之后失败；此处不能提前返回，
	// 否则消息落库成功而 Run 创建失败的事件会被永久丢弃。Run 与外发记录都按幂等键去重。
	// 入站消息只写 PostgreSQL；必须清理 Redis 热窗口，否则下一次 Run 会命中旧缓存而看不到本轮问题。
	// 冷缓存或 Redis 重启后，清理也能让下一次 Run 从数据库重建完整历史。
	// 重推事件同样需要清理：上次处理可能停在清理之前，缓存里仍是旧窗口。
	if w.hotMessages != nil {
		if err := w.hotMessages.Invalidate(ctx, binding.UserID, session.ID); err != nil {
			w.logger.Warn("invalidate hot messages after inbound failed", "session_id", session.ID, "error", err)
		}
	}
	if created && messageEvent.ChatType == feishu.ChatTypeGroup && w.speakerFacts != nil {
		// 群聊话题中个人事实只写入发言人私有记忆：消息归属会话所有者，但提取与写入身份切换为发言人本人。
		// 失败不影响回复链路，仅记录日志等待下一轮重试。
		if err := w.speakerFacts.MaybeEnqueueSpeaker(ctx, binding.UserID, session.ID, identity.UserID, messageEvent.SenderOpenID); err != nil {
			w.logger.Warn("enqueue speaker fact extraction failed", "session_id", session.ID, "speaker_open_id", messageEvent.SenderOpenID, "error", err)
		}
	}
	principal, err := NewPrincipal(binding.UserID, RoleUser)
	if err != nil {
		return "", err
	}
	run, err := w.runs.Create(ctx, principal, CreateRunInput{
		SessionID: session.ID, AgentID: routeAgentID, AgentVersion: routeAgentVersion,
		Mode: routeMode, IdempotencyKey: "feishu:" + messageEvent.MessageID,
		TriggerMessageID: &stored.ID,
	})
	if err != nil {
		return "", err
	}
	if run.UserID != binding.UserID || run.SessionID != session.ID ||
		run.TriggerMessageID == nil || *run.TriggerMessageID != stored.ID {
		return "", fmt.Errorf("%w: Feishu run does not match the triggering message", ErrResourceConflict)
	}
	ctx = logging.WithRunID(ctx, run.ID.String())
	// 先创建外发记录占位，Run 完成后由回复 Worker 增量更新卡片。
	return w.createReply(ctx, record.AppID, messageEvent, threadID, binding.UserID, session.ID, run.ID)
}

func (w *FeishuEventWorker) createReply(ctx context.Context, appID string, messageEvent feishu.MessageEvent, threadID string, userID, sessionID, runID uuid.UUID) (string, error) {
	err := w.outbound.CreateOutbound(ctx, FeishuOutbound{
		ID: uuid.New(), UserID: userID, SessionID: sessionID, RunID: runID, AppID: appID,
		ChatID: messageEvent.ChatID, ThreadID: threadID, ReplyToMessageID: messageEvent.MessageID,
		Kind: "card", Status: "pending", DedupeKey: "feishu.run:" + runID.String(),
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

// findConversation 优先使用按应用隔离的检索能力，兼容未实现该能力的历史测试替身。
func (w *FeishuEventWorker) findConversation(ctx context.Context, appID, tenantKey, chatID, threadID, rootMessageID string) (FeishuConversationBinding, bool, error) {
	if scoped, ok := w.conversations.(FeishuConversationAppScopedRepository); ok {
		return scoped.FindConversationForApp(ctx, appID, tenantKey, chatID, threadID, rootMessageID)
	}
	return w.conversations.FindConversation(ctx, tenantKey, chatID, threadID, rootMessageID)
}

// createConversation 在同一事务创建 Session、所有者成员与话题绑定。
func (w *FeishuEventWorker) createConversation(ctx context.Context, userID uuid.UUID, appID, tenantKey string, agentID *uuid.UUID, event feishu.MessageEvent, threadID, rootMessageID string) (FeishuConversationBinding, error) {
	sessionID := uuid.New()
	// 群聊话题禁止注入个人长期记忆与画像，单聊保持私有记忆策略。
	memoryPolicy := SessionMemoryPrivate
	if event.ChatType == feishu.ChatTypeGroup {
		memoryPolicy = SessionMemoryGroup
	}
	session := Session{
		ID: sessionID, UserID: userID, AgentID: agentID, Title: feishuTitle(event.ChatID), Source: SessionSourceFeishu,
		MemoryPolicy: memoryPolicy, Status: SessionActive, Version: 1,
	}
	binding := FeishuConversationBinding{
		ID: uuid.New(), UserID: userID, Provider: "feishu", TenantKey: tenantKey, AppID: appID,
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
