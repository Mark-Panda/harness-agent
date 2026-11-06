package model

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"time"

	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
)

// 路由层错误，供 Worker 映射稳定的失败码。
var (
	// ErrNoCandidates 表示没有任何候选通过硬过滤或全部处于熔断状态。
	ErrNoCandidates = errors.New("no model candidate available")
	// ErrAttemptLimit 表示 Run 的总 Attempt 预算已耗尽。
	ErrAttemptLimit = errors.New("model attempt limit exceeded")
	// ErrSwitchLimit 表示模型切换次数已达到策略上限。
	ErrSwitchLimit = errors.New("model switch limit exceeded")
)

// Candidate 是 Run 固化快照中的单个可路由模型及其连接信息。
// BaseURL 与 APIKey 仅在 Worker 内存中短暂存在，禁止写入事件、日志或数据库。
type Candidate struct {
	// Priority 策略中的显式优先级，数值越小越优先，调用方须先按升序排序。
	Priority int
	// Ref 模型元数据快照，用于能力、窗口与熔断隔离。
	Ref ModelRef
	// BaseURL 供应商 OpenAI-compatible 基础地址。
	BaseURL string
	// APIKey 解密后的供应商密钥，仅用于构造 HTTP 客户端。
	APIKey string
	// InputPricePerMillion 每百万输入 Token 价格。
	InputPricePerMillion float64
	// OutputPricePerMillion 每百万输出 Token 价格。
	OutputPricePerMillion float64
	// DataRegion 数据区域，用于合规审计展示。
	DataRegion string
}

// HealthStore 是跨副本共享的熔断与半开探测状态存储。
// 实现方通常使用 Redis；读取失败时调用方按静态候选顺序降级，但绝不放宽硬准入。
type HealthStore interface {
	// Allow 判断当前是否允许向该模型发起调用；半开状态下仅放行有限探测。
	Allow(ctx context.Context, providerID, modelID string) (bool, error)
	// RecordSuccess 记录一次成功，关闭熔断并清理失败计数。
	RecordSuccess(ctx context.Context, providerID, modelID string) error
	// RecordFailure 记录一次失败，达到阈值后打开熔断。
	RecordFailure(ctx context.Context, providerID, modelID string) error
}

// AttemptRecord 是一次模型调用的持久化事实，对应 model_calls 表。
type AttemptRecord struct {
	// AttemptNo Run 内单调递增的尝试序号，从 1 开始。
	AttemptNo int
	// ModelID 本次尝试使用的模型。
	ModelID string
	// ProviderID 供应商 ID，熔断按供应商与模型组合隔离。
	ProviderID string
	// RemoteModelName 供应商侧模型名，仅用于排障展示。
	RemoteModelName string
	// StartedAt 调用开始时间（UTC）。
	StartedAt time.Time
	// Duration 调用耗时，用于延迟监控。
	Duration time.Duration
	// Status 尝试状态：succeeded/failed/cancelled。
	Status string
	// FinishReason 模型结束原因。
	FinishReason string
	// ProviderRequestID 供应商请求 ID，用于对账与排障。
	ProviderRequestID string
	// InputTokens 输入 Token 数。
	InputTokens int
	// OutputTokens 输出 Token 数。
	OutputTokens int
	// EstimatedCost 估算费用，单位与价格配置一致。
	EstimatedCost float64
	// RequestHash 规范化请求的 SHA-256 指纹，用于检测重复请求。
	RequestHash []byte
	// MessageCount 本次请求的上下文消息数，用于脱敏请求清单。
	MessageCount int
	// ToolCount 本次请求暴露的工具数。
	ToolCount int
	// ToolNames 本次请求暴露的工具名列表，仅用于脱敏请求清单重建。
	ToolNames []string
	// ResponseText 成功响应的文本，供按策略脱敏后持久化。
	ResponseText string
	// ErrorClass 错误分类，成功时为空。
	ErrorClass string
	// ErrorMessage 脱敏后的错误摘要，禁止包含凭证或用户正文。
	ErrorMessage string
	// SwitchReason 切换到下一候选的受控原因，仅由路由层填写。
	SwitchReason string
}

