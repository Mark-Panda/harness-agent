// Package react 实现有界 ReAct 执行循环：模型调用、工具调用、循环检测与预算控制。
package react

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// 执行器错误分类，供上层决定是否可重试或终止 Run。
var (
	// ErrStepLimit 超过单 Run 最大步数。
	ErrStepLimit = errors.New("react step limit exceeded")
	// ErrToolLoop 连续重复同一工具调用达到阈值且模型未能纠正，判定为死循环。
	ErrToolLoop = errors.New("react tool loop detected")
	// ErrContentFiltered 供应商内容审核拦截了模型输出，无法继续生成。
	ErrContentFiltered = errors.New("react output blocked by content filter")
	// ErrReasoningOnly 模型只返回了推理内容而没有最终回答，不能当作空答案成功返回。
	ErrReasoningOnly = errors.New("react model returned reasoning without final answer")
	// ErrEmptyResponse 模型既没有正文也没有推理内容，空答案成功的 Run 对用户没有意义。
	ErrEmptyResponse = errors.New("react model returned an empty response")
	// ErrIncompleteToolCall 模型输出被截断，工具调用的完整性无法保证。
	ErrIncompleteToolCall = errors.New("react model returned incomplete tool calls")
	// ErrBudgetExceeded Token 或费用预算耗尽。
	ErrBudgetExceeded = errors.New("react budget exceeded")
	// ErrUnknownTool 模型调用了未注册工具。
	ErrUnknownTool = errors.New("unknown tool")
	// ErrToolTimeout 工具执行超过单次超时。
	ErrToolTimeout = errors.New("tool execution timed out")
	// ErrToolFailed 工具返回业务错误。
	ErrToolFailed = errors.New("tool execution failed")
	// ErrToolResultTooLarge 工具结果超过进入上下文的字节上限。
	ErrToolResultTooLarge = errors.New("tool result too large")
	// ErrApprovalRequired 工具调用被人工审批门禁拦截，Run 需要暂停等待决策。
	ErrApprovalRequired = errors.New("react tool approval required")
)

// ApprovalRequiredError 描述一次需要人工审批的工具调用事实。
// 上层据此持久化审批记录并把 Run 推进到 waiting_approval。
type ApprovalRequiredError struct {
	// ApprovalID 审批记录 ID，由工具包装器创建。
	ApprovalID string
	// ToolCallID 模型产生的工具调用 ID。
	ToolCallID string
	// ToolName 规范化工具名。
	ToolName string
	// RiskLevel 工具风险等级。
	RiskLevel string
}

// Error 实现错误接口；只暴露非敏感元数据。
func (e *ApprovalRequiredError) Error() string {
	return fmt.Sprintf("%s: %s (%s)", ErrApprovalRequired.Error(), e.ToolName, e.RiskLevel)
}

// Unwrap 支持 errors.Is 判定 ErrApprovalRequired。
func (e *ApprovalRequiredError) Unwrap() error { return ErrApprovalRequired }

// toolCallContextKey 是工具调用元数据在 Context 中的键类型。
type toolCallContextKey struct{}

// ToolCallInfo 是执行器注入工具实现的调用元数据。
type ToolCallInfo struct {
	// ID 工具调用 ID。
	ID string
	// Name 工具名。
	Name string
}

// WithToolCall 把当前工具调用 ID 与名称注入 Context，供审批包装器创建审批记录。
func WithToolCall(ctx context.Context, callID, name string) context.Context {
	return context.WithValue(ctx, toolCallContextKey{}, ToolCallInfo{ID: callID, Name: name})
}

// ToolCallFromContext 读取当前工具调用元数据；不存在时返回零值与 false。
func ToolCallFromContext(ctx context.Context) (ToolCallInfo, bool) {
	info, ok := ctx.Value(toolCallContextKey{}).(ToolCallInfo)
	if !ok || info.ID == "" {
		return ToolCallInfo{}, false
	}
	return info, true
}

// Role 消息角色。
type Role string

const (
	// RoleSystem 系统提示词。
	RoleSystem Role = "system"
	// RoleUser 用户输入。
	RoleUser Role = "user"
	// RoleAssistant 模型回复。
	RoleAssistant Role = "assistant"
	// RoleTool 工具结果。
	RoleTool Role = "tool"
)

// Message 是执行器内部的规范消息。
type Message struct {
	// ID 是持久化消息标识，仅用于恢复，不进入模型协议。
	ID string `json:"-"`
	// RunID 是产生该消息的执行轮次，仅用于恢复，不进入模型协议。
	RunID string `json:"-"`
	// Role 消息角色。
	Role Role `json:"role"`
	// Content 文本内容。
	Content string `json:"content,omitempty"`
	// ToolCallID 工具结果对应的调用 ID。
	ToolCallID string `json:"tool_call_id,omitempty"`
	// Name 工具名称。
	Name string `json:"name,omitempty"`
	// ToolCalls 助手消息携带的工具调用，供下一轮协议回放。
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// Ephemeral 标记仅用于本次模型续写、不得落库的中间片段。
	Ephemeral bool `json:"-"`
}

// ToolSpec 是暴露给模型的工具定义。
type ToolSpec struct {
	// Name 工具名，必须唯一。
	Name string `json:"name"`
	// Description 工具用途说明。
	Description string `json:"description"`
	// Parameters JSON Schema 参数定义。
	Parameters json.RawMessage `json:"parameters"`
}

// ToolCall 是模型请求的工具调用。
type ToolCall struct {
	// ID 调用唯一标识。
	ID string `json:"id"`
	// Name 工具名。
	Name string `json:"name"`
	// Arguments JSON 参数原文。
	Arguments json.RawMessage `json:"arguments"`
}

