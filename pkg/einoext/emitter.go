package einoext

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
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

// LoopGuard 记录连续相同工具调用，防止模型原地循环。
type LoopGuard struct {
	mu          sync.Mutex
	lastHash    string
	consecutive int
	threshold   int
}

// NewLoopGuard 构造工具循环守卫。
func NewLoopGuard(threshold int) *LoopGuard {
	if threshold <= 0 {
		threshold = 3
	}
	return &LoopGuard{threshold: threshold}
}

// Repeated 判断工具调用是否达到重复阈值。
func (g *LoopGuard) Repeated(name, arguments string) bool {
	if g == nil {
		return false
	}
	sum := sha256.Sum256([]byte(name + "\x00" + arguments))
	hash := hex.EncodeToString(sum[:])
	g.mu.Lock()
	defer g.mu.Unlock()
	if hash == g.lastHash {
		g.consecutive++
	} else {
		g.lastHash = hash
		g.consecutive = 1
	}
	return g.consecutive >= g.threshold
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
