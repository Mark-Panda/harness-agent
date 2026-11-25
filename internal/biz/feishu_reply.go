package biz

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Mark-Panda/harness-agent/pkg/feishu"
	"github.com/google/uuid"
)

// FeishuReplyWorkerConfig 是外发回复 Worker 的轮询、租约与限流配置。
type FeishuReplyWorkerConfig struct {
	// DefaultAppID 标识默认客户端所使用的应用。
	DefaultAppID string
	// WorkerID 实例标识。
	WorkerID string
	// Concurrency 单实例并发投递数。
	Concurrency int
	// PollInterval 无任务时的轮询间隔。
	PollInterval time.Duration
	// Lease 外发记录租约时长。
	Lease time.Duration
	// PollRunInterval Run 未结束时再次检查的间隔，避免高频调用飞书。
	PollRunInterval time.Duration
	// RunWaitTimeout 在没有生效的执行截止时间时，限制外发等待时长。
	RunWaitTimeout time.Duration
	// MaxTextBytes 单条回复文本上限，按 UTF-8 边界截断。
	MaxTextBytes int
	// CardUpdateInterval 同一卡片两次增量更新的最小间隔，避免放大小飞书卡片更新限流。
	CardUpdateInterval time.Duration
	// MaxConcurrentPerChat 单个飞书会话的并发投递上限，默认 1。
	MaxConcurrentPerChat int
	// MaxConcurrentPerUser 单个用户的并发投递上限，默认 2。
	MaxConcurrentPerUser int
}

// normalize 补齐回复 Worker 默认配置。
func (c FeishuReplyWorkerConfig) normalize() FeishuReplyWorkerConfig {
	if c.Concurrency <= 0 {
		c.Concurrency = 1
	}
	if c.PollInterval <= 0 {
		c.PollInterval = time.Second
	}
	if c.Lease <= 0 {
		c.Lease = 30 * time.Second
	}
	if c.PollRunInterval <= 0 {
		c.PollRunInterval = 3 * time.Second
	}
	if c.RunWaitTimeout <= 0 {
		c.RunWaitTimeout = 10 * time.Minute
	}
	if c.MaxTextBytes <= 0 {
		c.MaxTextBytes = 16 * 1024
	}
	if c.CardUpdateInterval <= 0 {
		c.CardUpdateInterval = 5 * time.Second
	}
	if c.MaxConcurrentPerChat <= 0 {
		c.MaxConcurrentPerChat = 1
	}
	if c.MaxConcurrentPerUser <= 0 {
		c.MaxConcurrentPerUser = 2
	}
	return c
}

// keyedLimiter 是按业务键计数的并发信号量，用于限制单个会话或用户的并发投递。
type keyedLimiter struct {
	mu     sync.Mutex
	counts map[string]int
}

// newKeyedLimiter 构造空的并发限制器。
func newKeyedLimiter() *keyedLimiter {
	return &keyedLimiter{counts: make(map[string]int)}
}