// Usage 是本次模型调用的 Token 用量。
type Usage struct {
	// InputTokens 输入 Token。
	InputTokens int
	// OutputTokens 输出 Token。
	OutputTokens int
}

// Completion 是模型一次调用的结果，不含隐藏思维链。
type Completion struct {
	// Content 最终文本。
	Content string
	// ReasoningContent 供应商返回的推理内容，仅用于判断空回答，不进入上下文也不持久化。
	ReasoningContent string
	// ToolCalls 工具调用列表。
	ToolCalls []ToolCall
	// FinishReason 结束原因。
	FinishReason string
	// Usage Token 用量。
	Usage Usage
	// EstimatedCost 路由层按模型价格计算的单次调用估算费用，用于 Run 级费用预算。
	EstimatedCost float64
}

// Model 抽象一次非流式模型调用。
type Model interface {
	// Complete 在超时与取消约束下执行模型调用。
	Complete(ctx context.Context, messages []Message, tools []ToolSpec) (Completion, error)
}

// StreamingModel 是可选流式能力；实现方在文本增量到达时调用 onDelta。
type StreamingModel interface {
	// CompleteStream 执行流式模型调用，并返回与非流式一致的结构化结果。
	CompleteStream(ctx context.Context, messages []Message, tools []ToolSpec, onDelta func(string)) (Completion, error)
}

// Tool 是可执行工具，参数与返回值均为 JSON。
type Tool interface {
	// Spec 返回暴露给模型的工具定义。
	Spec() ToolSpec
	// Invoke 执行工具；返回内容视为不可信数据，不得转写为系统指令。
	Invoke(ctx context.Context, arguments json.RawMessage) (json.RawMessage, error)
	// RiskLevel 返回风险等级：low/medium/high。
	RiskLevel() string
}

// Registry 是工具注册表，按名称查找。
type Registry interface {
	// Lookup 按名称查找工具。
	Lookup(name string) (Tool, bool)
	// List 返回全部工具定义，顺序稳定。
	List() []ToolSpec
}

// Limits 是 ReAct 执行的硬上限。
type Limits struct {
	// MaxSteps 单 Run 最大模型步数，默认 12。
	MaxSteps int
	// ToolTimeout 单次工具执行超时，默认 60 秒。
	ToolTimeout time.Duration
	// Deadline 单 Run 总时长，默认 5 分钟。
	Deadline time.Duration
	// MaxTotalTokens 单 Run 输入加输出的 Token 上限。
	MaxTotalTokens int
	// MaxCost 单 Run 估算费用上限，单位与价格配置一致。
	MaxCost float64
	// DuplicateToolThreshold 连续相同工具调用阈值，默认 3。
	DuplicateToolThreshold int
	// MaxToolResultBytes 单个工具结果进入上下文的字节上限，默认 64KB；超限时保留首尾裁剪后回填。
	MaxToolResultBytes int
}

// DefaultLimits 返回计划基线默认值。
func DefaultLimits() Limits {
	return Limits{
		MaxSteps: 12, ToolTimeout: 60 * time.Second, Deadline: 5 * time.Minute,
		MaxTotalTokens: 2_000_000, MaxCost: 5, DuplicateToolThreshold: 3,
		MaxToolResultBytes: 64 * 1024,
	}
}

// Normalize 补齐非正值字段，避免零值导致无界执行。
func (l Limits) Normalize() Limits {
	defaults := DefaultLimits()
	if l.MaxSteps <= 0 {
		l.MaxSteps = defaults.MaxSteps
	}
	if l.ToolTimeout <= 0 {
		l.ToolTimeout = defaults.ToolTimeout
	}
	if l.Deadline <= 0 {
		l.Deadline = defaults.Deadline
	}
	if l.MaxTotalTokens <= 0 {
		l.MaxTotalTokens = defaults.MaxTotalTokens
	}
	if l.MaxCost <= 0 {
		l.MaxCost = defaults.MaxCost
	}
	if l.DuplicateToolThreshold <= 0 {
		l.DuplicateToolThreshold = defaults.DuplicateToolThreshold
	}
	if l.MaxToolResultBytes <= 0 {
		l.MaxToolResultBytes = defaults.MaxToolResultBytes
	}
	return l
}

// Request 是一次 ReAct 执行入参。
type Request struct {
	// Messages 初始上下文（System + 历史 + 当前输入）。
	Messages []Message
	// InputPricePerMillion 每百万输入 Token 价格，用于费用估算。
	InputPricePerMillion float64
	// OutputPricePerMillion 每百万输出 Token 价格。
	OutputPricePerMillion float64
}

// StepEvent 是执行过程中的可观测事件，由上层持久化为 run_events。
type StepEvent struct {
	// Type 事件类型，与 SSE 事件名一致。
	Type string
	// Payload 事件载荷。
	Payload map[string]any
}

// EventSink 接收执行事件；实现方负责持久化与推送。
type EventSink func(event StepEvent)

// Result 是一次 ReAct 执行的最终结果。
type Result struct {
	// Content 最终回答文本。
	Content string
	// Steps 实际执行的模型步数。
	Steps int
	// TotalTokens 累计 Token。
	TotalTokens int
	// InputTokens 累计输入 Token，供上层按模式统计成本。
	InputTokens int
	// OutputTokens 累计输出 Token，供上层按模式统计成本。
	OutputTokens int
	// EstimatedCost 累计估算费用。
	EstimatedCost float64
	// Messages 完整消息序列，用于会话持久化。
	Messages []Message
	// FinalMessageID 是最终答复的持久化消息 ID；编排模式使用自定义幂等键时由上层填写。
	FinalMessageID string
}

