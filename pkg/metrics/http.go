package metrics

import (
	"net/http"
	"time"
)

// Middleware 记录 API RED 指标；route 取 ServeMux 匹配模板，避免 UUID/会话 ID 形成高基数标签。
func Middleware(next http.Handler) http.Handler {
	if next == nil {
		return http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Prometheus 抓取自身不产生请求指标，避免监控流量放大噪声。
		if r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}
		started := time.Now()
		writer := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(writer, r)
		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		ObserveHTTP(normalizeMethod(r.Method), route, writer.status, time.Since(started))
	})
}

// normalizeMethod 将非常见方法折叠为 OTHER，防止恶意方法名形成无界标签。
func normalizeMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodConnect, http.MethodOptions, http.MethodTrace:
		return method
	default:
		return "OTHER"
	}
}

// statusWriter 记录响应状态码并透传 Flush，保证 SSE 指标中间件不破坏流式响应。
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

// Flush 透传底层 Flush。
func (w *statusWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Unwrap 暴露底层 ResponseWriter，兼容 http.ResponseController。
func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