// SwitchRecord 描述一次受控模型切换，供执行树与审计展示原模型和目标模型。
type SwitchRecord struct {
	// FromModelID 原模型 ID。
	FromModelID string
	// ToModelID 目标模型 ID。
	ToModelID string
	// FromRemoteName 原模型供应商侧名称。
	FromRemoteName string
	// ToRemoteName 目标模型供应商侧名称。
	ToRemoteName string
	// Reason 脱敏后的切换原因，如 retryable_failure、rate_limited、auth_failed、context_window。
	Reason string
}

// AttemptObserver 接收 Attempt 生命周期与切换事实；实现方负责数据库持久化与事件追加。
type AttemptObserver interface {
	// AttemptStarted 在调用前持久化请求清单与请求指纹；返回错误时不得发起模型调用。
	AttemptStarted(ctx context.Context, record AttemptRecord) error
	// AttemptFinished 在调用结束后持久化结果、Token、费用与错误分类。
	AttemptFinished(ctx context.Context, record AttemptRecord) error
	// ModelSwitched 在候选切换时记录原因并追加 model.switched 事件。
	ModelSwitched(ctx context.Context, record SwitchRecord) error
}

// RouterConfig 是单 Run 的路由上限与退避参数，来源为 YAML 配置。
type RouterConfig struct {
	// MinContextTokens 生成类模型的上下文硬准入下限，必须大于等于 1M。
	MinContextTokens int64
	// MaxAttemptsPerModel 同一模型的最大连续尝试次数。
	MaxAttemptsPerModel int
	// MaxAttemptsPerRun 单 Run 的总尝试上限，包含历史已落库尝试。
	MaxAttemptsPerRun int
	// MaxSwitchesPerRun 单 Run 的最大模型切换次数。
	MaxSwitchesPerRun int
	// InitialAttemptNo 本次执行开始前已落库的尝试序号，用于崩溃恢复后继续单调递增。
	InitialAttemptNo int
	// MaxOutputTokens 单次调用输出上限；为零时使用候选模型的上限。
	MaxOutputTokens int
	// RequestTimeout 单次模型调用的 HTTP 超时。
	RequestTimeout time.Duration
	// BackoffBase 可重试错误的基础退避时长。
	BackoffBase time.Duration
	// MaxBackoff 指数退避上限。
	MaxBackoff time.Duration
	// MaxRetryAfter 可接受的供应商 Retry-After 上限；超过则直接切换或失败。
	MaxRetryAfter time.Duration
	// Temperature 采样温度。
	Temperature float64
	// Limiter 供应商级并发舱壁，可为空表示不限制并发。
	Limiter ConcurrencyLimiter
}

// normalize 补齐零值默认，避免错误配置导致无界重试或零超时。
func (c RouterConfig) normalize() RouterConfig {
	if c.MinContextTokens < 1_000_000 {
		c.MinContextTokens = 1_000_000
	}
	if c.MaxAttemptsPerModel <= 0 {
		c.MaxAttemptsPerModel = 2
	}
	if c.MaxAttemptsPerRun < c.MaxAttemptsPerModel {
		c.MaxAttemptsPerRun = 6
	}
	if c.MaxSwitchesPerRun < 0 {
		c.MaxSwitchesPerRun = 2
	}
	if c.InitialAttemptNo < 0 {
		c.InitialAttemptNo = 0
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 120 * time.Second
	}
	if c.BackoffBase <= 0 {
		c.BackoffBase = 200 * time.Millisecond
	}
	if c.MaxBackoff < c.BackoffBase {
		c.MaxBackoff = 2 * time.Second
	}
	if c.MaxRetryAfter <= 0 {
		c.MaxRetryAfter = 30 * time.Second
	}
	return c
}

// RouterOption 用于注入测试友好的时间与退避实现。
type RouterOption func(*Router)

