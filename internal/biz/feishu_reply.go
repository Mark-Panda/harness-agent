package biz

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Mark-Panda/harness-agent/pkg/feishu"
	"github.com/Mark-Panda/harness-agent/pkg/metrics"
	"github.com/Mark-Panda/harness-agent/pkg/telemetry"
	"go.opentelemetry.io/otel/attribute"
)

// FeishuReplyWorkerConfig 是外发回复 Worker 的轮询、租约与限流配置。
type FeishuReplyWorkerConfig struct {
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
	// RunWaitTimeout Run 长时间未完成时降级为超时提示。
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
	// Sender 飞书回复发送端口。
	Sender FeishuReplySender
	// Conversations 话题绑定仓储，用于把主时间线根消息别名升级为真实话题 ID；为空时跳过升级。
	Conversations FeishuConversationRepository
	// Logger 结构化日志。
	Logger *slog.Logger
	// Clock 当前时间函数。
	Clock func() time.Time
}

// FeishuReplyWorker 将 Run 结果通过可更新卡片回复到飞书，失败时降级为文本。
type FeishuReplyWorker struct {
	outbound      FeishuOutboundStore
	runs          RunRepository
	sender        FeishuReplySender
	conversations FeishuConversationRepository
	config        FeishuReplyWorkerConfig
	clock         func() time.Time
	logger        *slog.Logger
	chatLimiter   *keyedLimiter
	userLimiter   *keyedLimiter
}

