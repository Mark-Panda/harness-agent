package server

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

// immediateRunEventStream 在订阅时立即发出一次通知，用于验证通知驱动路径。
type immediateRunEventStream struct{}

// Subscribe 返回已就绪的通知通道。
func (immediateRunEventStream) Subscribe(uuid.UUID) (<-chan struct{}, func()) {
	channel := make(chan struct{}, 1)
	channel <- struct{}{}
	return channel, func() {}
}

// TestRunEventStreamRegistry 验证通知源的注入与清除。
func TestRunEventStreamRegistry(t *testing.T) {
	SetRunEventStream(immediateRunEventStream{})
	if currentRunEventStream() == nil {
		t.Fatal("event stream was not installed")
	}
	SetRunEventStream(nil)
	if currentRunEventStream() != nil {
		t.Fatal("event stream was not cleared")
	}
}

// failingWriter 模拟慢消费者/断开连接后的写入失败。
type failingWriter struct{ header http.Header }

// Header 返回响应头。
func (f failingWriter) Header() http.Header { return f.header }

// WriteHeader 忽略状态码。
func (f failingWriter) WriteHeader(int) {}

// Write 始终返回错误，模拟写超时或连接断开。
func (f failingWriter) Write([]byte) (int, error) { return 0, errors.New("write timeout") }

// TestSSEConnectionCapacityAndRelease 验证连接上限生效且释放后可重新占用。
func TestSSEConnectionCapacityAndRelease(t *testing.T) {
	SetSSELimits(2, time.Second)
	t.Cleanup(func() { SetSSELimits(defaultSSEMaxConnections, defaultSSEWriteTimeout) })
	first, ok := acquireSSEConnection()
	if !ok {
		t.Fatal("first SSE slot rejected")
	}
	second, ok := acquireSSEConnection()
	if !ok {
		t.Fatal("second SSE slot rejected")
	}
	if _, ok := acquireSSEConnection(); ok {
		t.Fatal("third SSE slot must exceed capacity")
	}
	first()
	released, ok := acquireSSEConnection()
	if !ok {
		t.Fatal("slot was not released")
	}
	released()
	second()
}

// TestWriteSSEDetectsSlowConsumer 验证写入失败时返回 false，调用方停止推送。
func TestWriteSSEDetectsSlowConsumer(t *testing.T) {
	if writeSSE(failingWriter{header: http.Header{}}, 1, "run.completed", map[string]any{"status": "ok"}) {
		t.Fatal("write failure must be reported")
	}
}

// TestSafeSSEEventTypeRejectsHeaderInjection 验证非法事件名不会注入 SSE 帧字段。
func TestSafeSSEEventTypeRejectsHeaderInjection(t *testing.T) {
	for _, value := range []string{"run.completed\nid: 1", "event: evil", "数据", ""} {
		if got := safeSSEEventType(value); got != "message" {
			t.Fatalf("safeSSEEventType(%q) = %q, want message", value, got)
		}
	}
	if got := safeSSEEventType("run.completed"); got != "run.completed" {
		t.Fatalf("valid event type = %q", got)
	}
}