// WithSleep 覆写等待实现，测试可立即返回以验证重试与切换规则。
func WithSleep(sleep func(context.Context, time.Duration) error) RouterOption {
	return func(r *Router) {
		if sleep != nil {
			r.sleep = sleep
		}
	}
}

// WithJitter 覆写抖动实现，测试可注入确定性函数。
func WithJitter(jitter func(time.Duration) time.Duration) RouterOption {
	return func(r *Router) {
		if jitter != nil {
			r.jitter = jitter
		}
	}
}

// Router 实现 react.Model：在一次 ReAct 执行内保持模型粘性，并按确定性规则重试与切换。
// 同一个 Router 实例不跨 Run 复用，内部状态只服务于单次执行。
type Router struct {
	candidates []Candidate
	health     HealthStore
	config     RouterConfig
	observer   AttemptObserver
	clients    map[string]*Client
	current    int
	attempts   int
	total      int
	switches   int
	excluded   map[int]string
	sleep      func(context.Context, time.Duration) error
	jitter     func(time.Duration) time.Duration
}

// NewRouter 构造路由模型；候选按显式优先级排序，至少需要一个候选。
func NewRouter(candidates []Candidate, health HealthStore, config RouterConfig, observer AttemptObserver, options ...RouterOption) (*Router, error) {
	if len(candidates) == 0 {
		return nil, fmt.Errorf("%w: candidates are required", ErrNoCandidates)
	}
	for _, candidate := range candidates {
		if candidate.Ref.ProviderID == "" || candidate.Ref.ModelID == "" || candidate.BaseURL == "" || candidate.APIKey == "" {
			return nil, errors.New("router candidate requires provider, model, base URL and API key")
		}
	}
	router := &Router{
		candidates: candidates, health: health, config: config.normalize(), observer: observer,
		clients: make(map[string]*Client, len(candidates)), current: -1, excluded: make(map[int]string),
		sleep:  func(ctx context.Context, duration time.Duration) error { return sleepWithContext(ctx, duration) },
		jitter: defaultJitter,
	}
	for _, option := range options {
		option(router)
	}
	return router, nil
}

// Complete 实现 react.Model：执行一次带重试与受控切换的模型调用。
func (r *Router) Complete(ctx context.Context, messages []react.Message, tools []react.ToolSpec) (react.Completion, error) {
	return r.complete(ctx, messages, tools, nil)
}

// CompleteStream 实现 react.StreamingModel：以 SSE 流式执行并在增量到达时回调 onDelta。
// 一旦已经向调用方输出过增量，失败后不再重试或切换候选，避免重复文本与工具调用。
func (r *Router) CompleteStream(ctx context.Context, messages []react.Message, tools []react.ToolSpec, onDelta func(string)) (react.Completion, error) {
	if onDelta == nil {
		return r.complete(ctx, messages, tools, nil)
	}
	return r.complete(ctx, messages, tools, onDelta)
}