// InvalidArgumentsError 表示工具参数未通过本地 Schema 校验。
// 执行器会把校验详情作为可纠正的工具观察结果回填模型，而不是直接终止 Run。
type InvalidArgumentsError struct {
	// Detail 是脱敏后的校验失败原因，仅包含字段路径与约束说明。
	Detail string
}

// Error 实现 error 接口。
func (e *InvalidArgumentsError) Error() string {
	if e == nil || e.Detail == "" {
		return "invalid tool arguments"
	}
	return "invalid tool arguments: " + e.Detail
}

// RunnerOption 注入执行器的可选能力，便于上层在不引入包依赖的情况下提供上下文裁剪。
type RunnerOption func(*Runner)

// WithContextBudget 在每轮模型调用前按 Token 预算裁剪送入模型的消息；
// trim 必须保留 system 提示与当前轮次，避免拆散工具调用与结果。原消息序列仍完整保留用于会话持久化。
func WithContextBudget(budget int, trim func([]Message, int) []Message) RunnerOption {
	return func(r *Runner) {
		if budget > 0 && trim != nil {
			r.contextBudget = budget
			r.trimContext = trim
		}
	}
}

// WithMaxToolsPerCall 覆盖单次模型调用下发的工具定义上限；
// 默认上限足够覆盖常见注册表规模，只有工具数量极端多时才按相关性裁剪。
func WithMaxToolsPerCall(limit int) RunnerOption {
	return func(r *Runner) {
		if limit > 0 {
			r.maxTools = limit
		}
	}
}

// defaultMaxToolsPerCall 是单次模型调用下发工具定义的默认上限。
// 取值高于常见 MCP + 内置工具注册规模，避免大量工具因截断而对模型不可见。
const defaultMaxToolsPerCall = 128

// maxParallelTools 是单步内并行执行工具调用的并发上限，避免瞬时打爆下游连接池。
const maxParallelTools = 4

// omittedToolsNoteLimit 是提示中最多列出的未下发工具名数量。
const omittedToolsNoteLimit = 300

// Runner 是 ReAct 执行器，并发安全依赖无共享可变状态。
type Runner struct {
	model         Model
	registry      Registry
	limits        Limits
	contextBudget int
	trimContext   func([]Message, int) []Message
	maxTools      int
}

// NewRunner 构造执行器；模型与注册表必填。
func NewRunner(model Model, registry Registry, limits Limits, options ...RunnerOption) (*Runner, error) {
	if model == nil || registry == nil {
		return nil, errors.New("react model and tool registry are required")
	}
	runner := &Runner{model: model, registry: registry, limits: limits.Normalize(), maxTools: defaultMaxToolsPerCall}
	for _, option := range options {
		option(runner)
	}
	return runner, nil
}

// EstimateTextTokens 以保守口径估算文本 Token：ASCII 约 4 字符/Token，
// 非 ASCII（如中文）按 1 字符/Token，避免中文输入被低估。
func EstimateTextTokens(text string) int {
	total := 0
	ascii := 0
	for _, r := range text {
		if r < 128 {
			ascii++
			continue
		}
		total++
	}
	return total + (ascii+3)/4
}

// estimateToolSpecTokens 估算工具定义的 Token 开销，用于从 Prompt 预算中预留空间。
func estimateToolSpecTokens(specs []ToolSpec) int {
	total := 0
	for _, spec := range specs {
		total += 4 + EstimateTextTokens(spec.Name) + EstimateTextTokens(spec.Description) + EstimateTextTokens(string(spec.Parameters))
	}
	return total
}

// messagesForModel 按预算裁剪上下文，并把工具结果包裹为不可信数据后送入模型。
// 持久化消息保持原始内容，避免不可信边界标签污染会话历史与前端展示。
func (r *Runner) messagesForModel(messages []Message, toolSpecs []ToolSpec) []Message {
	prepared := messages
	if r.trimContext != nil && r.contextBudget > 0 {
		budget := r.contextBudget - estimateToolSpecTokens(toolSpecs)
		// 至少保留一半预算给对话，避免大量工具定义挤占全部上下文。
		if floor := r.contextBudget / 2; budget < floor {
			budget = floor
		}
		prepared = r.trimContext(messages, budget)
	}
	wrapped := make([]Message, len(prepared))
	for index, message := range prepared {
		wrapped[index] = message
		if message.Role == RoleTool {
			wrapped[index].Content = wrapUntrustedToolOutput(message.Name, message.Content)
		}
	}
	return wrapped
}

