package biz

import (
	"sync"
	"sync/atomic"

	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
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
