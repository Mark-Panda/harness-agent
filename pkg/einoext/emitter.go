package einoext

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"

	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
)

// Emitter 将 Eino 模型/工具执行事实转换为现有 RunEvent。
type Emitter interface {
	// Emit 追加一个执行事件。
	Emit(eventType string, payload map[string]any)
	// NextMessageIndex 分配会话消息序号。
	NextMessageIndex() int
	// NextStep 分配 ReAct 步序号。
	NextStep() int
}

type emitterContextKey struct{}

// WithEmitter 把事件发射器注入 Eino 调用上下文。
func WithEmitter(ctx context.Context, emitter Emitter) context.Context {
	if emitter == nil {
		return ctx
	}
	return context.WithValue(ctx, emitterContextKey{}, emitter)
}

// EmitterFromContext 读取当前事件发射器。
func EmitterFromContext(ctx context.Context) (Emitter, bool) {
	emitter, ok := ctx.Value(emitterContextKey{}).(Emitter)
	return emitter, ok
}

// EmitMessage 从上下文读取发射器并追加一条消息创建事件。
func EmitMessage(ctx context.Context, index int, role, content, toolCallID, name string, toolCalls []map[string]any) {
	emitter, ok := EmitterFromContext(ctx)
	if !ok || emitter == nil {
		return
	}
	payload := map[string]any{"index": index, "role": role, "content": content}
	if toolCallID != "" {
		payload["tool_call_id"] = toolCallID
	}
	if name != "" {
		payload["name"] = name
	}
	if len(toolCalls) > 0 {
		payload["tool_calls"] = toolCalls
	}
	emitter.Emit("message.created", payload)
}

// ToolCallPayload 转换工具调用为事件载荷。
func ToolCallPayload(calls []ToolCall) []map[string]any {
	result := make([]map[string]any, 0, len(calls))
	for _, call := range calls {
		result = append(result, map[string]any{
			"id": call.ID, "name": call.Name, "arguments": call.Arguments,
		})
	}
	return result
}

// ToolCall 是 Eino 适配层使用的工具调用元数据。
type ToolCall struct {
	// ID 调用 ID。
	ID string
	// Name 工具名。
	Name string
	// Arguments JSON 参数。
	Arguments string
}

// LoopGuard 记录本次 Run 内相同工具调用的累计次数，防止模型原地循环。
// 采用按调用指纹计数的滑动语义：A/B 交替重复同样会被识别，而不仅限于连续相同调用。
type LoopGuard struct {
	mu        sync.Mutex
	counts    map[string]int
	threshold int
}

// repeatedToolCallContent 是首次达到重复阈值时回填模型的纠偏提示。
const repeatedToolCallContent = `{"error":"repeated_tool_call","hint":"相同工具调用已重复多次，工具未再执行；请总结已有结果、补充缺失信息或改用其他参数或工具。"}`

// NewLoopGuard 构造工具循环守卫。
func NewLoopGuard(threshold int) *LoopGuard {
	if threshold <= 0 {
		threshold = 3
	}
	return &LoopGuard{counts: make(map[string]int), threshold: threshold}
}

// checkToolCall 统计工具调用指纹并决定是否放行。
// 首次达到阈值返回纠偏提示，给模型一次自我修正机会；再次重复则返回 ErrToolLoop 终止 Run。
func (g *LoopGuard) checkToolCall(_ context.Context, name, arguments string) (string, error) {
	if g == nil {
		return "", nil
	}
	sum := sha256.Sum256([]byte(name + "\x00" + arguments))
	hash := hex.EncodeToString(sum[:])
	g.mu.Lock()
	if g.counts == nil {
		g.counts = make(map[string]int)
	}
	g.counts[hash]++
	count := g.counts[hash]
	g.mu.Unlock()
	if count < g.threshold {
		return "", nil
	}
	if count == g.threshold {
		return repeatedToolCallContent, nil
	}
	return "", fmt.Errorf("%w: %s repeated %d times", react.ErrToolLoop, name, count)
}

// Repeated 判断工具调用是否达到重复阈值；保留给不关心终止语义的调用方。
func (g *LoopGuard) Repeated(name, arguments string) bool {
	content, err := g.checkToolCall(context.Background(), name, arguments)
	return err != nil || content != ""
}

type loopGuardContextKey struct{}

// WithLoopGuard 注入本次 Run 的工具循环守卫。
func WithLoopGuard(ctx context.Context, guard *LoopGuard) context.Context {
	if guard == nil {
		return ctx
	}
	return context.WithValue(ctx, loopGuardContextKey{}, guard)
}

// LoopGuardFromContext 读取工具循环守卫。
func LoopGuardFromContext(ctx context.Context) *LoopGuard {
	guard, _ := ctx.Value(loopGuardContextKey{}).(*LoopGuard)
	return guard
}