// Run 执行有界 ReAct 循环：每步调用模型，工具结果回填后继续，直到无工具调用或触发上限。
func (r *Runner) Run(ctx context.Context, request Request, sink EventSink) (Result, error) {
	runContext, cancel := context.WithTimeout(ctx, r.limits.Deadline)
	defer cancel()

	messages := append([]Message(nil), request.Messages...)
	result := Result{Messages: messages}
	var totalTokens int
	var cost float64
	// 重复检测只统计「连续相同」的调用：紧邻的前一次调用与本次工具与参数完全一致时累计，
	// 出现不同的调用就重置。这样既能识别原地循环，也不会把合法轮询在第 3 次误拦截。
	// 历史消息不作为种子，否则重启后的第一次正常调用会被误判为重复。
	var lastCallHash string
	consecutiveCalls := 0
	continuationRequested := false
	continuationParts := make([]string, 0, 2)
	messageIndex := len(messages)
	appendMessage := func(message Message) {
		messages = append(messages, message)
		emit(sink, "message.created", messageEventPayload(messageIndex, message))
		messageIndex++
	}
	// appendEphemeral 只把续写片段留在内存中供下一次模型调用，不产生落库事件；
	// 否则半截答案会被持久化，最终答案再落库后会话里会出现重复内容。
	appendEphemeral := func(message Message) {
		message.Ephemeral = true
		messages = append(messages, message)
		messageIndex++
	}

	for step := 1; step <= r.limits.MaxSteps; step++ {
		if err := runContext.Err(); err != nil {
			return result, err
		}
		result.Steps = step
		// 步骤级事件只描述 ReAct 步，模型身份与 Attempt 事实由路由观察者写入。
		emit(sink, "step.started", map[string]any{"step": step})
		// 工具数量超过阈值时按与当前输入的词法相关性筛选，避免每步都下发全部工具定义挤占上下文。
		toolSpecs, omittedTools := r.toolsForModel(messages)
		if len(omittedTools) > 0 {
			emit(sink, "tools.selected", map[string]any{"step": step, "omitted": len(omittedTools)})
		}
		// 长任务会把历史工具结果持续带入上下文；每轮调用前按预算裁剪，避免超出模型窗口。
		// 工具定义也计入预算，先确定本轮工具集再裁剪消息。
		modelMessages := r.messagesForModel(messages, toolSpecs)
		trimmed := len(modelMessages) < len(messages)
		if note := omittedToolsNote(omittedTools); note != "" {
			modelMessages = append([]Message{{Role: RoleSystem, Content: note}}, modelMessages...)
		}
		if trimmed {
			emit(sink, "context.trimmed", map[string]any{"step": step, "dropped_messages": len(messages) - len(modelMessages) + 1})
		}
		var completion Completion
		var err error
		if streamer, ok := r.model.(StreamingModel); ok && sink != nil {
			// 流式增量按 100ms 或 512 字节合并落库，避免逐 token 写事件表。
			emitter := &deltaEmitter{sink: sink, step: step}
			completion, err = streamer.CompleteStream(runContext, modelMessages, toolSpecs, emitter.push)
			emitter.flush()
		} else {
			completion, err = r.model.Complete(runContext, modelMessages, toolSpecs)
		}
		if err != nil {
			emit(sink, "step.failed", map[string]any{"step": step, "error": sanitizeError(err)})
			return result, err
		}
		totalTokens += completion.Usage.InputTokens + completion.Usage.OutputTokens
		result.InputTokens += completion.Usage.InputTokens
		result.OutputTokens += completion.Usage.OutputTokens
		if completion.EstimatedCost > 0 {
			cost += completion.EstimatedCost
		} else {
			cost += float64(completion.Usage.InputTokens)*request.InputPricePerMillion/1_000_000 +
				float64(completion.Usage.OutputTokens)*request.OutputPricePerMillion/1_000_000
		}
		if totalTokens > r.limits.MaxTotalTokens || cost > r.limits.MaxCost {
			emit(sink, "run.failed", map[string]any{"reason": "budget_exceeded", "total_tokens": totalTokens, "estimated_cost": cost})
			return result, fmt.Errorf("%w: tokens=%d cost=%.4f", ErrBudgetExceeded, totalTokens, cost)
		}
		result.TotalTokens = totalTokens
		result.EstimatedCost = cost
		emit(sink, "step.completed", map[string]any{
			"step": step, "finish_reason": completion.FinishReason,
			"input_tokens": completion.Usage.InputTokens, "output_tokens": completion.Usage.OutputTokens,
		})

		// 工具调用被截断或内容审核拦截时不得执行：即使参数片段碰巧是合法 JSON，
		// 也无法证明调用语义完整，继续执行可能产生非预期副作用。
		if len(completion.ToolCalls) > 0 {
			switch strings.ToLower(strings.TrimSpace(completion.FinishReason)) {
			case "content_filter":
				emit(sink, "run.failed", map[string]any{"reason": "content_filter", "step": step})
				result.Messages = messages
				return result, fmt.Errorf("%w: step=%d", ErrContentFiltered, step)
			case "length":
				emit(sink, "run.failed", map[string]any{"reason": "tool_calls_truncated", "step": step})
				result.Messages = messages
				return result, fmt.Errorf("%w: step=%d", ErrIncompleteToolCall, step)
			}
		}
		// 无工具调用即得到最终回答；但必须先处理截断与内容审核两种结束原因。
		if len(completion.ToolCalls) == 0 {
			switch strings.ToLower(strings.TrimSpace(completion.FinishReason)) {
			case "content_filter":
				emit(sink, "run.failed", map[string]any{"reason": "content_filter", "step": step})
				result.Messages = messages
				return result, fmt.Errorf("%w: step=%d", ErrContentFiltered, step)
			case "length":
				partial := strings.TrimSpace(completion.Content)
				if partial != "" {
					continuationParts = append(continuationParts, partial)
				}
				// Run 结果必须包含所有续写片段，不能只返回最后一次模型调用的末段。
				result.Content = strings.Join(continuationParts, "")
				// 输出被长度截断时先请求模型续写一次，避免把半截答案直接返回用户。
				if step < r.limits.MaxSteps && !continuationRequested {
					if partial != "" {
						appendEphemeral(Message{Role: RoleAssistant, Content: partial})
					}
					emit(sink, "message.truncated", map[string]any{"step": step, "has_partial": partial != ""})
					continuationRequested = true
					messages = append(messages, Message{
						Role:    RoleUser,
						Content: "上一条回答因输出长度限制被截断。请从中断处继续，不要重复已经输出的内容。",
					})
					messageIndex = len(messages)
					result.Messages = messages
					continue
				}
				if partial == "" {
					emit(sink, "run.failed", map[string]any{"reason": "output_truncated", "step": step})
					return result, fmt.Errorf("%w: output truncated at step %d", ErrStepLimit, step)
				}
				// 收尾时只追加合并后的完整答案，message_id 因而始终指向完整结果而不是末段。
				appendMessage(Message{Role: RoleAssistant, Content: result.Content})
				emit(sink, "message.truncated", map[string]any{"step": step, "has_partial": true})
				result.Messages = messages
				emit(sink, "run.completed", map[string]any{"steps": step, "total_tokens": totalTokens, "truncated": true})
				return result, nil
			}
			if strings.TrimSpace(completion.Content) == "" {
				// 推理模型把答案留在 reasoning 字段时会表现为空回答；
				// 两者都为空同样必须显式失败，而不是静默返回一个空答案。
				if completion.ReasoningContent != "" {
					emit(sink, "run.failed", map[string]any{"reason": "reasoning_only", "step": step})
					return result, fmt.Errorf("%w: step %d", ErrReasoningOnly, step)
				}
				emit(sink, "run.failed", map[string]any{"reason": "empty_response", "step": step})
				return result, fmt.Errorf("%w: step %d", ErrEmptyResponse, step)
			}
			result.Content = completion.Content
			if len(continuationParts) > 0 {
				result.Content = strings.Join(append(continuationParts, completion.Content), "")
			}
			appendMessage(Message{Role: RoleAssistant, Content: result.Content})
			result.Messages = messages
			emit(sink, "run.completed", map[string]any{"steps": step, "total_tokens": totalTokens})
			return result, nil
		}
		appendMessage(Message{Role: RoleAssistant, Content: completion.Content, ToolCalls: completion.ToolCalls})
		// 第一遍串行完成循环检测、工具查找与 started 事件，保证事件顺序稳定；
		// 结果先写入与 tool_calls 等长的槽位，最后统一按原顺序追加，
		// 避免“被拦截的调用”和“真正执行的调用”交错导致 tool 消息乱序。
		stepSeen := make(map[string]bool, len(completion.ToolCalls))
		toolImpls := make([]Tool, len(completion.ToolCalls))
		outcomes := make([]toolOutcome, len(completion.ToolCalls))
		slots := make([]Message, len(completion.ToolCalls))
		slotSet := make([]bool, len(completion.ToolCalls))
		runnable := make([]int, 0, len(completion.ToolCalls))
		// 同批包含需要审批的工具时串行执行：既避免低风险工具在高风险工具获批前先产生副作用，
		// 也避免模型把有依赖的调用放进同一批时被并发打乱。
		serial := false
		for index, call := range completion.ToolCalls {
			hash := toolCallHash(call)
			if hash == lastCallHash {
				consecutiveCalls++
			} else {
				lastCallHash = hash
				consecutiveCalls = 1
			}
			// 同一 assistant 消息内的重复调用只执行一次，避免并行重复产生双份副作用。
			if stepSeen[hash] || consecutiveCalls >= r.limits.DuplicateToolThreshold {
				emit(sink, "tool.completed", map[string]any{"tool": call.Name, "tool_call_id": call.ID, "status": "repeated"})
				slots[index] = Message{
					Role: RoleTool, ToolCallID: call.ID, Name: call.Name,
					Content: `{"error":"repeated_tool_call","hint":"与紧邻的上一次调用参数完全相同，工具未重复执行；请总结已有结果、补充缺失信息或改用其他参数或工具。"}`,
				}
				slotSet[index] = true
				continue
			}
			stepSeen[hash] = true
			tool, exists := r.registry.Lookup(call.Name)
			if !exists {
				emit(sink, "tool.completed", map[string]any{"tool": call.Name, "status": "unknown_tool"})
				slots[index] = Message{Role: RoleTool, ToolCallID: call.ID, Name: call.Name, Content: `{"error":"unknown_tool"}`}
				slotSet[index] = true
				continue
			}
			toolImpls[index] = tool
			runnable = append(runnable, index)
			if tool.RiskLevel() != "low" {
				serial = true
			}
		}
		invoke := func(index int) {
			call := completion.ToolCalls[index]
			tool := toolImpls[index]
			// 工具实现来自内部注册表与 MCP 适配层，仍隔离 panic，避免单个工具拖垮整个 Run。
			defer func() {
				if recovered := recover(); recovered != nil {
					outcomes[index] = toolOutcome{err: fmt.Errorf("%w: %s panic: %v", ErrToolFailed, call.Name, recovered)}
				}
			}()
			// 工具可通过 Timeout() 声明自身超时；未声明或返回非正值时使用执行层默认值。
			toolTimeout := r.limits.ToolTimeout
			if provider, ok := tool.(interface{ Timeout() time.Duration }); ok {
				if custom := provider.Timeout(); custom > 0 {
					toolTimeout = custom
				}
			}
			toolContext, toolCancel := context.WithTimeout(runContext, toolTimeout)
			defer toolCancel()
			// 注入调用元数据，审批包装器据此把审批绑定到具体 tool_call_id。
			toolContext = WithToolCall(toolContext, call.ID, call.Name)
			output, toolErr := tool.Invoke(toolContext, call.Arguments)
			outcomes[index] = toolOutcome{output: output, err: toolErr}
		}
		executed := make([]int, 0, len(runnable))
		if len(runnable) > 0 {
			if serial {
				for _, index := range runnable {
					call := completion.ToolCalls[index]
					tool := toolImpls[index]
					emit(sink, "tool.started", map[string]any{"tool": call.Name, "tool_call_id": call.ID, "risk_level": tool.RiskLevel()})
					executed = append(executed, index)
					invoke(index)
					// 一旦出现审批门禁，本批后续调用一律不执行：既避免审批前产生副作用，
					// 也避免尚未决策时继续创建一串相互依赖的审批。
					if errors.Is(outcomes[index].err, ErrApprovalRequired) {
						break
					}
				}
			} else {
				for _, index := range runnable {
					call := completion.ToolCalls[index]
					tool := toolImpls[index]
					emit(sink, "tool.started", map[string]any{"tool": call.Name, "tool_call_id": call.ID, "risk_level": tool.RiskLevel()})
				}
				executed = append(executed, runnable...)
				var waitGroup sync.WaitGroup
				semaphore := make(chan struct{}, maxParallelTools)
				for _, index := range runnable {
					waitGroup.Add(1)
					go func(index int) {
						defer waitGroup.Done()
						semaphore <- struct{}{}
						defer func() { <-semaphore }()
						invoke(index)
					}(index)
				}
				waitGroup.Wait()
			}
		}
		// 第二遍按原始顺序回填槽位与完成事件，保持 tool_calls 与 tool 消息一一对应。
		var approvalErr *ApprovalRequiredError
		var approvalCause error
		for _, index := range executed {
			call := completion.ToolCalls[index]
			outcome := outcomes[index]
			if outcome.err != nil {
				var approval *ApprovalRequiredError
				if errors.As(outcome.err, &approval) {
					// 审批门禁拦截时不伪造工具结果；同一批里其他已执行的调用结果仍保留，
					// 恢复后模型无需重新调用它们。Run 由上层暂停等待决策。
					emit(sink, "tool.approval.required", map[string]any{
						"tool": call.Name, "tool_call_id": call.ID,
						"approval_id": approval.ApprovalID, "risk_level": approval.RiskLevel,
					})
					if approvalErr == nil {
						approvalErr = approval
						approvalCause = outcome.err
					}
					continue
				}
				status := "failed"
				toolErr := outcome.err
				var invalidArgs *InvalidArgumentsError
				switch {
				case errors.As(toolErr, &invalidArgs):
					status = "invalid_arguments"
				case errors.Is(toolErr, context.DeadlineExceeded):
					status = "timeout"
					toolErr = fmt.Errorf("%w: %s", ErrToolTimeout, call.Name)
				}
				detail := sanitizeError(toolErr)
				emit(sink, "tool.completed", map[string]any{"tool": call.Name, "tool_call_id": call.ID, "status": status, "error": detail})
				// 工具错误以不可信数据回填模型，由模型决定下一步，不直接终止 Run。
				// 保留脱敏后的错误详情，模型才能区分参数错误、超时与业务失败并纠正调用。
				slots[index] = Message{
					Role: RoleTool, ToolCallID: call.ID, Name: call.Name,
					Content: toolErrorObservation(status, detail, outcome.output, r.limits.MaxToolResultBytes),
				}
				slotSet[index] = true
				continue
			}
			// 工具结果超过上限时保留首尾并标注原始字节数：
			// 直接丢弃会让模型完全失去观测结果，只能反复重试同一调用。
			content, truncated := truncateToolResult(outcome.output, r.limits.MaxToolResultBytes)
			if truncated {
				emit(sink, "tool.completed", map[string]any{
					"tool": call.Name, "tool_call_id": call.ID, "status": "truncated", "original_bytes": len(outcome.output),
				})
			} else {
				emit(sink, "tool.completed", map[string]any{"tool": call.Name, "tool_call_id": call.ID, "status": "completed"})
			}
			slots[index] = Message{Role: RoleTool, ToolCallID: call.ID, Name: call.Name, Content: content}
			slotSet[index] = true
		}
		for index := range slots {
			if slotSet[index] {
				appendMessage(slots[index])
			}
		}
		result.Messages = messages
		if approvalErr != nil {
			return result, approvalCause
		}
	}
	// 步数耗尽时追加一次禁止工具的收尾调用，让模型基于已获得的工具结果生成结论，
	// 避免“最后一步刚查到数据、却没有下一轮模型调用汇总”导致整轮失败。
	finalStep := r.limits.MaxSteps + 1
	finalMessages := append(append([]Message(nil), messages...), Message{Role: RoleUser, Content: "已达到工具调用步数上限。请不要再调用任何工具，直接基于上面已获得的信息给出最终结论；若关键信息缺失，请明确说明缺少什么。"})
	emit(sink, "step.started", map[string]any{"step": finalStep, "final": true})
	// 收尾调用不携带工具定义，因此预算只需为消息本身预留。
	summaryMessages := r.messagesForModel(finalMessages, nil)
	var completion Completion
	var err error
	if streamer, ok := r.model.(StreamingModel); ok && sink != nil {
		// 收尾也走流式，避免最后一段答案在前端表现为长时间无输出。
		emitter := &deltaEmitter{sink: sink, step: finalStep}
		completion, err = streamer.CompleteStream(runContext, summaryMessages, nil, emitter.push)
		emitter.flush()
	} else {
		completion, err = r.model.Complete(runContext, summaryMessages, nil)
	}
	if err != nil {
		emit(sink, "step.failed", map[string]any{"step": finalStep, "final": true, "error": sanitizeError(err)})
		emit(sink, "run.failed", map[string]any{"reason": "step_limit", "steps": r.limits.MaxSteps})
		return result, fmt.Errorf("%w: final summary failed: %v", ErrStepLimit, err)
	}
	totalTokens += completion.Usage.InputTokens + completion.Usage.OutputTokens
	result.InputTokens += completion.Usage.InputTokens
	result.OutputTokens += completion.Usage.OutputTokens
	if completion.EstimatedCost > 0 {
		cost += completion.EstimatedCost
	} else {
		cost += float64(completion.Usage.InputTokens)*request.InputPricePerMillion/1_000_000 +
			float64(completion.Usage.OutputTokens)*request.OutputPricePerMillion/1_000_000
	}
	result.Steps = finalStep
	result.TotalTokens = totalTokens
	result.EstimatedCost = cost
	if totalTokens > r.limits.MaxTotalTokens || cost > r.limits.MaxCost {
		emit(sink, "run.failed", map[string]any{"reason": "budget_exceeded", "total_tokens": totalTokens, "estimated_cost": cost})
		return result, fmt.Errorf("%w: tokens=%d cost=%.4f", ErrBudgetExceeded, totalTokens, cost)
	}
	emit(sink, "step.completed", map[string]any{
		"step": finalStep, "final": true, "finish_reason": completion.FinishReason,
		"input_tokens": completion.Usage.InputTokens, "output_tokens": completion.Usage.OutputTokens,
	})
	switch strings.ToLower(strings.TrimSpace(completion.FinishReason)) {
	case "content_filter":
		emit(sink, "run.failed", map[string]any{"reason": "content_filter", "step": finalStep, "final": true})
		return result, fmt.Errorf("%w: final summary", ErrContentFiltered)
	case "length":
		emit(sink, "run.failed", map[string]any{"reason": "output_truncated", "step": finalStep, "final": true})
		return result, fmt.Errorf("%w: final summary truncated", ErrStepLimit)
	}
	content := strings.TrimSpace(completion.Content)
	if content == "" {
		emit(sink, "run.failed", map[string]any{"reason": "step_limit", "steps": r.limits.MaxSteps})
		return result, fmt.Errorf("%w: %d steps (final summary empty)", ErrStepLimit, r.limits.MaxSteps)
	}
	appendMessage(Message{Role: RoleAssistant, Content: content})
	result.Content = content
	result.Messages = messages
	emit(sink, "run.completed", map[string]any{"steps": finalStep, "total_tokens": totalTokens, "finalized": true})
	return result, nil
}

