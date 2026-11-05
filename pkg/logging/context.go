package logging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
)

const (
	// AttrRequestID 请求关联 ID，贯通 HTTP 与后台任务日志。
	AttrRequestID = "request_id"
	// AttrRunID 一次 Agent Run 的 ID，仅用于日志关联。
	AttrRunID = "run_id"
)

// requestIDKey 是 request id 在 context 中的私有键。
type requestIDKey struct{}

// runIDKey 是 run id 在 context 中的私有键，供结构化日志自动关联。
type runIDKey struct{}

// WithRequestID 把请求关联 ID 写入上下文，供日志自动附加 request_id。
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
// 非法值视为缺失并重新生成，防止日志注入或超长头污染关联字段。
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
