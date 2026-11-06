package einoext

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/Mark-Panda/harness-agent/pkg/agent/model"
	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// usageState 在 WithTools 产生的副本之间共享调用用量。
type usageState struct {
	calls        atomic.Int64
	inputTokens  atomic.Int64
	outputTokens atomic.Int64
	costBits     atomic.Uint64
	lastMu       sync.Mutex
	last         string
}

// ChatModel 把现有模型路由适配为 Eino ToolCallingChatModel。
// 路由层继续负责候选切换、重试、熔断、Attempt 持久化与预算。
type ChatModel struct {
	router    *model.Router
	specs     []react.ToolSpec
	emitter   Emitter
	state     *usageState
	maxCalls  int
	maxTokens int
	maxCost   float64
}

// NewChatModel 构造 Eino 模型适配器；maxCalls、maxTokens、maxCost 为零时对应限制关闭。
func NewChatModel(router *model.Router, emitter Emitter, maxCalls, maxTokens int, maxCost float64) *ChatModel {
	return &ChatModel{
		router: router, emitter: emitter, state: &usageState{},
		maxCalls: maxCalls, maxTokens: maxTokens, maxCost: maxCost,
	}
}

// WithTools 返回绑定工具定义的不可变副本，符合 Eino 并发安全约定。
func (m *ChatModel) WithTools(tools []*schema.ToolInfo) (einomodel.ToolCallingChatModel, error) {
	specs := make([]react.ToolSpec, 0, len(tools))
	for _, info := range tools {
		spec, err := ToolInfoToSpec(info)
		if err != nil {
			return nil, err
		}
		specs = append(specs, spec)
	}
	return &ChatModel{
		router: m.router, specs: specs, emitter: m.emitter, state: m.state,
		maxCalls: m.maxCalls, maxTokens: m.maxTokens, maxCost: m.maxCost,
	}, nil
}

// stepLimitInstruction 指导模型在步数耗尽后基于已有观测给出最终结论。
// 收尾调用不携带工具定义，避免模型继续请求无法执行的工具。
const stepLimitInstruction = "已达到工具调用步数上限。请不要再调用任何工具，直接基于上面已获得的信息给出最终结论；若关键信息缺失，请明确说明缺少什么。"

// continuationInstruction 要求模型在输出被长度截断后从中断处继续，不要重复已有内容。
const continuationInstruction = "上一条回答因输出长度限制被截断。请从中断处继续，不要重复已经输出的内容，也不要调用工具。"

// Generate 执行一次非流式模型调用并记录 Attempt 用量与消息事件。
func (m *ChatModel) Generate(ctx context.Context, input []*schema.Message, _ ...einomodel.Option) (*schema.Message, error) {
	if m.stepLimitReached() {
		return m.summarize(ctx, input)
	}
	step := m.beginStep(ctx)
	completion, err := m.router.Complete(ctx, ToReactMessages(input), m.specs)
	m.finishStep(ctx, step, completion, err)
	if err != nil {
		return nil, err
	}
	if reasonErr := completionError(completion); reasonErr != nil {
		return nil, reasonErr
	}
	// 输出被长度截断时先请求一次续写，避免把半截答案直接返回用户。
	if isLengthFinish(completion) && len(completion.ToolCalls) == 0 {
		completion, err = m.continueCompletion(ctx, ToReactMessages(input), completion)
		if err != nil {
			return nil, err
		}
	}
	m.setLastContent(completion.Content)
	if budgetErr := m.emitAssistant(ctx, completion); budgetErr != nil {
		return nil, budgetErr
	}
	return completionToMessage(completion), nil
}

// Stream 执行一次流式模型调用；文本增量通过 StreamReader 交给 Eino，
// 完整助手消息在流结束后以 message.created 事件持久化。
func (m *ChatModel) Stream(ctx context.Context, input []*schema.Message, _ ...einomodel.Option) (*schema.StreamReader[*schema.Message], error) {
	if m.stepLimitReached() {
		return m.summarizeStream(ctx, input), nil
	}
	step := m.beginStep(ctx)
	reader, writer := schema.Pipe[*schema.Message](16)
	go func() {
		defer writer.Close()
		messages := ToReactMessages(input)
		var content strings.Builder
		completion, err := m.router.CompleteStream(ctx, messages, m.specs, func(delta string) {
			if delta == "" {
				return
			}
			content.WriteString(delta)
			if m.emitter != nil {
				m.emitter.Emit("message.delta", map[string]any{"step": step, "delta": delta})
			}
			writer.Send(&schema.Message{Role: schema.Assistant, Content: delta}, nil)
		})
		m.finishStep(ctx, step, completion, err)
		if err != nil {
			writer.Send(nil, err)
			return
		}
		// 内容审核与工具调用截断必须在执行工具前终止，避免产生非预期副作用。
		if reasonErr := completionError(completion); reasonErr != nil {
			writer.Send(nil, reasonErr)
			return
		}
		// 输出被长度截断时续写一次，把两段文本合并成同一条助手消息。
		if isLengthFinish(completion) && len(completion.ToolCalls) == 0 {
			continued, contErr := m.continueStream(ctx, messages, step, completion, writer)
			if contErr != nil {
				writer.Send(nil, contErr)
				return
			}
			completion = continued
		}
		// 预算耗尽必须在发送工具调用帧之前终止，避免 Eino 继续执行工具产生副作用。
		m.setLastContent(completion.Content)
		if budgetErr := m.emitAssistant(ctx, completion); budgetErr != nil {
			writer.Send(nil, budgetErr)
			return
		}
		// 最后一帧只携带工具调用与结束原因，正文已通过增量帧发送，避免重复拼接。
		final := completionToMessage(completion)
		final.Content = ""
		writer.Send(final, nil)
	}()
	return reader, nil
}