// acquire 尝试占用一个并发名额；limit <= 0 或键为空时视为不限制。
func (l *keyedLimiter) acquire(key string, limit int) bool {
	if limit <= 0 || key == "" {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.counts[key] >= limit {
		return false
	}
	l.counts[key]++
	return true
}

// release 释放并发名额，计数归零后删除键避免长期占用内存。
func (l *keyedLimiter) release(key string) {
	if key == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.counts[key] <= 1 {
		delete(l.counts, key)
		return
	}
	l.counts[key]--
}

// FeishuReplyWorkerDependencies 聚合外发 Worker 所需依赖。
type FeishuReplyWorkerDependencies struct {
	// Outbound 外发记录仓储。
	Outbound FeishuOutboundStore
	// Runs Run 读取端口，用于判断执行状态。
	Runs RunRepository
	// Sender 只处理由默认应用验证的事件。
	Sender FeishuReplySender
	// Senders 校验注册机器人的身份及凭据并解析客户端。
	Senders FeishuReplySenderResolver
	// Logger 结构化日志。
	Logger *slog.Logger
	// Clock 当前时间函数。
	Clock func() time.Time
}

// FeishuReplyWorker 将 Run 结果通过可更新卡片回复到飞书，失败时降级为文本。
type FeishuReplyWorker struct {
	outbound    FeishuOutboundStore
	runs        RunRepository
	sender      FeishuReplySender
	senders     FeishuReplySenderResolver
	config      FeishuReplyWorkerConfig
	clock       func() time.Time
	logger      *slog.Logger
	chatLimiter *keyedLimiter
	userLimiter *keyedLimiter
}

// NewFeishuReplyWorker 构造外发 Worker。
func NewFeishuReplyWorker(dependencies FeishuReplyWorkerDependencies, config FeishuReplyWorkerConfig) (*FeishuReplyWorker, error) {
	if dependencies.Outbound == nil || dependencies.Runs == nil || (dependencies.Sender == nil && dependencies.Senders == nil) {
		return nil, invalid("feishu reply worker requires outbound, runs and sender")
	}
	if strings.TrimSpace(config.WorkerID) == "" {
		return nil, invalid("feishu reply worker id is required")
	}
	logger := dependencies.Logger
	if logger == nil {
		logger = slog.Default()
	}
	clock := dependencies.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &FeishuReplyWorker{
		outbound: dependencies.Outbound, runs: dependencies.Runs, sender: dependencies.Sender, senders: dependencies.Senders,
		config: config.normalize(), clock: clock, logger: logger,
		chatLimiter: newKeyedLimiter(), userLimiter: newKeyedLimiter(),
	}, nil
}

// Start 启动配置数量的投递协程并在上下文取消后返回。
func (w *FeishuReplyWorker) Start(ctx context.Context) {
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

// loop 持续领取外发记录；无任务时按轮询间隔等待。
func (w *FeishuReplyWorker) loop(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		if WorkerDraining() {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if _, err := func() (bool, error) {
			// 单次迭代 panic 不得终止投递协程：记录堆栈后继续处理下一条外发记录。
			defer func() {
				if recovered := recover(); recovered != nil {
					w.logger.Error("feishu reply worker iteration panicked",
						"panic", fmt.Sprint(recovered), "stack", string(debug.Stack()))
				}
			}()
			return w.ProcessOne(ctx)
		}(); err != nil && ctx.Err() == nil {
			w.logger.Warn("claim feishu outbound failed", "error", err)
		}
		timer.Reset(w.config.PollInterval)
	}
}

// ProcessOne 领取并投递至多一条外发记录。
func (w *FeishuReplyWorker) ProcessOne(ctx context.Context) (bool, error) {
	outbound, claimed, err := w.outbound.ClaimOutbound(ctx, w.config.WorkerID, w.config.Lease, w.clock())
	if err != nil {
		return false, err
	}
	if !claimed {
		return false, nil
	}
	// 同一飞书会话并发投递会放大开放平台限流，拿不到名额时延后重试，不消耗失败预算。
	if !w.chatLimiter.acquire(outbound.ChatID, w.config.MaxConcurrentPerChat) {
		w.deferOutbound(ctx, outbound, w.clock().Add(w.config.PollInterval))
		return true, nil
	}
	defer w.chatLimiter.release(outbound.ChatID)
	// 同一用户的多个会话同样限制并发，避免单用户突发流量拖垮外发队列。
	if !w.userLimiter.acquire(outbound.UserID.String(), w.config.MaxConcurrentPerUser) {
		w.deferOutbound(ctx, outbound, w.clock().Add(w.config.PollInterval))
		return true, nil
	}
	defer w.userLimiter.release(outbound.UserID.String())
	leaseCtx, cancel := context.WithTimeout(ctx, outbound.LeaseExpiresAt.Sub(w.clock()))
	defer cancel()
	w.handle(leaseCtx, outbound)
	return true, nil
}

// deferOutbound 延后外发记录且不消耗失败预算。
func (w *FeishuReplyWorker) deferOutbound(ctx context.Context, outbound FeishuOutbound, retryAt time.Time) {
	if err := w.outbound.DeferOutbound(context.WithoutCancel(ctx), outbound, retryAt, w.clock()); err != nil && !errors.Is(err, ErrFeishuOutboundClaimLost) {
		w.logger.Warn("defer feishu outbound failed", "outbound_id", outbound.ID, "error", err)
	}
}

// shouldReplyInThread 判断外发记录是否应走话题回复：单聊（p2p）与空话题保持普通回复。
func shouldReplyInThread(outbound FeishuOutbound) bool {
	return outbound.ThreadID != "" && outbound.ThreadID != "p2p"
}

// isBootstrapAlias 判断话题键是否为主时间线根消息的过渡别名。
// 过渡别名表示话题尚未创建，回复失败时允许降级为普通回复；真实话题 ID 不允许降级到主时间线。
func isBootstrapAlias(outbound FeishuOutbound) bool {
	return outbound.ThreadID != "" && outbound.ThreadID == outbound.ReplyToMessageID
}

// senderFor 在每次飞书调用前校验当前机器人路由与领取凭证。
func (w *FeishuReplyWorker) senderFor(ctx context.Context, outbound FeishuOutbound) (FeishuReplySender, error) {
	var sender FeishuReplySender
	switch {
	case outbound.DefaultRoute && outbound.BotID == uuid.Nil && outbound.AppID != "" && outbound.AppID == w.config.DefaultAppID:
		sender = w.sender
	case !outbound.DefaultRoute && outbound.BotID != uuid.Nil && outbound.AppID != "" && w.senders != nil:
		var err error
		sender, err = w.senders.ResolveSender(ctx, outbound.BotID, outbound.AppID)
		if err != nil {
			return nil, err
		}
	}
	if sender == nil {
		return nil, invalid("feishu outbound sender route is unavailable")
	}
	if err := w.outbound.CheckOutboundClaim(ctx, outbound, w.clock()); err != nil {
		return nil, err
	}
	return sender, nil
}

// postReply reserves the exact request before invoking the external POST. Only a
// confirmed rejection of the first request may enable a different reply.
func (w *FeishuReplyWorker) postReply(ctx context.Context, outbound *FeishuOutbound, request feishu.ReplyRequest, fingerprint string, groupText *string) (bool, error) {
	for {
		sender, err := w.senderFor(ctx, *outbound)
		if err != nil {
			return false, err
		}
		request.UUID = uuid.NewString()
		post := &FeishuOutboundPost{Request: request, StartedAt: w.clock(), CardFingerprint: fingerprint, GroupText: groupText}
		if err := w.outbound.SetOutboundPost(ctx, *outbound, post, post.StartedAt); err != nil {
			return false, err
		}
		outbound.Post = post
		err = w.deliverPost(ctx, outbound, sender)
		if err == nil || !feishu.IsMessageRejected(err) {
			return false, err
		}
		if err := w.outbound.SetOutboundPost(ctx, *outbound, nil, w.clock()); err != nil {
			return false, err
		}
		outbound.Post = nil
		if !request.InThread || !isBootstrapAlias(*outbound) || !feishu.IsThreadReplyUnsupported(err) {
			return !feishu.IsRateLimited(err) && !feishu.IsTokenInvalid(err), err
		}
		request.InThread = false
	}
}

// deliverPost retries within Feishu's one-hour UUID window, allowing one minute
// for a request to reach Feishu before that window can expire.
func (w *FeishuReplyWorker) deliverPost(ctx context.Context, outbound *FeishuOutbound, sender FeishuReplySender) error {
	post := outbound.Post
	replayFor := post.StartedAt.Add(time.Hour - time.Minute).Sub(w.clock())
	if replayFor <= 0 {
		return errors.New("feishu reply outcome is unknown and its deduplication window has expired")
	}
	postCtx, cancel := context.WithTimeout(ctx, replayFor)
	defer cancel()
	result, err := sender.Reply(postCtx, outbound.ReplyToMessageID, post.Request)
	if err != nil {
		return err
	}
	if result.MessageID == "" {
		return errors.New("feishu reply response is missing message id")
	}
	if post.Request.InThread {
		w.promoteThread(ctx, *outbound, result.ThreadID)
	}
	now := w.clock()
	if err := w.outbound.UpdateOutboundMessageID(ctx, *outbound, result.MessageID, now); err != nil {
		// Do not let persistence errors be mistaken for a Feishu rejection.
		return fmt.Errorf("persist feishu reply message id: %w", err)
	}
	outbound.MessageID = result.MessageID
	if post.Request.Type == "text" {
		outbound.Kind = "text"
	} else {
		outbound.Kind = "card"
		outbound.CardUpdatedAt, outbound.CardFingerprint = now, post.CardFingerprint
		outbound.Post = nil
	}
	return nil
}

func (w *FeishuReplyWorker) postCard(ctx context.Context, outbound *FeishuOutbound, title, text string) (bool, error) {
	card, err := json.Marshal(buildFeishuCard(title, text, w.config.MaxTextBytes))
	if err != nil {
		return false, err
	}
	request := feishu.ReplyRequest{Type: "interactive", Content: string(card), InThread: shouldReplyInThread(*outbound)}
	return w.postReply(ctx, outbound, request, cardFingerprint(title, text, w.config.MaxTextBytes), nil)
}

// promoteThread 把根消息过渡别名升级为飞书真实话题 ID，失败只记录日志不影响回复送达。
func (w *FeishuReplyWorker) promoteThread(ctx context.Context, outbound FeishuOutbound, threadID string) {
	if threadID == "" || !isBootstrapAlias(outbound) {
		return
	}
	if _, err := w.outbound.PromoteOutboundThread(ctx, outbound, threadID, w.clock()); err != nil && !errors.Is(err, ErrFeishuOutboundClaimLost) {
		w.logger.Warn("promote feishu thread failed", "outbound_id", outbound.ID, "thread_id", threadID, "error", err)
	}
}

// handle 根据 Run 状态发送占位卡片、增量更新或最终文本。
func (w *FeishuReplyWorker) handle(ctx context.Context, outbound FeishuOutbound) {
	finishJob := beginJob()
	jobOutcome := "success"
	defer func() { finishJob(jobOutcome) }()
	// 单次 panic 不得终止进程：记录堆栈并延后外发，避免记录停留在租约中无人投递。
	defer func() {
		if recovered := recover(); recovered != nil {
			jobOutcome = "failure"
			w.logger.Error("feishu reply handling panicked", "outbound_id", outbound.ID,
				"panic", fmt.Sprint(recovered), "stack", string(debug.Stack()))
			w.deferOutbound(ctx, outbound, w.clock().Add(w.config.PollInterval))
		}
	}()
	if outbound.Post != nil {
		if outbound.MessageID == "" || outbound.Kind != "text" {
			sender, err := w.senderFor(ctx, outbound)
			if err == nil {
				err = w.deliverPost(ctx, &outbound, sender)
			}
			if err != nil {
				jobOutcome = "failure"
				w.failOutbound(ctx, outbound, err)
				return
			}
		}
		if outbound.Kind == "text" {
			if err := w.completeReply(ctx, outbound, outbound.Post.GroupText); err != nil {
				jobOutcome = "failure"
				w.failOutbound(ctx, outbound, err)
			}
			return
		}
	}
	run, err := w.runs.GetRun(ctx, outbound.UserID, outbound.RunID)
	if err != nil {
		if errors.Is(err, ErrResourceNotFound) {
			// Run 已被删除时直接结束外发，避免永久重试。
			_ = w.outbound.CompleteOutbound(context.WithoutCancel(ctx), outbound, w.clock())
			return
		}
		// 瞬时故障不能当作 Run 已删除：否则用户永远收不到最终答复。
		jobOutcome = "failure"
		w.failOutbound(ctx, outbound, err)
		return
	}
	terminal, title, text, err := w.render(ctx, run)
	if err != nil {
		// 渲染失败按基础设施故障重试，避免把读取故障固化成“没有生成回复”发给用户。
		jobOutcome = "failure"
		w.failOutbound(ctx, outbound, err)
		return
	}
	if !terminal {
		w.waitRun(ctx, outbound, run, title, text)
		return
	}
	var groupText *string
	if isFeishuSharedGroup(run) && run.Status == RunCompleted {
		// 卡片与文本降级的共同可见正文，避免后续发言人读取投递时被截掉的答案。
		visible := truncateUTF8Bytes(text, max(0, w.config.MaxTextBytes-len(title)-1-len("…")))
		groupText = &visible
	}
	if err := w.sendOrUpdate(ctx, &outbound, title, text, groupText); err != nil {
		jobOutcome = "failure"
		w.failOutbound(ctx, outbound, err)
		return
	}
	if err := w.completeReply(ctx, outbound, groupText); err != nil {
		jobOutcome = "failure"
		w.failOutbound(ctx, outbound, err)
	}
}

func (w *FeishuReplyWorker) completeReply(ctx context.Context, outbound FeishuOutbound, groupText *string) error {
	if groupText != nil {
		return w.outbound.CompleteGroupReply(context.WithoutCancel(ctx), outbound, *groupText, w.clock())
	}
	return w.outbound.CompleteOutbound(context.WithoutCancel(ctx), outbound, w.clock())
}

// render 把 Run 状态映射为卡片标题与正文；终态返回 true。
// 返回错误表示读取最终内容时遇到基础设施故障，调用方应重试而不是降级为错误答复。
func (w *FeishuReplyWorker) render(ctx context.Context, run Run) (bool, string, string, error) {
	switch run.Status {
	case RunCompleted:
		runID := run.ID
		message, err := w.outbound.LatestAssistantMessage(ctx, run.UserID, run.SessionID, &runID)
		if err != nil && !errors.Is(err, ErrResourceNotFound) {
			return false, "", "", err
		}
		if strings.TrimSpace(message.Content) == "" {
			return true, "执行完成", "任务已执行完成，但没有生成可展示的回复。", nil
		}
		return true, "执行完成", message.Content, nil
	case RunFailed:
		return true, "执行失败", "任务执行失败，请稍后重试或在 Web 端查看执行详情。", nil
	case RunCancelled:
		return true, "已取消", "任务已取消。", nil
	case RunWaitingApproval:
		return false, "等待审批", "任务正在等待人工审批，请到 Web 端处理。", nil
	case RunReconciliationRequired:
		return true, "需要对账", "任务需要人工对账，请在 Web 端查看执行详情。", nil
	default:
		return false, "正在处理", "已收到你的消息，正在执行，请稍候。", nil
	}
}

// waitRun 在 Run 未结束时发送或增量更新占位卡片，并延后再次检查。
func (w *FeishuReplyWorker) waitRun(ctx context.Context, outbound FeishuOutbound, run Run, title, text string) {
	deadline := outbound.CreatedAt.Add(w.config.RunWaitTimeout)
	if run.DeadlineAt != nil && !run.DeadlineAt.IsZero() && (run.Status != RunQueued || run.StartedAt != nil) {
		// 审批恢复会顺延执行截止时间，首次领取也会补回排队时长。
		deadline = *run.DeadlineAt
	}
	if run.Status != RunWaitingApproval && !w.clock().Before(deadline) {
		final := "任务执行超时，请稍后在 Web 端查看结果。"
		if err := w.sendOrUpdate(ctx, &outbound, "执行超时", final, nil); err != nil {
			w.failOutbound(ctx, outbound, err)
			return
		}
		if err := w.completeReply(ctx, outbound, nil); err != nil {
			w.failOutbound(ctx, outbound, err)
		}
		return
	}
	card := buildFeishuCard(title, text, w.config.MaxTextBytes)
	fingerprint := cardFingerprint(title, text, w.config.MaxTextBytes)
	if outbound.MessageID == "" {
		if _, err := w.postCard(ctx, &outbound, title, text); err != nil {
			w.logger.Warn("send feishu placeholder card failed", "outbound_id", outbound.ID, "error", err)
		}
		w.deferOutbound(ctx, outbound, w.clock().Add(w.config.PollRunInterval))
		return
	}
	// 内容未发生变化时不做任何卡片更新，减少无效调用。
	if fingerprint == outbound.CardFingerprint {
		w.deferOutbound(ctx, outbound, w.clock().Add(w.config.PollRunInterval))
		return
	}
	// 未达到最小更新间隔时按剩余时间延后，避免连续更新触发飞书限流。
	if !outbound.CardUpdatedAt.IsZero() {
		if elapsed := w.clock().Sub(outbound.CardUpdatedAt); elapsed < w.config.CardUpdateInterval {
			w.deferOutbound(ctx, outbound, w.clock().Add(w.config.CardUpdateInterval-elapsed))
			return
		}
	}
	sender, senderErr := w.senderFor(ctx, outbound)
	if senderErr != nil {
		w.logger.Warn("resolve feishu sender failed", "outbound_id", outbound.ID, "error", senderErr)
		w.deferOutbound(ctx, outbound, w.clock().Add(w.config.PollRunInterval))
		return
	}
	if err := sender.UpdateCard(ctx, outbound.MessageID, card); err != nil {
		// 非终态的增量更新失败只记录日志，等待下一轮或最终文本降级。
		w.logger.Warn("update feishu card failed", "outbound_id", outbound.ID, "status", run.Status, "error", err)
		w.deferOutbound(ctx, outbound, w.clock().Add(w.config.PollRunInterval))
		return
	}
	if err := w.outbound.MarkOutboundCardUpdated(ctx, outbound, fingerprint, w.clock()); err != nil {
		w.logger.Warn("persist feishu card fingerprint failed", "outbound_id", outbound.ID, "error", err)
	}
	w.deferOutbound(ctx, outbound, w.clock().Add(w.config.PollRunInterval))
}

// sendOrUpdate delivers terminal content, using text only after a definite card rejection.
func (w *FeishuReplyWorker) sendOrUpdate(ctx context.Context, outbound *FeishuOutbound, title, text string, groupText *string) error {
	if outbound.MessageID != "" {
		fingerprint := cardFingerprint(title, text, w.config.MaxTextBytes)
		if fingerprint == outbound.CardFingerprint {
			return nil
		}
		sender, err := w.senderFor(ctx, *outbound)
		if err != nil {
			return err
		}
		err = sender.UpdateCard(ctx, outbound.MessageID, buildFeishuCard(title, text, w.config.MaxTextBytes))
		if err == nil {
			return w.outbound.MarkOutboundCardUpdated(ctx, *outbound, fingerprint, w.clock())
		}
		if !feishu.IsMessageRejected(err) || feishu.IsRateLimited(err) || feishu.IsTokenInvalid(err) {
			return err
		}
	} else {
		rejected, err := w.postCard(ctx, outbound, title, text)
		if !rejected {
			return err
		}
	}
	content, err := json.Marshal(map[string]string{"text": truncateFeishuText(title+"\n"+text, w.config.MaxTextBytes)})
	if err != nil {
		return err
	}
	request := feishu.ReplyRequest{Type: "text", Content: string(content), InThread: shouldReplyInThread(*outbound)}
	_, err = w.postReply(ctx, outbound, request, "", groupText)
	return err
}

// failOutbound 记录失败并按飞书限流建议退避，超出预算进入死信。
func (w *FeishuReplyWorker) failOutbound(ctx context.Context, outbound FeishuOutbound, err error) {
	if errors.Is(err, ErrFeishuOutboundClaimLost) {
		return
	}
	retryAt := w.clock().Add(feishuBackoff(outbound.Attempts))
	var apiError *feishu.APIError
	if errorsAsAPI(err, &apiError) && apiError.RetryAfter > 0 {
		retryAt = w.clock().Add(apiError.RetryAfter)
	}
	reason := feishuErrorCode(err)
	w.logger.Warn("feishu outbound failed", "outbound_id", outbound.ID, "attempts", outbound.Attempts, "error", reason)
	_ = w.outbound.FailOutbound(context.WithoutCancel(ctx), outbound, reason, retryAt, w.clock())
}

// errorsAsAPI 统一用 errors.As 识别包装后的飞书业务错误，避免降级路径漏判限流。
func errorsAsAPI(err error, target **feishu.APIError) bool {
	if err == nil {
		return false
	}
	return errors.As(err, target)
}

// truncateFeishuText 按 UTF-8 边界截断文本，避免把中文或 emoji 截成半个字符导致飞书渲染失败。
func truncateFeishuText(text string, maxTextBytes int) string {
	if maxTextBytes <= 0 || len(text) <= maxTextBytes {
		return text
	}
	limit := maxTextBytes
	// 向前回退到完整的 UTF-8 字符起始字节，保证输出是合法前缀。
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit]
}

// cardFingerprint 计算卡片标题与截断后正文的指纹，用于跳过重复更新并跨副本协作。
func cardFingerprint(title, text string, maxTextBytes int) string {
	sum := sha256.Sum256([]byte(title + "\x00" + truncateFeishuText(text, maxTextBytes)))
	return hex.EncodeToString(sum[:])
}

// buildFeishuCard 生成可更新的飞书卡片。
// 使用卡片 JSON 2.0 的 markdown 组件，支持标题、表格、代码块、分割线等标准 Markdown 语法。
func buildFeishuCard(title, text string, maxTextBytes int) map[string]any {
	text = truncateFeishuText(text, maxTextBytes)
	return map[string]any{
		"schema": "2.0",
		"config": map[string]any{"update_multi": true, "width_mode": "fill"},
		"header": map[string]any{
			"template": "blue",
			"title":    map[string]any{"tag": "plain_text", "content": title},
		},
		"body": map[string]any{
			"elements": []map[string]any{
				{"tag": "markdown", "content": text},
			},
		},
	}
}

// 编译期断言 RunService 实现 Run 创建端口。
var _ FeishuRunCreator = (*RunService)(nil)