// complete 是流式与非流式调用的共享路由实现。
func (r *Router) complete(ctx context.Context, messages []react.Message, tools []react.ToolSpec, onDelta func(string)) (react.Completion, error) {
	chatMessages := toChatMessages(messages)
	definitions := toToolDefinitions(tools)
	inputTokens := EstimateRequestTokens(chatMessages, definitions)
	requiresTools := len(definitions) > 0
	requestHash := hashRequest(chatMessages, definitions)
	var lastErr error

	for {
		if err := ctx.Err(); err != nil {
			return react.Completion{}, err
		}
		// 总尝试预算按历史已落库序号累计，崩溃恢复后仍然受限。
		if r.config.InitialAttemptNo+r.total >= r.config.MaxAttemptsPerRun {
			return react.Completion{}, newAttemptLimitError(lastErr)
		}
		if r.current < 0 {
			index := r.pick(ctx, inputTokens, requiresTools, -1)
			if index < 0 {
				return react.Completion{}, r.noCandidateError(lastErr)
			}
			r.current, r.attempts = index, 0
		}
		candidate := r.candidates[r.current]
		outputBudget := r.outputBudget(candidate)

		client, err := r.client(candidate)
		if err != nil {
			return react.Completion{}, err
		}
		started := time.Now()
		record := AttemptRecord{
			AttemptNo: r.config.InitialAttemptNo + r.total + 1,
			ModelID:   candidate.Ref.ModelID, ProviderID: candidate.Ref.ProviderID,
			RemoteModelName: candidate.Ref.RemoteModelName, StartedAt: started,
			Status: "running", RequestHash: requestHash, InputTokens: int(inputTokens),
			MessageCount: len(chatMessages), ToolCount: len(definitions), ToolNames: toolNames(definitions),
		}
		// 调用前先落库请求清单；持久化失败时禁止发起调用，避免出现无审计的模型请求。
		if err := r.started(ctx, record); err != nil {
			return react.Completion{}, err
		}
		// 供应商级舱壁：等待槽位期间受 Run Deadline 约束，避免超卖供应商配额。
		release := func() {}
		if r.config.Limiter != nil {
			acquired, acquireErr := r.config.Limiter.Acquire(ctx, "provider:"+candidate.Ref.ProviderID)
			if acquireErr != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return react.Completion{}, ctxErr
				}
				// Redis 故障时允许降级放行，避免并发控制在缓存故障时阻断全部请求。
			} else if acquired != nil {
				release = acquired
			}
		}
		chatRequest := ChatRequest{
			Model: candidate.Ref, Messages: chatMessages, Tools: definitions,
			Temperature: r.config.Temperature, MaxOutputTokens: outputBudget,
		}
		streamed := false
		var response ChatResponse
		var callErr error
		if onDelta != nil {
			response, callErr = client.ChatStream(ctx, chatRequest, func(delta string) {
				if delta == "" {
					return
				}
				streamed = true
				onDelta(delta)
			})
		} else {
			response, callErr = client.Chat(ctx, chatRequest)
		}
		release()
		record.Duration = time.Since(started)
		r.total++
		r.attempts++

		if callErr == nil {
			record.Status = "succeeded"
			record.FinishReason = response.FinishReason
			record.ProviderRequestID = response.ProviderRequestID
			record.InputTokens = response.InputTokens
			record.OutputTokens = response.OutputTokens
			record.ResponseText = response.Content
			record.EstimatedCost = estimateCost(candidate, response.InputTokens, response.OutputTokens)
			// attempts 只统计当前模型连续失败的尝试次数；成功即结束本轮失败序列，
			// 否则 ReAct 多轮调用会把历史成功次数累加，导致一次瞬时 429 被误判为重试超限。
			r.attempts = 0
			// 健康状态写入失败不应抹掉已成功的模型结果，仅影响后续熔断统计。
			r.recordSuccess(ctx, candidate)
			if err := r.finished(ctx, record); err != nil {
				return react.Completion{}, err
			}
			return react.Completion{
				Content: response.Content, FinishReason: response.FinishReason,
				Usage:         react.Usage{InputTokens: response.InputTokens, OutputTokens: response.OutputTokens},
				EstimatedCost: record.EstimatedCost,
				ToolCalls:     toReactToolCalls(response.ToolCalls),
			}, nil
		}

		// 用户取消与 Deadline 是控制流错误，不进入重试/切换决策。
		if ctxErr := ctx.Err(); ctxErr != nil {
			record.Status = "cancelled"
			record.ErrorClass = classOf(ctxErr)
			record.ErrorMessage = sanitizedMessage(ctxErr)
			_ = r.finished(ctx, record)
			return react.Completion{}, ctxErr
		}
		record.Status = "failed"
		record.ErrorClass = ClassifyError(callErr)
		record.ErrorMessage = sanitizedMessage(callErr)
		// 已经向用户输出过增量后不能再重试或切换：重放会产生重复文本，
		// 工具调用增量也可能已被上游消费。此处直接失败，由 Run 终态明确告知用户。
		if streamed {
			if err := r.finished(ctx, record); err != nil {
				return react.Completion{}, err
			}
			return react.Completion{}, callErr
		}
		// 仅健康类故障计入熔断；400、内容审核与上下文超限属于请求问题，不能污染模型健康状态。
		if record.ErrorClass == "retryable_failure" || record.ErrorClass == "rate_limited" ||
			record.ErrorClass == "authentication_failed" || record.ErrorClass == "timeout" || record.ErrorClass == "unknown" {
			r.recordFailure(ctx, candidate)
		}
		action, wait, reason := r.decide(callErr, candidate, inputTokens, requiresTools)
		// 切换原因随 Attempt 事实一并落库，执行树无需从后续事件反推。
		if action == routeSwitch {
			record.SwitchReason = reason
		}
		if err := r.finished(ctx, record); err != nil {
			return react.Completion{}, err
		}
		lastErr = callErr

		switch action {
		case routeFail:
			return react.Completion{}, callErr
		case routeRetry:
			if deadline, ok := ctx.Deadline(); ok && time.Now().Add(wait).After(deadline) {
				return react.Completion{}, callErr
			}
			if err := r.sleep(ctx, wait); err != nil {
				return react.Completion{}, err
			}
		case routeSwitch:
			if r.switches >= r.config.MaxSwitchesPerRun {
				return react.Completion{}, newSwitchLimitError(lastErr)
			}
			index := r.pick(ctx, inputTokens, requiresTools, r.current)
			if index < 0 {
				// 没有可切换候选时区分两种原因：总尝试预算耗尽，或候选已被硬过滤/熔断排除。
				if r.config.InitialAttemptNo+r.total >= r.config.MaxAttemptsPerRun {
					return react.Completion{}, newAttemptLimitError(lastErr)
				}
				return react.Completion{}, r.noCandidateError(lastErr)
			}
			if err := r.notifySwitch(ctx, candidate, r.candidates[index], reason); err != nil {
				return react.Completion{}, err
			}
			r.excluded[r.current] = reason
			r.switches++
			r.current, r.attempts = index, 0
		}
	}
}