// Usage 返回模型调用累计用量。
func (m *ChatModel) Usage() (calls int, inputTokens, outputTokens int, estimatedCost float64) {
	if m == nil || m.state == nil {
		return 0, 0, 0, 0
	}
	return int(m.state.calls.Load()), int(m.state.inputTokens.Load()),
		int(m.state.outputTokens.Load()), math.Float64frombits(m.state.costBits.Load())
}

// LastContent 返回最近一次模型响应的完整正文，供 Eino 流式输出为空时兜底。
func (m *ChatModel) LastContent() string {
	if m == nil || m.state == nil {
		return ""
	}
	m.state.lastMu.Lock()
	defer m.state.lastMu.Unlock()
	return m.state.last
}

// stepLimitReached 判断是否已用尽普通模型调用步数；达到上限后转为收尾调用。
func (m *ChatModel) stepLimitReached() bool {
	return m.maxCalls > 0 && int(m.state.calls.Load()) >= m.maxCalls
}

// summarize 在步数耗尽后执行一次不带工具的收尾调用，返回最终结论文本。
func (m *ChatModel) summarize(ctx context.Context, input []*schema.Message) (*schema.Message, error) {
	step := m.beginFinalStep(ctx)
	completion, err := m.router.Complete(ctx, m.summaryMessages(input), nil)
	m.finishStep(ctx, step, completion, err)
	if err != nil {
		return nil, fmt.Errorf("%w: final summary failed: %v", react.ErrStepLimit, err)
	}
	// 收尾答案被截断或审核拦截时不能当作有效结论返回。
	if reasonErr := completionError(completion); reasonErr != nil {
		return nil, reasonErr
	}
	if isLengthFinish(completion) {
		return nil, fmt.Errorf("%w: final summary truncated", react.ErrStepLimit)
	}
	m.setLastContent(completion.Content)
	if budgetErr := m.emitAssistant(ctx, completion); budgetErr != nil {
		return nil, budgetErr
	}
	return completionToMessage(completion), nil
}

// summarizeStream 以流式方式执行步数耗尽后的收尾调用。
func (m *ChatModel) summarizeStream(ctx context.Context, input []*schema.Message) *schema.StreamReader[*schema.Message] {
	reader, writer := schema.Pipe[*schema.Message](16)
	go func() {
		defer writer.Close()
		step := m.beginFinalStep(ctx)
		var content strings.Builder
		completion, err := m.router.CompleteStream(ctx, m.summaryMessages(input), nil, func(delta string) {
			if delta == "" {
				return
			}
			content.WriteString(delta)
			if m.emitter != nil {
				m.emitter.Emit("message.delta", map[string]any{"step": step, "delta": delta, "final": true})
			}
			writer.Send(&schema.Message{Role: schema.Assistant, Content: delta}, nil)
		})
		m.finishStep(ctx, step, completion, err)
		if err != nil {
			writer.Send(nil, fmt.Errorf("%w: final summary failed: %v", react.ErrStepLimit, err))
			return
		}
		if reasonErr := completionError(completion); reasonErr != nil {
			writer.Send(nil, reasonErr)
			return
		}
		if isLengthFinish(completion) {
			writer.Send(nil, fmt.Errorf("%w: final summary truncated", react.ErrStepLimit))
			return
		}
		m.setLastContent(completion.Content)
		if budgetErr := m.emitAssistant(ctx, completion); budgetErr != nil {
			writer.Send(nil, budgetErr)
			return
		}
		final := completionToMessage(completion)
		final.Content = ""
		writer.Send(final, nil)
	}()
	return reader
}