// deltaEmitter 合并流式文本增量，避免每个 token 都产生一条持久化事件。
type deltaEmitter struct {
	sink   EventSink
	step   int
	last   time.Time
	buffer strings.Builder
}

// push 接收一个文本增量，按时间或体积阈值触发合并落库。
func (e *deltaEmitter) push(delta string) {
	e.buffer.WriteString(delta)
	if e.last.IsZero() {
		e.last = time.Now()
	}
	if e.buffer.Len() >= 512 || time.Since(e.last) >= 100*time.Millisecond {
		e.flush()
	}
}

// flush 把缓冲区中的增量写入事件流。
func (e *deltaEmitter) flush() {
	if e.buffer.Len() == 0 {
		return
	}
	text := e.buffer.String()
	e.buffer.Reset()
	e.last = time.Now()
	emit(e.sink, "message.delta", map[string]any{"step": e.step, "delta": text})
}

// toolsForModel 返回本次调用应下发的工具定义，以及因数量限制未下发的工具名。
// 工具数量在阈值内时全量下发；超过阈值时按与整段对话的词法相关性选择，
// 既避免工具定义挤占上下文，也保留与当前任务最可能相关的工具。
func (r *Runner) toolsForModel(messages []Message) ([]ToolSpec, []string) {
	specs := r.registry.List()
	if len(specs) <= r.maxTools {
		return specs, nil
	}
	query := conversationUserContent(messages, 6, 2000)
	if query == "" {
		return specs[:r.maxTools], toolNames(specs[r.maxTools:])
	}
	type scoredSpec struct {
		index int
		score int
	}
	scored := make([]scoredSpec, 0, len(specs))
	for index, spec := range specs {
		scored = append(scored, scoredSpec{index: index, score: textOverlap(query, spec.Name+" "+spec.Description)})
	}
	sort.SliceStable(scored, func(i, j int) bool { return scored[i].score > scored[j].score })
	selected := make([]ToolSpec, 0, r.maxTools)
	selectedIndex := make(map[int]struct{}, r.maxTools)
	for _, item := range scored[:r.maxTools] {
		selected = append(selected, specs[item.index])
		selectedIndex[item.index] = struct{}{}
	}
	omitted := make([]string, 0, len(specs)-len(selected))
	for index, spec := range specs {
		if _, ok := selectedIndex[index]; !ok {
			omitted = append(omitted, spec.Name)
		}
	}
	return selected, omitted
}

