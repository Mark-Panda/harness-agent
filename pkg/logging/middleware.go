package logging

import (
	"net/http"
)

// headerRequestID 是请求关联 ID 的标准响应头与入站提取头。
const headerRequestID = "X-Request-ID"

// RequestIDMiddleware 为入站 HTTP 请求建立或复用 request_id，并回写响应头。
// 关键约束：只记录方法与路径，不记录查询串、请求体、响应体或认证头。
func RequestIDMiddleware(next http.Handler) http.Handler {
	if next == nil {
		return http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := normalizeRequestID(r.Header.Get(headerRequestID))
		if requestID == "" {
			requestID = NewRequestID()
		}
		ctx := WithRequestID(r.Context(), requestID)
		w.Header().Set(headerRequestID, requestID)

		writer := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(writer, r.WithContext(ctx))
	})
}

// statusWriter 记录响应状态码并透传 Flush，保证 SSE 流式响应不被中间件破坏。
type statusWriter struct {
	http.ResponseWriter
	status int
}

// WriteHeader 记录首次写入的状态码。
func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Write 在未显式写头时按 200 记录，并透传响应体。
func (w *statusWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(body)
}

// Flush 透传底层 Flush，保证 SSE 心跳与增量事件实时下发。
func (w *statusWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Unwrap 暴露底层 ResponseWriter，兼容支持 http.ResponseController 的调用方。
func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
