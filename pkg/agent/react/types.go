// Package react 定义 Agent 执行层共享的消息、工具、预算与错误类型。
// 生产执行路径由 pkg/einoext 编排，本包不再包含 ReAct 循环实现。
package react

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
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