// 路由动作枚举，决定失败后是终止、等待重试还是切换候选。
type routeAction int

const (
	// routeFail 终止执行并保留原始错误。
	routeFail routeAction = iota
	// routeRetry 在同一模型上等待后重试。
	routeRetry
	// routeSwitch 切换到下一个健康候选。
	routeSwitch
)

// decide 根据错误分类、尝试次数与上下文适配返回确定性路由动作。
func (r *Router) decide(err error, candidate Candidate, inputTokens int64, requiresTools bool) (routeAction, time.Duration, string) {
	var responseErr *ResponseError
	_ = errors.As(err, &responseErr)
	switch {
	case errors.Is(err, ErrContextExceeded):
		if r.hasLargerCandidate(inputTokens, requiresTools) {
			return routeSwitch, 0, "context_window"
		}
		return routeFail, 0, ""
	case errors.Is(err, ErrRateLimited):
		wait := r.config.BackoffBase
		if responseErr != nil && responseErr.RetryAfter > 0 {
			wait = responseErr.RetryAfter
		}
		// Retry-After 超出可接受预算时不再等待，直接切换健康候选。
		if wait > r.config.MaxRetryAfter {
			return routeSwitch, 0, "rate_limited"
		}
		if r.attempts >= r.config.MaxAttemptsPerModel {
			return routeSwitch, 0, "rate_limited"
		}
		return routeRetry, wait, "rate_limited"
	case errors.Is(err, ErrRetryable):
		if r.attempts >= r.config.MaxAttemptsPerModel {
			return routeSwitch, 0, "retryable_failure"
		}
		return routeRetry, r.backoff(r.attempts), "retryable_failure"
	case errors.Is(err, ErrAuthentication):
		// 当前 Endpoint 凭证失效：熔断由失败计数完成，随后立即切换其他合规供应商。
		return routeSwitch, 0, "auth_failed"
	case errors.Is(err, ErrCapability):
		return routeSwitch, 0, "capability_mismatch"
	case errors.Is(err, ErrPermanent):
		// 400、内容审核拒绝等错误禁止通过切换模型绕过安全与格式校验。
		return routeFail, 0, ""
	default:
		return routeFail, 0, ""
	}
}

