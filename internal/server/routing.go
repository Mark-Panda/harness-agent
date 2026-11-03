package server

import (
	"net/http"

	"github.com/Mark-Panda/harness-agent/internal/biz"
)

// listRoutingHealth 处理 GET /api/v1/model-routing/health：管理员查看各模型熔断状态。
func (h *platformHTTPHandler) listRoutingHealth(w http.ResponseWriter, r *http.Request) {
	health, err := h.service.Routing.List(r.Context(), requestPrincipal(r))
	writePlatformResult(w, http.StatusOK, map[string]any{"items": mapItems(health, modelHealthResponse)}, err)
}

// modelHealthResponse 构造模型健康出参；不包含任何供应商密钥或请求正文。
func modelHealthResponse(health biz.ModelHealth) map[string]any {
	return map[string]any{
		"model_id": health.ModelID, "provider_id": health.ProviderID, "remote_model_name": health.RemoteModelName,
		"status": health.Status, "failures": health.Failures, "checked_at": health.CheckedAt,
	}
}
