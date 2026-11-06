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
	router   *model.Router
	specs    []react.ToolSpec
	emitter  Emitter
	state    *usageState
	maxCalls int
}

// NewChatModel 构造 Eino 模型适配器；maxCalls 为零时不限制模型调用次数。
func NewChatModel(router *model.Router, emitter Emitter, maxCalls int) *ChatModel {
	return &ChatModel{router: router, emitter: emitter, state: &usageState{}, maxCalls: maxCalls}
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
	return &ChatModel{router: m.router, specs: specs, emitter: m.emitter, state: m.state, maxCalls: m.maxCalls}, nil
}

// Generate 执行一次非流式模型调用并记录 Attempt 用量与消息事件。
func (m *ChatModel) Generate(ctx context.Context, input []*schema.Message, _ ...einomodel.Option) (*schema.Message, error) {
	if err := m.checkStepLimit(); err != nil {
		return nil, err
	}
	step := m.beginStep(ctx)
	completion, err := m.router.Complete(ctx, ToReactMessages(input), m.specs)
	m.finishStep(ctx, step, completion, err)
	if err != nil {
		return nil, err
	}
	m.setLastContent(completion.Content)
	m.emitAssistant(ctx, completion)
	return completionToMessage(completion), nil
}

// Stream 执行一次流式模型调用；文本增量通过 StreamReader 交给 Eino，
// 完整助手消息在流结束后以 message.created 事件持久化。
func (m *ChatModel) Stream(ctx context.Context, input []*schema.Message, _ ...einomodel.Option) (*schema.StreamReader[*schema.Message], error) {
	if err := m.checkStepLimit(); err != nil {
		return nil, err
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
		// 最后一帧只携带工具调用与结束原因，正文已通过增量帧发送，避免重复拼接。
		final := completionToMessage(completion)
		final.Content = ""
		writer.Send(final, nil)
		m.setLastContent(completion.Content)
		m.emitAssistant(ctx, completion)
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

func (m *ChatModel) checkStepLimit() error {
	if m.maxCalls > 0 && int(m.state.calls.Load()) >= m.maxCalls {
		return fmt.Errorf("%w: %d calls", react.ErrStepLimit, m.maxCalls)
	}
	return nil
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

func (m *ChatModel) emitAssistant(ctx context.Context, completion react.Completion) {
	if m.state != nil {
		m.state.calls.Add(1)
		m.state.inputTokens.Add(int64(completion.Usage.InputTokens))
		m.state.outputTokens.Add(int64(completion.Usage.OutputTokens))
		addCost(m.state, completion.EstimatedCost)
	}
	emitter, ok := EmitterFromContext(ctx)
	if !ok || emitter == nil {
		return
	}
	calls := make([]ToolCall, 0, len(completion.ToolCalls))
	for _, call := range completion.ToolCalls {
		calls = append(calls, ToolCall{ID: call.ID, Name: call.Name, Arguments: string(call.Arguments)})
	}
	EmitMessage(ctx, emitter.NextMessageIndex(), string(react.RoleAssistant),
		completion.Content, "", "", ToolCallPayload(calls))
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

func errorText(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	if len(text) > 256 {
		text = text[:256]
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