// pick 按优先级选择首个通过硬过滤且未熔断的候选；after 用于切换时跳过当前项。
func (r *Router) pick(ctx context.Context, inputTokens int64, requiresTools bool, after int) int {
	for index, candidate := range r.candidates {
		if index == after {
			continue
		}
		if _, excluded := r.excluded[index]; excluded {
			continue
		}
		if !r.fits(candidate, inputTokens, requiresTools) {
			continue
		}
		if r.health != nil {
			allowed, err := r.health.Allow(ctx, candidate.Ref.ProviderID, candidate.Ref.ModelID)
			// 健康缓存不可用时退化为静态候选顺序，但仍保留 1M+ 与能力硬过滤。
			if err == nil && !allowed {
				r.excluded[index] = "circuit_open"
				continue
			}
		}
		return index
	}
	return -1
}

// fits 判断候选是否满足 1M+、能力与输入适配；excluded 仅表示临时熔断，不影响硬准入。
func (r *Router) fits(candidate Candidate, inputTokens int64, requiresTools bool) bool {
	if candidate.Ref.ContextWindowTokens < r.config.MinContextTokens {
		return false
	}
	if requiresTools && !candidate.Ref.Tools {
		return false
	}
	return inputTokens+int64(r.outputBudget(candidate)) <= candidate.Ref.ContextWindowTokens
}

// hasLargerCandidate 判断除当前候选外是否还有能容纳完整请求的候选，避免以切换绕过上下文限制。
func (r *Router) hasLargerCandidate(inputTokens int64, requiresTools bool) bool {
	for index := range r.candidates {
		if index == r.current {
			continue
		}
		if _, excluded := r.excluded[index]; excluded {
			continue
		}
		if r.fits(r.candidates[index], inputTokens, requiresTools) {
			return true
		}
	}
	return false
}

// outputBudget 计算本候选的单次输出预算，受模型上限与策略上限双重约束。
func (r *Router) outputBudget(candidate Candidate) int {
	budget := candidate.Ref.MaxOutputTokens
	if budget <= 0 {
		budget = 4096
	}
	if r.config.MaxOutputTokens > 0 && r.config.MaxOutputTokens < budget {
		budget = r.config.MaxOutputTokens
	}
	return budget
}

// client 懒加载并缓存供应商客户端；同一 Run 内复用连接避免重复凭证解密。
func (r *Router) client(candidate Candidate) (*Client, error) {
	if client, exists := r.clients[candidate.Ref.ModelID]; exists {
		return client, nil
	}
	client, err := NewClient(candidate.BaseURL, candidate.APIKey, r.config.RequestTimeout)
	if err != nil {
		return nil, fmt.Errorf("build model client for %s: %w", candidate.Ref.ModelID, err)
	}
	r.clients[candidate.Ref.ModelID] = client
	return client, nil
}

// started 通知观察者 Attempt 开始，观察者为空时视为无需持久化。
func (r *Router) started(ctx context.Context, record AttemptRecord) error {
	if r.observer == nil {
		return nil
	}
	if err := r.observer.AttemptStarted(ctx, record); err != nil {
		return fmt.Errorf("persist model attempt %d: %w", record.AttemptNo, err)
	}
	return nil
}

// finished 通知观察者 Attempt 结束。
func (r *Router) finished(ctx context.Context, record AttemptRecord) error {
	if r.observer == nil {
		return nil
	}
	if err := r.observer.AttemptFinished(ctx, record); err != nil {
		return fmt.Errorf("persist model attempt %d result: %w", record.AttemptNo, err)
	}
	return nil
}