// summaryMessages 在原始上下文后追加禁止继续调用工具的收尾指令。
func (m *ChatModel) summaryMessages(input []*schema.Message) []react.Message {
	messages := ToReactMessages(input)
	return append(messages, react.Message{Role: react.RoleUser, Content: stepLimitInstruction})
}

// beginFinalStep 为收尾调用分配独立步序号并标注 final，便于前端展示。
func (m *ChatModel) beginFinalStep(ctx context.Context) int {
	if m.emitter == nil {
		return 0
	}
	step := m.emitter.NextStep()
	m.emitter.Emit("step.started", map[string]any{"step": step, "final": true})
	return step
}

// continueCompletion 在输出被长度截断后请求一次非流式续写，并合并两次调用用量。
func (m *ChatModel) continueCompletion(ctx context.Context, messages []react.Message, partial react.Completion) (react.Completion, error) {
	if err := m.continuationAllowed(partial); err != nil {
		return partial, err
	}
	continued, err := m.router.Complete(ctx, m.continuationMessages(messages, partial), nil)
	if err != nil {
		return partial, fmt.Errorf("%w: continuation failed: %v", react.ErrStepLimit, err)
	}
	return m.mergeContinuation(partial, continued)
}

// continueStream 以流式方式续写截断答案，增量直接写入同一个 StreamReader。
func (m *ChatModel) continueStream(ctx context.Context, messages []react.Message, step int, partial react.Completion, writer *schema.StreamWriter[*schema.Message]) (react.Completion, error) {
	if err := m.continuationAllowed(partial); err != nil {
		return partial, err
	}
	if m.emitter != nil {
		m.emitter.Emit("message.truncated", map[string]any{"step": step, "has_partial": true})
	}
	continued, err := m.router.CompleteStream(ctx, m.continuationMessages(messages, partial), nil, func(delta string) {
		if delta == "" {
			return
		}
		if m.emitter != nil {
			m.emitter.Emit("message.delta", map[string]any{"step": step, "delta": delta, "continuation": true})
		}
		writer.Send(&schema.Message{Role: schema.Assistant, Content: delta}, nil)
	})
	if err != nil {
		return partial, fmt.Errorf("%w: continuation failed: %v", react.ErrStepLimit, err)
	}
	return m.mergeContinuation(partial, continued)
}

// continuationAllowed 校验续写不会突破步数上限且存在可续写的半截内容。
func (m *ChatModel) continuationAllowed(partial react.Completion) error {
	if strings.TrimSpace(partial.Content) == "" {
		return fmt.Errorf("%w: output truncated", react.ErrStepLimit)
	}
	if m.maxCalls > 0 && int(m.state.calls.Load())+1 >= m.maxCalls {
		return fmt.Errorf("%w: continuation would exceed step limit", react.ErrStepLimit)
	}
	return nil
}

// continuationMessages 把半截回答与续写指令追加到原上下文后。
func (m *ChatModel) continuationMessages(messages []react.Message, partial react.Completion) []react.Message {
	result := make([]react.Message, 0, len(messages)+2)
	result = append(result, messages...)
	result = append(result,
		react.Message{Role: react.RoleAssistant, Content: partial.Content},
		react.Message{Role: react.RoleUser, Content: continuationInstruction},
	)
	return result
}

// mergeContinuation 合并续写结果与用量；第二次调用同样计入步数。
func (m *ChatModel) mergeContinuation(partial, continued react.Completion) (react.Completion, error) {
	if len(continued.ToolCalls) > 0 {
		return partial, fmt.Errorf("%w: continuation returned tool calls", react.ErrIncompleteToolCall)
	}
	m.state.calls.Add(1)
	partial.Content += continued.Content
	partial.Usage.InputTokens += continued.Usage.InputTokens
	partial.Usage.OutputTokens += continued.Usage.OutputTokens
	partial.EstimatedCost += continued.EstimatedCost
	partial.FinishReason = continued.FinishReason
	if isLengthFinish(continued) {
		return partial, fmt.Errorf("%w: continuation truncated", react.ErrStepLimit)
	}
	return partial, nil
}

// isLengthFinish 判断供应商是否因输出长度截断而结束。
func isLengthFinish(completion react.Completion) bool {
	return strings.EqualFold(strings.TrimSpace(completion.FinishReason), "length")
}

func (m *ChatModel) setLastContent(content string) {
	if m == nil || m.state == nil {
		return
	}
	m.state.lastMu.Lock()
	m.state.last = content
	m.state.lastMu.Unlock()
}

func (m *ChatModel) beginStep(ctx context.Context) int {
	if m.emitter == nil {
		return 0
	}
	step := m.emitter.NextStep()
	m.emitter.Emit("step.started", map[string]any{"step": step})
	return step
}