// NewFeishuReplyWorker 构造外发 Worker。
func NewFeishuReplyWorker(dependencies FeishuReplyWorkerDependencies, config FeishuReplyWorkerConfig) (*FeishuReplyWorker, error) {
	if dependencies.Outbound == nil || dependencies.Runs == nil || dependencies.Sender == nil {
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
		outbound: dependencies.Outbound, runs: dependencies.Runs, sender: dependencies.Sender,
		conversations: dependencies.Conversations,
		config:        config.normalize(), clock: clock, logger: logger,
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
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if _, err := w.ProcessOne(ctx); err != nil && ctx.Err() == nil {
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
	w.handle(ctx, outbound)
	return true, nil
}

// deferOutbound 延后外发记录且不消耗失败预算。
func (w *FeishuReplyWorker) deferOutbound(ctx context.Context, outbound FeishuOutbound, retryAt time.Time) {
	if err := w.outbound.DeferOutbound(context.WithoutCancel(ctx), outbound.ID, retryAt, w.clock()); err != nil {
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

// sendCard 统一发送卡片：群聊话题内优先创建/复用话题，普通群不支持话题时降级为主时间线回复。
func (w *FeishuReplyWorker) sendCard(ctx context.Context, outbound FeishuOutbound, card any) (string, error) {
	if !shouldReplyInThread(outbound) {
		return w.sender.ReplyCard(ctx, outbound.ReplyToMessageID, card)
	}
	threaded, err := w.sender.ReplyCardInThread(ctx, outbound.ReplyToMessageID, card)
	if err == nil {
		// 主时间线首条回复由飞书创建话题，记录真实话题 ID 供后续追问复用同一 Session。
		w.promoteThread(ctx, outbound, threaded.ThreadID)
		return threaded.MessageID, nil
	}
	if feishu.IsRateLimited(err) {
		return "", err
	}
	if !isBootstrapAlias(outbound) {
		// 话题已存在时不能把答案降级到主时间线，避免不同话题的上下文串台；交给失败退避重试。
		return "", err
	}
	// 普通群不支持话题能力：降级为主时间线普通回复，保证至少送达一次最终结果。
	w.logger.Warn("create feishu thread failed, fallback to plain reply", "outbound_id", outbound.ID, "error", err)
	return w.sender.ReplyCard(ctx, outbound.ReplyToMessageID, card)
}

// sendText 统一发送纯文本：话题内优先保持话题上下文，普通群不支持话题时降级为主时间线回复。
func (w *FeishuReplyWorker) sendText(ctx context.Context, outbound FeishuOutbound, text string) (string, error) {
	if !shouldReplyInThread(outbound) {
		return w.sender.ReplyText(ctx, outbound.ReplyToMessageID, text)
	}
	threaded, err := w.sender.ReplyTextInThread(ctx, outbound.ReplyToMessageID, text)
	if err == nil {
		w.promoteThread(ctx, outbound, threaded.ThreadID)
		return threaded.MessageID, nil
	}
	if feishu.IsRateLimited(err) {
		return "", err
	}
	if !isBootstrapAlias(outbound) {
		return "", err
	}
	w.logger.Warn("reply feishu thread text failed, fallback to plain reply", "outbound_id", outbound.ID, "error", err)
	return w.sender.ReplyText(ctx, outbound.ReplyToMessageID, text)
}

// promoteThread 把根消息过渡别名升级为飞书真实话题 ID，失败只记录日志不影响回复送达。
func (w *FeishuReplyWorker) promoteThread(ctx context.Context, outbound FeishuOutbound, threadID string) {
	if w.conversations == nil || threadID == "" || !isBootstrapAlias(outbound) {
		return
	}
	if _, err := w.conversations.PromoteConversationThread(ctx, outbound.UserID, outbound.SessionID, outbound.ThreadID, threadID); err != nil {
		w.logger.Warn("promote feishu thread failed", "outbound_id", outbound.ID, "thread_id", threadID, "error", err)
	}
}

// handle 根据 Run 状态发送占位卡片、增量更新或最终文本。
func (w *FeishuReplyWorker) handle(ctx context.Context, outbound FeishuOutbound) {
	// 外发 Span 只记录会话定位与外发类型，卡片正文、用户消息与 Run 输出禁止进入 Trace。
	ctx, span := telemetry.Start(ctx, "feishu.reply",
		attribute.String(telemetry.AttrFeishuChatID, outbound.ChatID),
		attribute.String(telemetry.AttrFeishuThreadID, outbound.ThreadID),
		attribute.String("feishu.outbound_kind", outbound.Kind),
	)
	defer span.End()
	finishJob := metrics.StartJob("feishu.reply")
	jobOutcome := "success"
	defer func() { finishJob(jobOutcome) }()
	run, err := w.runs.GetRun(ctx, outbound.UserID, outbound.RunID)
	if err != nil {
		// Run 已被删除时直接结束外发，避免永久重试。
		_ = w.outbound.CompleteOutbound(context.WithoutCancel(ctx), outbound.ID, w.clock())
		return
	}
	terminal, title, text := w.render(ctx, run)
	if !terminal {
		w.waitRun(ctx, outbound, run, title, text)
		return
	}
	if err := w.sendFinal(ctx, outbound, title, text); err != nil {
		telemetry.Fail(span, "reply_failed")
		jobOutcome = "failure"
		w.failOutbound(ctx, outbound, err)
		return
	}
	_ = w.outbound.CompleteOutbound(context.WithoutCancel(ctx), outbound.ID, w.clock())
}

// render 把 Run 状态映射为卡片标题与正文；终态返回 true。
func (w *FeishuReplyWorker) render(ctx context.Context, run Run) (bool, string, string) {
	switch run.Status {
	case RunCompleted:
		message, err := w.outbound.LatestAssistantMessage(ctx, run.UserID, run.SessionID)
		if err != nil || strings.TrimSpace(message.Content) == "" {
			return true, "执行完成", "任务已执行完成，但没有生成可展示的回复。"
		}
		return true, "执行完成", message.Content
	case RunFailed:
		return true, "执行失败", "任务执行失败，请稍后重试或在 Web 端查看执行详情。"
	case RunCancelled:
		return true, "已取消", "任务已取消。"
	case RunWaitingApproval:
		return false, "等待审批", "任务正在等待人工审批，请到 Web 端处理。"
	case RunReconciliationRequired:
		return true, "需要对账", "任务需要人工对账，请在 Web 端查看执行详情。"
	default:
		return false, "正在处理", "已收到你的消息，正在执行，请稍候。"
	}
}

// waitRun 在 Run 未结束时发送或增量更新占位卡片，并延后再次检查。
func (w *FeishuReplyWorker) waitRun(ctx context.Context, outbound FeishuOutbound, run Run, title, text string) {
	if w.clock().Sub(outbound.CreatedAt) > w.config.RunWaitTimeout {
		final := "任务执行超时，请稍后在 Web 端查看结果。"
		if err := w.sendOrUpdate(ctx, outbound, "执行超时", final); err != nil {
			w.failOutbound(ctx, outbound, err)
			return
		}
		_ = w.outbound.CompleteOutbound(context.WithoutCancel(ctx), outbound.ID, w.clock())
		return
	}
	card := buildFeishuCard(title, text, w.config.MaxTextBytes)
	fingerprint := cardFingerprint(title, text, w.config.MaxTextBytes)
	if outbound.MessageID == "" {
		messageID, err := w.sendCard(ctx, outbound, card)
		if err != nil {
			// 占位卡片失败不消耗预算，直接降级为最终文本等待下一次。
			w.logger.Warn("send feishu placeholder card failed", "outbound_id", outbound.ID, "error", err)
			w.deferOutbound(ctx, outbound, w.clock().Add(w.config.PollRunInterval))
			return
		}
		if err := w.outbound.UpdateOutboundMessageID(ctx, outbound.ID, messageID, w.clock()); err != nil {
			w.logger.Warn("persist feishu card message id failed", "outbound_id", outbound.ID, "error", err)
		}
		// 首次卡片内容即视为一次更新，后续按最小间隔节流。
		if err := w.outbound.MarkOutboundCardUpdated(ctx, outbound.ID, fingerprint, w.clock()); err != nil {
			w.logger.Warn("persist feishu card fingerprint failed", "outbound_id", outbound.ID, "error", err)
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
	if err := w.sender.UpdateCard(ctx, outbound.MessageID, card); err != nil {
		// 非终态的增量更新失败只记录日志，等待下一轮或最终文本降级。
		w.logger.Warn("update feishu card failed", "outbound_id", outbound.ID, "status", run.Status, "error", err)
		w.deferOutbound(ctx, outbound, w.clock().Add(w.config.PollRunInterval))
		return
	}
	if err := w.outbound.MarkOutboundCardUpdated(ctx, outbound.ID, fingerprint, w.clock()); err != nil {
		w.logger.Warn("persist feishu card fingerprint failed", "outbound_id", outbound.ID, "error", err)
	}
	w.deferOutbound(ctx, outbound, w.clock().Add(w.config.PollRunInterval))
}

// sendFinal 发送最终结果：已有卡片则更新，否则发送新卡片；卡片失败时降级为文本。
func (w *FeishuReplyWorker) sendFinal(ctx context.Context, outbound FeishuOutbound, title, text string) error {
	return w.sendOrUpdate(ctx, outbound, title, text)
}

// sendOrUpdate 统一处理卡片更新与文本降级。
func (w *FeishuReplyWorker) sendOrUpdate(ctx context.Context, outbound FeishuOutbound, title, text string) error {
	if outbound.MessageID != "" {
		err := w.sender.UpdateCard(ctx, outbound.MessageID, buildFeishuCard(title, text, w.config.MaxTextBytes))
		if err == nil {
			// 终态卡片更新不受节流限制，但仍记录指纹与时间，便于排查重复投递。
			if markErr := w.outbound.MarkOutboundCardUpdated(ctx, outbound.ID, cardFingerprint(title, text, w.config.MaxTextBytes), w.clock()); markErr != nil {
				w.logger.Warn("persist feishu final card fingerprint failed", "outbound_id", outbound.ID, "error", markErr)
			}
			return nil
		}
		if feishu.IsRateLimited(err) {
			return err
		}
		return w.sendTextFallback(ctx, outbound, title, text)
	}
	messageID, err := w.sendCard(ctx, outbound, buildFeishuCard(title, text, w.config.MaxTextBytes))
	if err == nil {
		if saveErr := w.outbound.UpdateOutboundMessageID(ctx, outbound.ID, messageID, w.clock()); saveErr != nil {
			w.logger.Warn("persist feishu final message id failed", "outbound_id", outbound.ID, "error", saveErr)
		}
		if markErr := w.outbound.MarkOutboundCardUpdated(ctx, outbound.ID, cardFingerprint(title, text, w.config.MaxTextBytes), w.clock()); markErr != nil {
			w.logger.Warn("persist feishu final card fingerprint failed", "outbound_id", outbound.ID, "error", markErr)
		}
		return nil
	}
	if feishu.IsRateLimited(err) {
		return err
	}
	return w.sendTextFallback(ctx, outbound, title, text)
}

// sendTextFallback 在卡片发送或更新失败时发送纯文本，保证用户至少收到最终结果。
func (w *FeishuReplyWorker) sendTextFallback(ctx context.Context, outbound FeishuOutbound, title, text string) error {
	content := truncateFeishuText(title+"\n"+text, w.config.MaxTextBytes)
	_, err := w.sendText(ctx, outbound, content)
	return err
}

// failOutbound 记录失败并按飞书限流建议退避，超出预算进入死信。
func (w *FeishuReplyWorker) failOutbound(ctx context.Context, outbound FeishuOutbound, err error) {
	retryAt := w.clock().Add(feishuBackoff(outbound.Attempts))
	var apiError *feishu.APIError
	if errorsAsAPI(err, &apiError) && apiError.RetryAfter > 0 {
		retryAt = w.clock().Add(apiError.RetryAfter)
	}
	reason := feishuErrorCode(err)
	w.logger.Warn("feishu outbound failed", "outbound_id", outbound.ID, "attempts", outbound.Attempts, "error", reason)
	_ = w.outbound.FailOutbound(context.WithoutCancel(ctx), outbound.ID, reason, retryAt, w.clock())
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

// buildFeishuCard 生成可更新的飞书交互卡片，正文超限时按 UTF-8 边界截断。
func buildFeishuCard(title, text string, maxTextBytes int) map[string]any {
	text = truncateFeishuText(text, maxTextBytes)
	return map[string]any{
		"config": map[string]any{"wide_screen_mode": true, "update_multi": true},
		"header": map[string]any{
			"template": "blue",
			"title":    map[string]any{"tag": "plain_text", "content": title},
		},
		"elements": []map[string]any{
			{"tag": "div", "text": map[string]any{"tag": "lark_md", "content": text}},
		},
	}
}

// 编译期断言 RunService 实现 Run 创建端口。
var _ FeishuRunCreator = (*RunService)(nil)