// notifySwitch 记录切换事实并追加 model.switched 事件。
func (r *Router) notifySwitch(ctx context.Context, from, to Candidate, reason string) error {
	if r.observer == nil {
		return nil
	}
	record := SwitchRecord{
		FromModelID: from.Ref.ModelID, ToModelID: to.Ref.ModelID,
		FromRemoteName: from.Ref.RemoteModelName, ToRemoteName: to.Ref.RemoteModelName, Reason: reason,
	}
	if err := r.observer.ModelSwitched(ctx, record); err != nil {
		return fmt.Errorf("persist model switch: %w", err)
	}
	return nil
}

// recordSuccess 关闭熔断状态；存储故障只影响统计，不影响调用结果。
func (r *Router) recordSuccess(ctx context.Context, candidate Candidate) {
	if r.health != nil {
		_ = r.health.RecordSuccess(ctx, candidate.Ref.ProviderID, candidate.Ref.ModelID)
	}
}

// recordFailure 累加失败计数，达到阈值后由 HealthStore 打开熔断。
func (r *Router) recordFailure(ctx context.Context, candidate Candidate) {
	if r.health != nil {
		_ = r.health.RecordFailure(ctx, candidate.Ref.ProviderID, candidate.Ref.ModelID)
	}
}

// noCandidateError 构造带诊断原因的无候选错误，保留最后一次模型失败供排障。
func (r *Router) noCandidateError(lastErr error) error {
	if lastErr != nil {
		return fmt.Errorf("%w: candidates exhausted after %v", ErrNoCandidates, lastErr)
	}
	return fmt.Errorf("%w: no candidate satisfies 1M+, capability, context or circuit rules", ErrNoCandidates)
}

// attemptLimitError 是总尝试预算耗尽的结构化错误，保留底层分类供失败码映射。
type attemptLimitError struct {
	// last 最后一次模型调用错误，可能为空。
	last error
}

// Error 返回预算耗尽与底层原因。
func (e *attemptLimitError) Error() string {
	if e.last == nil {
		return ErrAttemptLimit.Error()
	}
	return fmt.Sprintf("%s: %v", ErrAttemptLimit, e.last)
}

// Unwrap 暴露最后一次模型错误。
func (e *attemptLimitError) Unwrap() error { return e.last }

// Is 允许调用方使用 errors.Is 判断预算耗尽。
func (e *attemptLimitError) Is(target error) bool { return target == ErrAttemptLimit }

// newAttemptLimitError 构造尝试预算耗尽错误。
func newAttemptLimitError(last error) error { return &attemptLimitError{last: last} }

// switchLimitError 是切换预算耗尽的结构化错误。
type switchLimitError struct {
	// last 最后一次模型调用错误。
	last error
}

// Error 返回切换上限与底层原因。
func (e *switchLimitError) Error() string {
	if e.last == nil {
		return ErrSwitchLimit.Error()
	}
	return fmt.Sprintf("%s: %v", ErrSwitchLimit, e.last)
}

// Unwrap 暴露最后一次模型错误。
func (e *switchLimitError) Unwrap() error { return e.last }

// Is 允许调用方使用 errors.Is 判断切换上限。
func (e *switchLimitError) Is(target error) bool { return target == ErrSwitchLimit }

// newSwitchLimitError 构造切换预算耗尽错误。
func newSwitchLimitError(last error) error { return &switchLimitError{last: last} }

// ClassifyError 返回稳定的错误分类字符串，供 model_calls 与失败码使用。
func ClassifyError(err error) string {
	var netErr net.Error
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	// http.Client.Timeout 触发的是 net.Error 超时，而不是 context.DeadlineExceeded；
	// 单独识别可让慢供应商在指标与执行树中显示为 timeout。
	case errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	case errors.Is(err, ErrContextExceeded):
		return "context_exceeded"
	case errors.Is(err, ErrRateLimited):
		return "rate_limited"
	case errors.Is(err, ErrAuthentication):
		return "authentication_failed"
	case errors.Is(err, ErrCapability):
		return "capability_mismatch"
	case errors.Is(err, ErrRetryable):
		return "retryable_failure"
	case errors.Is(err, ErrPermanent):
		return "request_rejected"
	case errors.Is(err, ErrNoCandidates), errors.Is(err, ErrAttemptLimit), errors.Is(err, ErrSwitchLimit):
		return "routing_exhausted"
	default:
		return "unknown"
	}
}

