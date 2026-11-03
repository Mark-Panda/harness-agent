package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
)

// deletionResponse 是删除请求状态的 HTTP 响应结构。
type deletionResponse struct {
	// ID 请求 ID。
	ID string `json:"id"`
	// ResourceType 资源类型。
	ResourceType string `json:"resource_type"`
	// ResourceID 资源标识。
	ResourceID string `json:"resource_id"`
	// Status 生命周期状态。
	Status string `json:"status"`
	// RequestedSource 请求渠道。
	RequestedSource string `json:"requested_source"`
	// Attempts 已领取次数。
	Attempts int `json:"attempts"`
	// MaxAttempts 最大尝试次数。
	MaxAttempts int `json:"max_attempts"`
	// LastError 最近失败原因。
	LastError string `json:"last_error,omitempty"`
	// CreatedAt 创建时间。
	CreatedAt time.Time `json:"created_at"`
	// CompletedAt 完成时间。
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

// deletionHTTPResponse 把领域删除请求转换为响应结构。
func deletionHTTPResponse(request biz.DeletionRequest) deletionResponse {
	return deletionResponse{
		ID: request.ID.String(), ResourceType: string(request.ResourceType), ResourceID: request.ResourceID,
		Status: string(request.Status), RequestedSource: request.RequestedSource, Attempts: request.Attempts,
		MaxAttempts: request.MaxAttempts, LastError: request.LastError, CreatedAt: request.CreatedAt,
		CompletedAt: request.CompletedAt,
	}
}

// requestSessionDeletion 处理 POST /api/v1/sessions/{id}/deletion：创建会话异步删除请求。
func (h *platformHTTPHandler) requestSessionDeletion(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	request, err := h.service.Deletions.RequestSessionDeletion(r.Context(), requestPrincipal(r), id, r.URL.Query().Get("source"))
	writePlatformResult(w, http.StatusAccepted, deletionHTTPResponse(request), err)
}

// requestMemoryDeletion 处理 POST /api/v1/memory/deletion：创建用户长期记忆异步删除请求。
func (h *platformHTTPHandler) requestMemoryDeletion(w http.ResponseWriter, r *http.Request) {
	request, err := h.service.Deletions.RequestMemoryDeletion(r.Context(), requestPrincipal(r), r.URL.Query().Get("source"))
	writePlatformResult(w, http.StatusAccepted, deletionHTTPResponse(request), err)
}

// getDeletion 处理 GET /api/v1/deletions/{id}：读取删除请求状态。
func (h *platformHTTPHandler) getDeletion(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	request, err := h.service.Deletions.Get(r.Context(), requestPrincipal(r), id)
	writePlatformResult(w, http.StatusOK, deletionHTTPResponse(request), err)
}

// listDeletions 处理 GET /api/v1/deletions：分页列出当前用户删除请求。
func (h *platformHTTPHandler) listDeletions(w http.ResponseWriter, r *http.Request) {
	limit, cursor, ok := pageRequest(w, r)
	if !ok {
		return
	}
	page, err := h.service.Deletions.List(r.Context(), requestPrincipal(r), limit, cursor)
	writePlatformResult(w, http.StatusOK, map[string]any{
		"items": mapItems(page.Items, func(request biz.DeletionRequest) map[string]any {
			response := deletionHTTPResponse(request)
			return map[string]any{
				"id": response.ID, "resource_type": response.ResourceType, "resource_id": response.ResourceID,
				"status": response.Status, "requested_source": response.RequestedSource,
				"attempts": response.Attempts, "max_attempts": response.MaxAttempts,
				"last_error": response.LastError, "created_at": response.CreatedAt, "completed_at": response.CompletedAt,
			}
		}), "next_cursor": page.NextCursor,
	}, err)
}

// exportSession 处理 GET /api/v1/sessions/{id}/export：下载会话完整导出 JSON。
func (h *platformHTTPHandler) exportSession(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	export, err := h.service.Exports.ExportSession(r.Context(), requestPrincipal(r), id)
	if err != nil {
		writePlatformError(w, err)
		return
	}
	writeExport(w, "session-"+id.String()+".json", export)
}

// exportMemory 处理 GET /api/v1/memory/export：下载用户长期记忆导出 JSON。
func (h *platformHTTPHandler) exportMemory(w http.ResponseWriter, r *http.Request) {
	export, err := h.service.Exports.ExportMemory(r.Context(), requestPrincipal(r))
	if err != nil {
		writePlatformError(w, err)
		return
	}
	writeExport(w, "memory-export.json", export)
}

// requestAccountDeletion 处理 POST /api/v1/account/deletion：创建账号级异步删除请求。
func (h *platformHTTPHandler) requestAccountDeletion(w http.ResponseWriter, r *http.Request) {
	request, err := h.service.Deletions.RequestAccountDeletion(r.Context(), requestPrincipal(r), r.URL.Query().Get("source"))
	writePlatformResult(w, http.StatusAccepted, deletionHTTPResponse(request), err)
}

// exportAccount 处理 GET /api/v1/account/export：下载账号全量导出 JSON。
func (h *platformHTTPHandler) exportAccount(w http.ResponseWriter, r *http.Request) {
	export, err := h.service.Exports.ExportAccount(r.Context(), requestPrincipal(r))
	if err != nil {
		writePlatformError(w, err)
		return
	}
	writeExport(w, "account-export.json", export)
}

// writeExport 以附件形式输出 JSON，禁止浏览器缓存敏感导出。
func writeExport(w http.ResponseWriter, filename string, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(payload)
}