// omittedToolsNote 把未下发定义的工具名告知模型：名称仍可通过 registry 调用，
// 只是没有完整 Schema，避免“必要工具被词法筛选静默隐藏”。
func omittedToolsNote(names []string) string {
	if len(names) == 0 {
		return ""
	}
	shown := names
	if len(shown) > omittedToolsNoteLimit {
		shown = shown[:omittedToolsNoteLimit]
	}
	note := "以下工具因数量限制未下发完整定义，但仍可按名称调用（参数需按工具语义推断）：" + strings.Join(shown, ", ")
	if len(names) > len(shown) {
		note += fmt.Sprintf(" 等共 %d 个", len(names))
	}
	return note
}

// toolNames 提取工具名列表。
func toolNames(specs []ToolSpec) []string {
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, spec.Name)
	}
	return names
}

// conversationUserContent 拼接最近若干条用户消息，用于工具筛选。
// 只看最后一条会漏掉多轮任务的前置约束；只看整段又会引入无关噪声，因此取近几条并做长度上限。
func conversationUserContent(messages []Message, maxMessages, maxRunes int) string {
	if maxMessages <= 0 {
		maxMessages = 1
	}
	parts := make([]string, 0, maxMessages)
	total := 0
	for index := len(messages) - 1; index >= 0 && len(parts) < maxMessages; index-- {
		if messages[index].Role != RoleUser {
			continue
		}
		content := strings.TrimSpace(messages[index].Content)
		if content == "" {
			continue
		}
		runes := []rune(content)
		if maxRunes > 0 && total+len(runes) > maxRunes {
			remaining := maxRunes - total
			if remaining <= 0 {
				break
			}
			runes = runes[:remaining]
		}
		parts = append(parts, string(runes))
		total += len(runes)
	}
	// parts 按时间倒序收集，拼接时恢复为正序，保持语义连贯。
	for left, right := 0, len(parts)-1; left < right; left, right = left+1, right-1 {
		parts[left], parts[right] = parts[right], parts[left]
	}
	return strings.Join(parts, "\n")
}