// classOf 返回控制流错误的分类，用于取消场景的 Attempt 记录。
func classOf(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "cancelled"
}

// sanitizedMessage 截断错误文本并移除可能的换行注入，避免事件载荷污染。
func sanitizedMessage(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	if len(message) > 512 {
		message = message[:512]
	}
	return message
}

// estimateCost 按模型价格估算单次调用费用。
func estimateCost(candidate Candidate, inputTokens, outputTokens int) float64 {
	return float64(inputTokens)*candidate.InputPricePerMillion/1_000_000 +
		float64(outputTokens)*candidate.OutputPricePerMillion/1_000_000
}

// hashRequest 计算规范化请求的 SHA-256 指纹，用于重复请求检测与审计对账。
func hashRequest(messages []ChatMessage, tools []ToolDefinition) []byte {
	payload := struct {
		Messages []ChatMessage    `json:"messages"`
		Tools    []ToolDefinition `json:"tools"`
	}{Messages: messages, Tools: tools}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	digest := sha256.Sum256(encoded)
	return digest[:]
}

// toChatMessages 把执行器消息转换为协议消息，工具调用原样回放。
func toChatMessages(messages []react.Message) []ChatMessage {
	converted := make([]ChatMessage, 0, len(messages))
	for _, message := range messages {
		item := ChatMessage{Role: string(message.Role), Content: message.Content, ToolCallID: message.ToolCallID}
		for _, call := range message.ToolCalls {
			item.ToolCalls = append(item.ToolCalls, ChatToolCall{
				ID: call.ID, Type: "function", Function: ChatFunction{Name: call.Name, Arguments: string(call.Arguments)},
			})
		}
		converted = append(converted, item)
	}
	return converted
}

// toToolDefinitions 把执行器工具定义转换为协议定义，空 Schema 使用合法对象占位。
func toToolDefinitions(tools []react.ToolSpec) []ToolDefinition {
	converted := make([]ToolDefinition, 0, len(tools))
	for _, tool := range tools {
		parameters := tool.Parameters
		if len(parameters) == 0 {
			parameters = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		converted = append(converted, ToolDefinition{Name: tool.Name, Description: tool.Description, Parameters: parameters})
	}
	return converted
}

// toolNames 提取请求暴露的工具名，用于脱敏请求清单重建，不包含参数与描述。
func toolNames(definitions []ToolDefinition) []string {
	names := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		names = append(names, definition.Name)
	}
	return names
}

// toReactToolCalls 把协议工具调用转换为执行器表示，空参数归一为 JSON 对象。
func toReactToolCalls(calls []ToolCall) []react.ToolCall {
	converted := make([]react.ToolCall, 0, len(calls))
	for _, call := range calls {
		arguments := json.RawMessage(call.Arguments)
		if len(arguments) == 0 {
			arguments = json.RawMessage(`{}`)
		}
		converted = append(converted, react.ToolCall{ID: call.ID, Name: call.Name, Arguments: arguments})
	}
	return converted
}

// backoff 计算指数退避并叠加抖动，避免多副本同时重试造成风暴。
func (r *Router) backoff(attempt int) time.Duration {
	duration := r.config.BackoffBase
	for i := 1; i < attempt; i++ {
		duration *= 2
		if duration >= r.config.MaxBackoff {
			duration = r.config.MaxBackoff
			break
		}
	}
	if r.jitter == nil {
		return duration
	}
	return r.jitter(duration)
}

// defaultJitter 在基准时长上下浮动 20%，降低同步重试概率。
func defaultJitter(duration time.Duration) time.Duration {
	if duration <= 0 {
		return 0
	}
	delta := float64(duration) * 0.2
	return duration + time.Duration((rand.Float64()*2-1)*delta) // #nosec G404 -- 仅用于重试退避抖动，不用于安全随机
}

// sleepWithContext 在可取消等待中执行退避，取消时立即返回。
func sleepWithContext(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		return nil
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
