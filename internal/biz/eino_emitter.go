package biz

import (
	"encoding/json"
	"sync"
	"sync/atomic"

	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
	"github.com/Mark-Panda/harness-agent/pkg/einoext"
	"github.com/google/uuid"
)

// einoWorkerEmitter 把 Eino 适配层事件写回现有 Run 事件通道。
type einoWorkerEmitter struct {
	sink                react.EventSink
	runID               uuid.UUID
	initialMessageIndex int
	messageIndex        atomic.Int64
	stepIndex           atomic.Int64
	lastMu              sync.Mutex
	lastMessageID       string
}

// Emit 追加一个执行事件。
func (e *einoWorkerEmitter) Emit(eventType string, payload map[string]any) {
	if e == nil || e.sink == nil {
		return
	}
	e.sink(react.StepEvent{Type: eventType, Payload: payload})
}

// NextMessageIndex 分配会话消息序号。
func (e *einoWorkerEmitter) NextMessageIndex() int {
	index := e.initialMessageIndex + int(e.messageIndex.Add(1)) - 1
	if e.runID != uuid.Nil {
		e.lastMu.Lock()
		e.lastMessageID = deterministicMessageID(e.runID, index).String()
		e.lastMu.Unlock()
	}
	return index
}

// LastMessageID 返回最近分配消息的持久化 ID，用于 Run 完成事件关联。
func (e *einoWorkerEmitter) LastMessageID() string {
	e.lastMu.Lock()
	defer e.lastMu.Unlock()
	return e.lastMessageID
}

// NextStep 分配 ReAct 模型步序号。
func (e *einoWorkerEmitter) NextStep() int {
	return int(e.stepIndex.Add(1))
}

// recordingEmitter 记录 Eino 产生的助手与工具消息，供子会话按顺序持久化。
// Worker 的增量事件只推送 message.delta，完整消息必须先记录下来再统一落库。
type recordingEmitter struct {
	inner    einoext.Emitter
	mu       sync.Mutex
	messages []react.Message
}

// newRecordingEmitter 包装事件发射器并开始记录消息。
func newRecordingEmitter(inner einoext.Emitter) *recordingEmitter {
	return &recordingEmitter{inner: inner}
}

// Emit 记录消息事件后转发给底层发射器。
func (e *recordingEmitter) Emit(eventType string, payload map[string]any) {
	if eventType == "message.created" {
		if message, ok := messageFromPayload(payload); ok {
			e.mu.Lock()
			e.messages = append(e.messages, message)
			e.mu.Unlock()
		}
	}
	if e.inner != nil {
		e.inner.Emit(eventType, payload)
	}
}

// NextMessageIndex 透传消息序号分配。
func (e *recordingEmitter) NextMessageIndex() int {
	if e.inner == nil {
		return 0
	}
	return e.inner.NextMessageIndex()
}

// NextStep 透传模型步序号分配。
func (e *recordingEmitter) NextStep() int {
	if e.inner == nil {
		return 0
	}
	return e.inner.NextStep()
}

// Messages 返回已记录的助手与工具消息快照。
func (e *recordingEmitter) Messages() []react.Message {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]react.Message(nil), e.messages...)
}

// messageFromPayload 把 message.created 事件转换为内部消息结构。
func messageFromPayload(payload map[string]any) (react.Message, bool) {
	role := stringValue(payload["role"])
	if role != string(react.RoleAssistant) && role != string(react.RoleTool) {
		return react.Message{}, false
	}
	message := react.Message{
		Role: react.Role(role), Content: stringValue(payload["content"]),
		ToolCallID: stringValue(payload["tool_call_id"]), Name: stringValue(payload["name"]),
	}
	calls, _ := payload["tool_calls"].([]map[string]any)
	for _, call := range calls {
		message.ToolCalls = append(message.ToolCalls, react.ToolCall{
			ID: stringValue(call["id"]), Name: stringValue(call["name"]),
			Arguments: json.RawMessage(stringValue(call["arguments"])),
		})
	}
	return message, true
}

// stringValue 安全读取事件载荷中的字符串字段。
func stringValue(value any) string {
	text, _ := value.(string)
	return text
}