// toolOutcome 是单次工具调用的执行结果，供并行调度后按序回填。
type toolOutcome struct {
	// output 工具返回的原始 JSON。
	output json.RawMessage
	// err 工具执行错误；审批中断时携带 ApprovalRequiredError。
	err error
}

// toolErrorObservation 保留工具随错误返回的结构化 payload，供模型修正参数或理解部分失败。
// result 仅在工具确实返回合法 JSON 时附加；没有 payload 的错误保持原有 error/detail 结构。
func toolErrorObservation(status, detail string, output json.RawMessage, limit int) string {
	payload := map[string]any{"error": status, "detail": detail}
	if len(output) > 0 && json.Valid(output) {
		payload["result"] = output
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Sprintf(`{"error":%q,"detail":%q}`, status, detail)
	}
	content, _ := truncateToolResult(encoded, limit)
	return content
}

// truncateToolResult 把超限结果裁剪为首尾保留的文本并标注原始字节数。
// 直接丢弃会让模型完全失去观测结果，只能反复重试；保留首尾足以判断内容形态与关键片段。
func truncateToolResult(output []byte, limit int) (string, bool) {
	if limit <= 0 || len(output) <= limit {
		return string(output), false
	}
	head := utf8BoundaryBefore(output, limit*2/3)
	tailStart := utf8BoundaryAfter(output, len(output)-(limit-head))
	marker := fmt.Sprintf("\n[... tool result truncated: original %d bytes, kept first %d and last %d bytes. "+
		"The complete output is archived in the execution payload; if you need the omitted middle part, "+
		"retry the same tool with a narrower filter, pagination or a smaller range instead of repeating the call unchanged ...]\n",
		len(output), head, len(output)-tailStart)
	return string(output[:head]) + marker + string(output[tailStart:]), true
}