func (m *ChatModel) finishStep(ctx context.Context, step int, completion react.Completion, err error) {
	if m.emitter == nil {
		return
	}
	if err != nil {
		m.emitter.Emit("step.failed", map[string]any{"step": step, "error": errorText(err)})
		return
	}
	m.emitter.Emit("step.completed", map[string]any{
		"step": step, "finish_reason": completion.FinishReason,
		"input_tokens": completion.Usage.InputTokens, "output_tokens": completion.Usage.OutputTokens,
	})
}

// emitAssistant 累计用量、检查 Token/费用预算并发出助手消息事件。
// 预算超限时返回错误，调用方必须在执行下一轮工具调用前终止。
func (m *ChatModel) emitAssistant(ctx context.Context, completion react.Completion) error {
	if m.state != nil {
		m.state.calls.Add(1)
		m.state.inputTokens.Add(int64(completion.Usage.InputTokens))
		m.state.outputTokens.Add(int64(completion.Usage.OutputTokens))
		addCost(m.state, completion.EstimatedCost)
	}
	if m.budgetExceeded() {
		inputTokens, outputTokens, cost := m.state.inputTokens.Load(), m.state.outputTokens.Load(), math.Float64frombits(m.state.costBits.Load())
		return fmt.Errorf("%w: tokens=%d cost=%.4f", react.ErrBudgetExceeded, inputTokens+outputTokens, cost)
	}
	emitter, ok := EmitterFromContext(ctx)
	if !ok || emitter == nil {
		return nil
	}
	calls := make([]ToolCall, 0, len(completion.ToolCalls))
	for _, call := range completion.ToolCalls {
		calls = append(calls, ToolCall{ID: call.ID, Name: call.Name, Arguments: string(call.Arguments)})
	}
	EmitMessage(ctx, emitter.NextMessageIndex(), string(react.RoleAssistant),
		completion.Content, "", "", ToolCallPayload(calls))
	return nil
}

// budgetExceeded 判断累计 Token 或估算费用是否超过配置上限。
func (m *ChatModel) budgetExceeded() bool {
	if m == nil || m.state == nil {
		return false
	}
	if m.maxTokens > 0 && m.state.inputTokens.Load()+m.state.outputTokens.Load() > int64(m.maxTokens) {
		return true
	}
	if m.maxCost > 0 && math.Float64frombits(m.state.costBits.Load()) > m.maxCost {
		return true
	}
	return false
}

func completionToMessage(completion react.Completion) *schema.Message {
	message := &schema.Message{
		Role: schema.Assistant, Content: completion.Content,
		ResponseMeta: &schema.ResponseMeta{
			FinishReason: completion.FinishReason,
			Usage: &schema.TokenUsage{
				PromptTokens: completion.Usage.InputTokens, CompletionTokens: completion.Usage.OutputTokens,
			},
		},
	}
	for index, call := range completion.ToolCalls {
		position := index
		message.ToolCalls = append(message.ToolCalls, schema.ToolCall{
			Index: &position, ID: call.ID, Type: "function",
			Function: schema.FunctionCall{Name: call.Name, Arguments: string(call.Arguments)},
		})
	}
	return message
}

// completionError 把供应商结束原因转换为统一执行错误。
// 内容审核与工具调用截断不能继续执行工具，否则可能产生非预期副作用。
func completionError(completion react.Completion) error {
	reason := strings.ToLower(strings.TrimSpace(completion.FinishReason))
	switch reason {
	case "content_filter":
		return fmt.Errorf("%w: finish_reason=content_filter", react.ErrContentFiltered)
	case "length":
		if len(completion.ToolCalls) > 0 {
			return fmt.Errorf("%w: tool calls truncated", react.ErrIncompleteToolCall)
		}
	}
	return nil
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	if len(text) > 256 {
		text = truncateUTF8(text, 256)
	}
	return text
}

// IsApprovalRequired 兼容 Eino 错误包装后仍能识别审批门禁。
func IsApprovalRequired(err error) bool {
	return errors.Is(err, react.ErrApprovalRequired)
}

// addCost 用 CAS 累加浮点费用，避免锁开销。
func addCost(state *usageState, value float64) {
	if state == nil || value == 0 {
		return
	}
	for {
		oldBits := state.costBits.Load()
		next := math.Float64frombits(oldBits) + value
		if state.costBits.CompareAndSwap(oldBits, math.Float64bits(next)) {
			return
		}
	}
}

// validateToolSpecs 保留给测试与后续图编排使用。
func validateToolSpecs(specs []react.ToolSpec) error {
	for _, spec := range specs {
		if strings.TrimSpace(spec.Name) == "" {
			return fmt.Errorf("tool spec name is empty")
		}
	}
	return nil
}
