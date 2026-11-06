package server

import (
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// 默认 SSE 容量与写入超时；单个慢消费者不得无限占用连接与 goroutine。
const (
	defaultSSEMaxConnections = 500
	defaultSSEWriteTimeout   = 10 * time.Second
)

var (
	// sseActive 当前活跃 SSE 连接数。
	sseActive atomic.Int64
	// sseMax 连接上限。
	sseMax atomic.Int64
	// sseWriteTimeout 单次写入超时（纳秒）。
	sseWriteTimeout atomic.Int64
)

func init() {
	sseMax.Store(defaultSSEMaxConnections)
	sseWriteTimeout.Store(int64(defaultSSEWriteTimeout))
}

// SetSSELimits 配置单实例 SSE 容量与写入超时；非正值使用安全默认值。
func SetSSELimits(maxConnections int, writeTimeout time.Duration) {
	if maxConnections <= 0 {
		maxConnections = defaultSSEMaxConnections
	}
	if writeTimeout <= 0 {
		writeTimeout = defaultSSEWriteTimeout
	}
	sseMax.Store(int64(maxConnections))
	sseWriteTimeout.Store(int64(writeTimeout))
}

// RunEventStream 是 Run 事件的本地通知源；未注入时 SSE 仅依赖兜底轮询。
type RunEventStream interface {
	// Subscribe 订阅指定 Run 的新事件通知；返回信号通道与退订函数。
	Subscribe(runID uuid.UUID) (<-chan struct{}, func())
}

var runEventStreamRegistry struct {
	// mu 保护 stream，注入与读取可能发生在启动阶段与请求并发时。
	mu sync.RWMutex
	// stream 当前通知源，可为空。
	stream RunEventStream
}

// SetRunEventStream 注入 SSE 事件通知源；传入 nil 表示退回固定间隔轮询。
func SetRunEventStream(stream RunEventStream) {
	runEventStreamRegistry.mu.Lock()
	runEventStreamRegistry.stream = stream
	runEventStreamRegistry.mu.Unlock()
}

// currentRunEventStream 读取当前通知源；未注入时返回 nil。
func currentRunEventStream() RunEventStream {
	runEventStreamRegistry.mu.RLock()
	defer runEventStreamRegistry.mu.RUnlock()
	return runEventStreamRegistry.stream
}

// acquireSSEConnection 占用一个 SSE 槽位；超过上限时返回 false。
func acquireSSEConnection() (func(), bool) {
	for {
		current := sseActive.Load()
		if current >= sseMax.Load() {
			return nil, false
		}
		if sseActive.CompareAndSwap(current, current+1) {
			return func() { sseActive.Add(-1) }, true
		}
	}
}

// setSSEWriteDeadline 为下一次写入设置超时；底层不支持时忽略，仍由上下文取消兜底。
func setSSEWriteDeadline(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Duration(sseWriteTimeout.Load())))
}
