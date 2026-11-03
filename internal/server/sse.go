package server

import (
	"net/http"
	"sync/atomic"
	"time"
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
