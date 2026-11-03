package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
)

// idempotencyKeyHeader 是客户端提供幂等键的请求头。
const idempotencyKeyHeader = "Idempotency-Key"

// maxIdempotentBodyBytes 限制参与指纹计算的请求体大小，与写接口上限保持一致。
const maxIdempotentBodyBytes = 4*1024*1024 + 64*1024

// withIdempotency 包装写接口：按 Idempotency-Key 做指纹校验、并发生效与结果回放。
func withIdempotency(service *biz.IdempotencyService, resourceType string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get(idempotencyKeyHeader)
		if service == nil || key == "" {
			next(w, r)
			return
		}
		principal, ok := biz.PrincipalFromContext(r.Context())
		if !ok || principal.UserID == uuid.Nil {
			writeErrorCode(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		// 读取请求体用于指纹计算后原样恢复，保证业务处理逻辑不受影响。
		body, err := io.ReadAll(io.LimitReader(r.Body, maxIdempotentBodyBytes+1))
		if err != nil || len(body) > maxIdempotentBodyBytes {
			writeErrorCode(w, http.StatusBadRequest, "invalid_json")
			return
		}
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))
		hash := sha256.Sum256([]byte(r.Method + "\n" + r.URL.Path + "\n" + string(body)))
		outcome, err := service.Begin(r.Context(), principal.UserID, key, resourceType, hash[:])
		if err != nil {
			writeIdempotencyError(w, err)
			return
		}
		if outcome.Replay {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Idempotency-Replayed", "true")
			w.WriteHeader(outcome.Status)
			_, _ = w.Write(outcome.Body)
			return
		}

		recorder := &idempotentResponseWriter{header: make(http.Header), status: http.StatusOK}
		next(recorder, r)
		resourceID := responseResourceID(recorder.body.Bytes())
		if recorder.status >= 200 && recorder.status < 300 {
			// 成功结果必须落库后才能回放；落库失败时返回 500，避免客户端误以为已持久化。
			if err := service.Complete(r.Context(), principal.UserID, key, recorder.status, recorder.body.Bytes(), resourceID); err != nil {
				writeErrorCode(w, http.StatusInternalServerError, "idempotency_store_failed")
				return
			}
		} else {
			_ = service.Fail(r.Context(), principal.UserID, key)
		}
		for name, values := range recorder.header {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		w.WriteHeader(recorder.status)
		_, _ = w.Write(recorder.body.Bytes())
	}
}

// idempotentResponseWriter 捕获下游响应，用于完成后持久化并统一写出。
type idempotentResponseWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

// Header 返回可写响应头。
func (w *idempotentResponseWriter) Header() http.Header { return w.header }

// WriteHeader 记录下游状态码。
func (w *idempotentResponseWriter) WriteHeader(status int) { w.status = status }

// Write 记录下游响应体。
func (w *idempotentResponseWriter) Write(value []byte) (int, error) {
	return w.body.Write(value)
}

// writeIdempotencyError 映射幂等错误到 HTTP。
func writeIdempotencyError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, biz.ErrIdempotencyConflict):
		writeErrorCode(w, http.StatusConflict, "idempotency_conflict")
	case errors.Is(err, biz.ErrForbidden):
		writeErrorCode(w, http.StatusUnauthorized, "unauthorized")
	case errors.Is(err, biz.ErrInvalidConfig):
		writeErrorCode(w, http.StatusBadRequest, "invalid_idempotency_key")
	default:
		writeErrorCode(w, http.StatusInternalServerError, "internal_error")
	}
}

// responseResourceID 从响应 JSON 中提取资源 id，用于审计关联；缺失时返回空串。
func responseResourceID(body []byte) string {
	var envelope struct {
		ID string `json:"id"`
	}
	if len(body) == 0 || json.Unmarshal(body, &envelope) != nil {
		return ""
	}
	if envelope.ID == "" {
		return ""
	}
	if _, err := uuid.Parse(envelope.ID); err != nil {
		return ""
	}
	return envelope.ID
}
