// Package react 实现有界 ReAct 执行循环：模型调用、工具调用、循环检测与预算控制。
package react

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Mark-Panda/harness-agent/pkg/metrics"
	"github.com/Mark-Panda/harness-agent/pkg/telemetry"
	"go.opentelemetry.io/otel/attribute"
)

// 执行器错误分类，供上层决定是否可重试或终止 Run。
var (
	// ErrStepLimit 超过单 Run 最大步数。
	ErrStepLimit = errors.New("react step limit exceeded")
	// ErrToolLoop 连续重复同一工具调用达到阈值，判定为死循环。
	ErrToolLoop = errors.New("react tool loop detected")
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
	// MaxToolResultBytes 单个工具结果进入上下文的字节上限，默认 64KB。
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
	// EstimatedCost 累计估算费用。
	EstimatedCost float64
	// Messages 完整消息序列，用于会话持久化。
	Messages []Message
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

// Runner 是 ReAct 执行器，并发安全依赖无共享可变状态。
type Runner struct {
	model         Model
	registry      Registry
	limits        Limits
	contextBudget int
	trimContext   func([]Message, int) []Message
}

// NewRunner 构造执行器；模型与注册表必填。
func NewRunner(model Model, registry Registry, limits Limits, options ...RunnerOption) (*Runner, error) {
	if model == nil || registry == nil {
		return nil, errors.New("react model and tool registry are required")
	}
	runner := &Runner{model: model, registry: registry, limits: limits.Normalize()}
	for _, option := range options {
		option(runner)
	}
	return runner, nil
}

// messagesForModel 返回裁剪后的模型上下文；未配置预算时复用原序列。
func (r *Runner) messagesForModel(messages []Message) []Message {
	if r.trimContext == nil || r.contextBudget <= 0 {
		return messages
	}
	return r.trimContext(messages, r.contextBudget)
}

// Run 执行有界 ReAct 循环：每步调用模型，工具结果回填后继续，直到无工具调用或触发上限。
func (r *Runner) Run(ctx context.Context, request Request, sink EventSink) (Result, error) {
	runContext, cancel := context.WithTimeout(ctx, r.limits.Deadline)
	defer cancel()

	messages := append([]Message(nil), request.Messages...)
	result := Result{Messages: messages}
	var totalTokens int
	var cost float64
	// 从历史消息恢复最后一次工具调用，使崩溃重启后仍能识别连续重复调用。
	lastToolHash, repeated := seedToolLoop(messages)
	messageIndex := len(messages)
	appendMessage := func(message Message) {
		messages = append(messages, message)
		emit(sink, "message.created", messageEventPayload(messageIndex, message))
		messageIndex++
	}

	for step := 1; step <= r.limits.MaxSteps; step++ {
		if err := runContext.Err(); err != nil {
			return result, err
		}
		result.Steps = step
		// 步骤级事件只描述 ReAct 步，模型身份与 Attempt 事实由路由观察者写入。
		emit(sink, "step.started", map[string]any{"step": step})
		// 长任务会把历史工具结果持续带入上下文；每轮调用前按预算裁剪，避免超出模型窗口。
		modelMessages := r.messagesForModel(messages)
		if len(modelMessages) < len(messages) {
			emit(sink, "context.trimmed", map[string]any{"step": step, "dropped_messages": len(messages) - len(modelMessages)})
		}
		completion, err := r.model.Complete(runContext, modelMessages, r.registry.List())
		if err != nil {
			emit(sink, "step.failed", map[string]any{"step": step, "error": sanitizeError(err)})
			return result, err
		}
		totalTokens += completion.Usage.InputTokens + completion.Usage.OutputTokens
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

		// 无工具调用即得到最终回答，结束 Run。
		if len(completion.ToolCalls) == 0 {
			result.Content = completion.Content
			appendMessage(Message{Role: RoleAssistant, Content: completion.Content})
			result.Messages = messages
			emit(sink, "run.completed", map[string]any{"steps": step, "total_tokens": totalTokens})
			return result, nil
		}
		appendMessage(Message{Role: RoleAssistant, Content: completion.Content, ToolCalls: completion.ToolCalls})
		for _, call := range completion.ToolCalls {
			hash := toolCallHash(call)
			// 连续相同调用达到阈值即判定死循环，防止模型反复触发同一副作用。
			if hash == lastToolHash {
				repeated++
			} else {
				lastToolHash, repeated = hash, 1
			}
			if repeated >= r.limits.DuplicateToolThreshold {
				emit(sink, "run.failed", map[string]any{"reason": "tool_loop", "tool": call.Name})
				return result, fmt.Errorf("%w: %s repeated %d times", ErrToolLoop, call.Name, repeated)
			}
			tool, exists := r.registry.Lookup(call.Name)
			if !exists {
				emit(sink, "tool.completed", map[string]any{"tool": call.Name, "status": "unknown_tool"})
				appendMessage(Message{Role: RoleTool, ToolCallID: call.ID, Name: call.Name, Content: `{"error":"unknown_tool"}`})
				continue
			}
			emit(sink, "tool.started", map[string]any{"tool": call.Name, "tool_call_id": call.ID, "risk_level": tool.RiskLevel()})
			// 工具可通过 Timeout() 声明自身超时；未声明或返回非正值时使用执行层默认值。
			toolTimeout := r.limits.ToolTimeout
			if provider, ok := tool.(interface{ Timeout() time.Duration }); ok {
				if custom := provider.Timeout(); custom > 0 {
					toolTimeout = custom
				}
			}
			toolContext, toolCancel := context.WithTimeout(runContext, toolTimeout)
			// 注入调用元数据，审批包装器据此把审批绑定到具体 tool_call_id。
			toolContext = WithToolCall(toolContext, call.ID, call.Name)
			// 工具 Span 只记录名称、风险等级与结果状态，不记录入参、结果或错误原文。
			toolContext, toolSpan := telemetry.Start(toolContext, "tool.invoke",
				attribute.String(telemetry.AttrToolName, call.Name),
				attribute.String(telemetry.AttrToolRiskLevel, tool.RiskLevel()),
			)
			toolStarted := time.Now()
			output, toolErr := tool.Invoke(toolContext, call.Arguments)
			elapsed := time.Since(toolStarted)
			toolCancel()
			if toolErr != nil {
				// 审批门禁拦截时立即结束本轮，不伪造工具结果；Run 由上层暂停等待决策。
				var approvalErr *ApprovalRequiredError
				if errors.As(toolErr, &approvalErr) {
					telemetry.Annotate(toolSpan, attribute.String(telemetry.AttrToolStatus, "approval_required"))
					metrics.ObserveTool(call.Name, "approval_required", elapsed)
					toolSpan.End()
					emit(sink, "tool.approval.required", map[string]any{
						"tool": call.Name, "tool_call_id": call.ID,
						"approval_id": approvalErr.ApprovalID, "risk_level": approvalErr.RiskLevel,
					})
					result.Messages = messages
					return result, toolErr
				}
				status := "failed"
				if errors.Is(toolErr, context.DeadlineExceeded) {
					status = "timeout"
					toolErr = fmt.Errorf("%w: %s", ErrToolTimeout, call.Name)
				}
				telemetry.Annotate(toolSpan, attribute.String(telemetry.AttrToolStatus, status))
				telemetry.Fail(toolSpan, status)
				metrics.ObserveTool(call.Name, status, elapsed)
				toolSpan.End()
				emit(sink, "tool.completed", map[string]any{"tool": call.Name, "tool_call_id": call.ID, "status": status, "error": sanitizeError(toolErr)})
				// 工具错误以不可信数据回填模型，由模型决定下一步，不直接终止 Run。
				appendMessage(Message{Role: RoleTool, ToolCallID: call.ID, Name: call.Name, Content: fmt.Sprintf(`{"error":%q}`, status)})
				continue
			}
			// 工具结果超过上限时拒绝进入上下文，避免挤占 Token 预算或注入超长内容。
			if len(output) > r.limits.MaxToolResultBytes {
				telemetry.Annotate(toolSpan, attribute.String(telemetry.AttrToolStatus, "too_large"))
				telemetry.Fail(toolSpan, "tool_result_too_large")
				metrics.ObserveTool(call.Name, "too_large", elapsed)
				toolSpan.End()
				emit(sink, "tool.completed", map[string]any{"tool": call.Name, "tool_call_id": call.ID, "status": "too_large"})
				appendMessage(Message{Role: RoleTool, ToolCallID: call.ID, Name: call.Name, Content: `{"error":"tool_result_too_large"}`})
				continue
			}
			telemetry.Annotate(toolSpan, attribute.String(telemetry.AttrToolStatus, "completed"))
			metrics.ObserveTool(call.Name, "completed", elapsed)
			toolSpan.End()
			emit(sink, "tool.completed", map[string]any{"tool": call.Name, "tool_call_id": call.ID, "status": "completed"})
			appendMessage(Message{Role: RoleTool, ToolCallID: call.ID, Name: call.Name, Content: string(output)})
		}
		result.Messages = messages
	}
	// 步数耗尽时追加一次禁止工具的收尾调用，让模型基于已获得的工具结果生成结论，
	// 避免“最后一步刚查到数据、却没有下一轮模型调用汇总”导致整轮失败。
	finalStep := r.limits.MaxSteps + 1
	finalMessages := append(append([]Message(nil), messages...), Message{Role: RoleUser, Content: "已达到工具调用步数上限。请不要再调用任何工具，直接基于上面已获得的信息给出最终结论；若关键信息缺失，请明确说明缺少什么。"})
	emit(sink, "step.started", map[string]any{"step": finalStep, "final": true})
	summaryMessages := r.messagesForModel(finalMessages)
	completion, err := r.model.Complete(runContext, summaryMessages, nil)
	if err != nil {
		emit(sink, "step.failed", map[string]any{"step": finalStep, "final": true, "error": sanitizeError(err)})
		emit(sink, "run.failed", map[string]any{"reason": "step_limit", "steps": r.limits.MaxSteps})
		return result, fmt.Errorf("%w: final summary failed: %v", ErrStepLimit, err)
	}
	totalTokens += completion.Usage.InputTokens + completion.Usage.OutputTokens
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

// toolCallHash 计算工具调用的稳定指纹，用于连续重复检测。
func toolCallHash(call ToolCall) string {
	normalized := strings.TrimSpace(call.Name) + "\n" + string(call.Arguments)
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
}

// seedToolLoop 从历史消息恢复最后一次工具调用指纹；没有历史调用时返回空值。
// 这样 Worker 崩溃重启后仍能沿用同一循环检测阈值，不会因内存状态丢失而无限重放。
func seedToolLoop(messages []Message) (string, int) {
	for index := len(messages) - 1; index >= 0; index-- {
		message := messages[index]
		if message.Role != RoleAssistant || len(message.ToolCalls) == 0 {
			continue
		}
		last := message.ToolCalls[len(message.ToolCalls)-1]
		return toolCallHash(last), 1
	}
	return "", 0
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
		return message[:512]
	}
	return message
}
