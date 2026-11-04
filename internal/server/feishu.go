package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/pkg/feishu"
	"github.com/google/uuid"
)

// 飞书回调请求头名称，签名基于时间戳、随机串与加密密钥计算。
const (
	// feishuTimestampHeader 事件时间戳（Unix 秒）。
	feishuTimestampHeader = "X-Lark-Request-Timestamp"
	// feishuNonceHeader 事件随机串。
	feishuNonceHeader = "X-Lark-Request-Nonce"
	// feishuSignatureHeader 事件签名。
	feishuSignatureHeader = "X-Lark-Signature"
)

// feishuHTTPHandler 承载飞书事件回调，不经过用户 JWT 鉴权。
type feishuHTTPHandler struct {
	// ingest 事件接入服务，负责验签、解密、幂等入库与异步入队。
	ingest *biz.FeishuIngestService
}

// events 处理飞书事件回调：读取原始请求体后交给接入服务，成功即快速 ACK。
// 关键步骤：URL 验证原样返回 challenge；普通事件只返回成功码，模型调用全部异步执行。
func (h *feishuHTTPHandler) events(w http.ResponseWriter, r *http.Request) {
	limit := h.ingest.MaxBodyBytes()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "feishu_event_too_large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "feishu_event_unreadable"})
		return
	}
	result, err := h.handle(r, body)
	if err != nil {
		// 验签与重放窗口失败统一返回 401，避免向调用方泄露具体校验细节。
		if errors.Is(err, feishu.ErrSignatureInvalid) || errors.Is(err, feishu.ErrTimestampOutOfWindow) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "feishu_signature_invalid"})
			return
		}
		if errors.Is(err, biz.ErrResourceNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "feishu_bot_not_found"})
			return
		}
		if errors.Is(err, biz.ErrInvalidConfig) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "feishu_event_invalid"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "feishu_event_failed"})
		return
	}
	if result.Challenge != "" {
		writeJSON(w, http.StatusOK, map[string]string{"challenge": result.Challenge})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"code": 0, "msg": "success"})
}

// handle 按回调路径中的机器人 ID 选择验签凭据；未提供 ID 时走默认应用回调。
func (h *feishuHTTPHandler) handle(r *http.Request, body []byte) (biz.FeishuIngestResult, error) {
	request := feishu.Request{
		Timestamp: r.Header.Get(feishuTimestampHeader),
		Nonce:     r.Header.Get(feishuNonceHeader),
		Signature: r.Header.Get(feishuSignatureHeader),
		Body:      body,
	}
	rawBotID := r.PathValue("botID")
	if rawBotID == "" {
		return h.ingest.Handle(r.Context(), request)
	}
	botID, err := uuid.Parse(rawBotID)
	if err != nil {
		return biz.FeishuIngestResult{}, fmt.Errorf("%w: invalid feishu bot id", biz.ErrResourceNotFound)
	}
	return h.ingest.HandleForBot(r.Context(), botID, request)
}
