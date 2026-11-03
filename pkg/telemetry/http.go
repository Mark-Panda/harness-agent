package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
)

// headerRequestID 是请求关联 ID 的标准响应头与入站提取头。
const headerRequestID = "X-Request-ID"

// requestIDKey 是 request id 在 context 中的私有键。
type requestIDKey struct{}

// runIDKey 是 run id 在 context 中的私有键，供结构化日志自动关联。
type runIDKey struct{}

// WithRunID 把 Run ID 写入上下文，后台任务与模型调用日志据此自动附加 run_id。
func WithRunID(ctx context.Context, runID string) context.Context {
	if runID == "" {
		return ctx
	}
	return context.WithValue(ctx, runIDKey{}, runID)
}

// RunIDFrom 读取上下文中的 Run ID；不存在时返回空串。
func RunIDFrom(ctx context.Context) string {
	value, _ := ctx.Value(runIDKey{}).(string)
	return value
}

// WithRequestID 把请求关联 ID 写入上下文，供日志与 Span 关联。
func WithRequestID(ctx context.Context, requestID string) context.Context {
	if requestID == "" {
		return ctx
	}
	return context.WithValue(ctx, requestIDKey{}, requestID)
}

// RequestIDFrom 读取上下文中的请求关联 ID；不存在时返回空串。
func RequestIDFrom(ctx context.Context) string {
	value, _ := ctx.Value(requestIDKey{}).(string)
	return value
}

// NewRequestID 生成 128 位随机十六进制请求 ID，避免跨副本碰撞。
func NewRequestID() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		// 随机源不可用时退回时间无关的固定前缀，仍保证响应头存在。
		return "req-unknown"
	}
	return hex.EncodeToString(buffer)
}

// normalizeRequestID 校验入站请求 ID：只允许可见安全字符且长度不超过 64。
// 非法值视为缺失并重新生成，防止日志注入或超长头污染 Trace。
func normalizeRequestID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 64 {
		return ""
	}
	for _, char := range raw {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z',
			char >= '0' && char <= '9', char == '-', char == '_', char == '.', char == ':':
		default:
			return ""
		}
	}
	return raw
}

// HTTPMiddleware 为入站 HTTP 请求建立服务端 Span，提取 traceparent 并生成 request_id。
// 关键约束：只记录方法与路径，不记录查询串、请求体、响应体或认证头。
func HTTPMiddleware(next http.Handler) http.Handler {
	if next == nil {
		return http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		requestID := normalizeRequestID(r.Header.Get(headerRequestID))
		if requestID == "" {
			requestID = NewRequestID()
		}
		ctx = WithRequestID(ctx, requestID)
		ctx, span := StartServer(ctx, "http.server.request",
			attribute.String(AttrHTTPMethod, r.Method),
			attribute.String(AttrHTTPPath, r.URL.Path),
			attribute.String(AttrRequestID, requestID),
		)
		defer span.End()
		w.Header().Set(headerRequestID, requestID)

		writer := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(writer, r.WithContext(ctx))

		span.SetAttributes(attribute.Int(AttrHTTPStatus, writer.status))
		if writer.status >= http.StatusBadRequest {
			span.SetStatus(codes.Error, http.StatusText(writer.status))
		}
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