// utf8BoundaryBefore 把字节下标回退到 UTF-8 字符起始位置，避免截断多字节字符。
func utf8BoundaryBefore(data []byte, index int) int {
	if index >= len(data) {
		index = len(data) - 1
	}
	for index > 0 && !utf8.RuneStart(data[index]) {
		index--
	}
	return index
}

// utf8BoundaryAfter 把字节下标前进到 UTF-8 字符起始位置，避免截断多字节字符。
func utf8BoundaryAfter(data []byte, index int) int {
	if index < 0 {
		index = 0
	}
	for index < len(data) && !utf8.RuneStart(data[index]) {
		index++
	}
	return index
}

// textOverlap 统计查询中的相邻二字片段在文本中出现的次数，兼容中英文。
func textOverlap(query, text string) int {
	query = strings.ToLower(strings.TrimSpace(query))
	text = strings.ToLower(text)
	if query == "" || text == "" {
		return 0
	}
	runes := []rune(query)
	if len(runes) < 2 {
		if strings.Contains(text, query) {
			return 1
		}
		return 0
	}
	matches := 0
	for index := 0; index+2 <= len(runes); index++ {
		if strings.Contains(text, string(runes[index:index+2])) {
			matches++
		}
	}
	return matches
}

// wrapUntrustedToolOutput 用固定边界包裹工具返回内容，提示模型只能当作数据处理；
// 同时转义内容中伪造的关闭标签，避免外部内容逃逸数据边界伪装成系统指令。
func wrapUntrustedToolOutput(tool, output string) string {
	output = strings.NewReplacer(
		"</untrusted_tool_output>", "&lt;/untrusted_tool_output&gt;",
		"</UNTRUSTED_TOOL_OUTPUT>", "&lt;/UNTRUSTED_TOOL_OUTPUT&gt;",
	).Replace(output)
	return fmt.Sprintf("<untrusted_tool_output tool=%q>\n%s\n</untrusted_tool_output>", tool, output)
}

// toolCallHash 计算工具调用的稳定指纹，用于连续重复检测。
// 参数先做 JSON 规范化，避免键顺序或空白差异导致等价调用无法识别。
func toolCallHash(call ToolCall) string {
	normalized := strings.TrimSpace(call.Name) + "\n" + normalizeArguments(call.Arguments)
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
}

// normalizeArguments 对 JSON 参数做规范化；非法 JSON 退化为原文裁剪空格。
func normalizeArguments(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return ""
	}
	var value any
	if err := json.Unmarshal([]byte(trimmed), &value); err != nil {
		return trimmed
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return trimmed
	}
	return string(encoded)
}

// messageEventPayload 构造消息落库事件，供 Worker 幂等写入会话并推送 SSE。
func messageEventPayload(index int, message Message) map[string]any {
	payload := map[string]any{
		"index": index, "role": string(message.Role), "content": message.Content,
	}
	if message.ToolCallID != "" {
		payload["tool_call_id"] = message.ToolCallID
	}
	if message.Name != "" {
		payload["name"] = message.Name
	}
	if len(message.ToolCalls) > 0 {
		calls := make([]map[string]any, 0, len(message.ToolCalls))
		for _, call := range message.ToolCalls {
			calls = append(calls, map[string]any{
				"id": call.ID, "name": call.Name, "arguments": string(call.Arguments),
			})
		}
		payload["tool_calls"] = calls
	}
	return payload
}

// emit 安全触发事件回调，回调为空时忽略。
func emit(sink EventSink, eventType string, payload map[string]any) {
	if sink == nil {
		return
	}
	sink(StepEvent{Type: eventType, Payload: payload})
}

// sanitizeError 截断错误文本，避免超长内容进入事件与日志。
func sanitizeError(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	if len(message) > 512 {
		return strings.ToValidUTF8(message[:512], "")
	}
	return message
}
