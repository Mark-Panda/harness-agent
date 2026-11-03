package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/pkg/metrics"
	"github.com/google/uuid"
)

// runRequest 是创建 Run 的请求体。
type runRequest struct {
	// AgentID 可选的 Agent 版本。
	AgentID *uuid.UUID `json:"agent_id"`
	// AgentVersion 可选的 Agent 版本号，与 AgentID 成对出现。
	AgentVersion *int `json:"agent_version"`
	// Mode 编排模式：react/plan/super。
	Mode biz.AgentMode `json:"mode"`
}

// createRun 处理 POST /api/v1/sessions/{id}/runs：创建排队中的 Run。
func (h *platformHTTPHandler) createRun(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := pathUUID(w, r)
	if !ok {
		return
	}
	var request runRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	// HTTP 幂等键同时作为 Run 的业务幂等键，双重防止重复创建。
	run, err := h.service.Runs.Create(r.Context(), requestPrincipal(r), biz.CreateRunInput{
		SessionID: sessionID, AgentID: request.AgentID, AgentVersion: request.AgentVersion,
		Mode: request.Mode, IdempotencyKey: strings.TrimSpace(r.Header.Get(idempotencyKeyHeader)),
	})
	writePlatformResult(w, http.StatusCreated, runResponse(run), err)
}

// getRun 处理 GET /api/v1/runs/{id}：读取 Run 状态与快照。
func (h *platformHTTPHandler) getRun(w http.ResponseWriter, r *http.Request) {
	runID, ok := pathUUID(w, r)
	if !ok {
		return
	}
	run, err := h.service.Runs.Get(r.Context(), requestPrincipal(r), runID)
	writePlatformResult(w, http.StatusOK, runResponse(run), err)
}

// listRuns 处理 GET /api/v1/sessions/{id}/runs：游标分页列出会话 Run。
func (h *platformHTTPHandler) listRuns(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := pathUUID(w, r)
	if !ok {
		return
	}
	limit, cursor, ok := pageRequest(w, r)
	if !ok {
		return
	}
	page, err := h.service.Runs.List(r.Context(), requestPrincipal(r), sessionID, limit, cursor)
	writePlatformResult(w, http.StatusOK, map[string]any{"items": mapItems(page.Items, runResponse), "next_cursor": page.NextCursor}, err)
}

// cancelRun 处理 POST /api/v1/runs/{id}/cancel：取消非终态 Run。
func (h *platformHTTPHandler) cancelRun(w http.ResponseWriter, r *http.Request) {
	runID, ok := pathUUID(w, r)
	if !ok {
		return
	}
	run, err := h.service.Runs.Cancel(r.Context(), requestPrincipal(r), runID)
	writePlatformResult(w, http.StatusOK, runResponse(run), err)
}

// runEvents 处理 GET /api/v1/runs/{id}/events：按 seq 游标回放并持续推送事件。
// 客户端通过 Last-Event-ID 或 ?after 指定已收到的最新序号，重连不会重复执行 Run。
func (h *platformHTTPHandler) runEvents(w http.ResponseWriter, r *http.Request) {
	runID, ok := pathUUID(w, r)
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErrorCode(w, http.StatusInternalServerError, "streaming_unsupported")
		return
	}
	after := int64(0)
	if header := r.Header.Get("Last-Event-ID"); header != "" {
		parsed, err := strconv.ParseInt(header, 10, 64)
		if err != nil || parsed < 0 {
			writeErrorCode(w, http.StatusBadRequest, "invalid_event_cursor")
			return
		}
		after = parsed
	} else if raw := r.URL.Query().Get("after"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			writeErrorCode(w, http.StatusBadRequest, "invalid_event_cursor")
			return
		}
		after = parsed
	}
	if _, err := h.service.Runs.Get(r.Context(), requestPrincipal(r), runID); err != nil {
		writePlatformError(w, err)
		return
	}
	// 连接槽位必须在写入 200 之前占用，超限时返回 503 而不是建立后立刻断开。
	release, acquired := acquireSSEConnection()
	if !acquired {
		writeErrorCode(w, http.StatusServiceUnavailable, "sse_capacity_exceeded")
		return
	}
	defer release()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	metrics.AddSSEConnection(1)
	defer metrics.AddSSEConnection(-1)

	deadline := time.Now().Add(5 * time.Minute)
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	poll := time.NewTicker(250 * time.Millisecond)
	defer poll.Stop()

	for {
		events, listErr := h.service.Runs.ListEvents(r.Context(), requestPrincipal(r), runID, after, 200)
		if listErr != nil {
			// 流已开始，只能以 run.failed 风格结束，避免写入非法 JSON 错误体。
			writeSSE(w, after+1, "run.failed", map[string]any{"reason": "event_read_failed"})
			flusher.Flush()
			return
		}
		for _, event := range events {
			after = event.Seq
			if !writeSSE(w, event.Seq, event.EventType, event.Payload) {
				return
			}
		}
		if len(events) > 0 {
			flusher.Flush()
		}
		current, getErr := h.service.Runs.Get(r.Context(), requestPrincipal(r), runID)
		if getErr != nil {
			return
		}
		// 终态且事件已全部输出后关闭连接，客户端无需猜测结束时间。
		if biz.RunTerminal(current.Status) && len(events) == 0 {
			return
		}
		if time.Now().After(deadline) {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if !writeSSE(w, 0, "heartbeat", map[string]any{"status": "ok"}) {
				return
			}
			flusher.Flush()
		case <-poll.C:
		}
	}
}

// runSteps 处理 GET /api/v1/runs/{id}/steps：返回执行树步骤，可选附带脱敏载荷。
func (h *platformHTTPHandler) runSteps(w http.ResponseWriter, r *http.Request) {
	runID, ok := pathUUID(w, r)
	if !ok {
		return
	}
	steps, err := h.service.Runs.ListSteps(r.Context(), requestPrincipal(r), runID, 500)
	if err != nil {
		writePlatformError(w, err)
		return
	}
	response := map[string]any{"steps": mapItems(steps, stepResponse)}
	include := r.URL.Query().Get("include_payloads")
	if include == "1" || strings.EqualFold(include, "true") {
		var stepID *uuid.UUID
		if raw := r.URL.Query().Get("step_id"); raw != "" {
			parsed, parseErr := uuid.Parse(raw)
			if parseErr != nil {
				writeErrorCode(w, http.StatusBadRequest, "invalid_step_id")
				return
			}
			stepID = &parsed
		}
		// 载荷查阅必须填写理由并写入 Run 事件流，形成可查询的审计记录。
		reason := strings.TrimSpace(r.URL.Query().Get("reason"))
		if reason == "" {
			writeErrorCode(w, http.StatusBadRequest, "payload_reason_required")
			return
		}
		if err := h.service.Runs.RecordPayloadAccess(r.Context(), requestPrincipal(r), runID, stepID, reason); err != nil {
			writePlatformError(w, err)
			return
		}
		payloads, listErr := h.service.Runs.ListPayloads(r.Context(), requestPrincipal(r), runID, stepID, 200)
		if listErr != nil {
			writePlatformError(w, listErr)
			return
		}
		response["payloads"] = mapItems(payloads, payloadResponse)
	}
	writeJSON(w, http.StatusOK, response)
}

// runAttempts 处理 GET /api/v1/runs/{id}/attempts：返回模型 Attempt 与切换事实。
func (h *platformHTTPHandler) runAttempts(w http.ResponseWriter, r *http.Request) {
	runID, ok := pathUUID(w, r)
	if !ok {
		return
	}
	attempts, err := h.service.Runs.ListModelCalls(r.Context(), requestPrincipal(r), runID)
	if err != nil {
		writePlatformError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": mapItems(attempts, attemptResponse)})
}

// breakGlassRequest 是管理员特权读取执行记录的请求体。
type breakGlassRequest struct {
	// UserID 目标用户 ID。
	UserID uuid.UUID `json:"user_id"`
	// RunID 目标 Run ID。
	RunID uuid.UUID `json:"run_id"`
	// Reason 书面理由，必填并写入审计事件。
	Reason string `json:"reason"`
}

// breakGlass 处理 POST /api/v1/admin/break-glass：管理员凭书面理由读取目标用户执行记录。
// 读取动作强制写入 Run 审计事件，审计失败时拒绝返回数据。
func (h *platformHTTPHandler) breakGlass(w http.ResponseWriter, r *http.Request) {
	var request breakGlassRequest
	if !decodeJSON(w, r, &request, 64*1024) {
		return
	}
	result, err := h.service.Runs.BreakGlassRead(r.Context(), requestPrincipal(r), request.UserID, request.RunID, request.Reason)
	response := map[string]any{
		"run":         runResponse(result.Run),
		"steps":       mapItems(result.Steps, stepResponse),
		"payloads":    mapItems(result.Payloads, payloadResponse),
		"model_calls": mapItems(result.ModelCalls, attemptResponse),
	}
	writePlatformResult(w, http.StatusOK, response, err)
}

// attemptResponse 构造模型 Attempt 出参，包含切换原因与费用，不包含请求正文与密钥。
func attemptResponse(call biz.ModelCall) map[string]any {
	return map[string]any{
		"id": call.ID, "run_id": call.RunID, "step_id": call.StepID, "attempt_no": call.AttemptNo,
		"provider_id": call.ProviderID, "model_id": call.ModelID, "policy_version": call.PolicyVersion,
		"status": call.Status, "provider_request_id": call.ProviderRequestID, "request_manifest": call.RequestManifest,
		"input_tokens": call.InputTokens, "output_tokens": call.OutputTokens, "estimated_cost": call.EstimatedCost,
		"duration_ms": call.DurationMS, "finish_reason": call.FinishReason, "switch_reason": call.SwitchReason,
		"error_class": call.ErrorClass, "error_message": call.ErrorMessage,
		"created_at": call.CreatedAt, "completed_at": call.CompletedAt,
	}
}

// runSupervisorTasks 处理 GET /api/v1/runs/{id}/supervisor-tasks：返回 Worker 执行树。
func (h *platformHTTPHandler) runSupervisorTasks(w http.ResponseWriter, r *http.Request) {
	runID, ok := pathUUID(w, r)
	if !ok {
		return
	}
	tasks, err := h.service.Runs.ListSupervisorTasks(r.Context(), requestPrincipal(r), runID)
	if err != nil {
		writePlatformError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": mapItems(tasks, supervisorTaskResponse)})
}

// supervisorTaskResponse 构造 Supervisor 子任务出参，不包含任何凭证或隐藏推理。
func supervisorTaskResponse(task biz.SupervisorTask) map[string]any {
	return map[string]any{
		"id": task.ID, "parent_run_id": task.ParentRunID, "child_session_id": task.ChildSessionID,
		"worker_name": task.WorkerName, "worker_index": task.WorkerIndex, "task": task.Task,
		"tool_scope": task.ToolScope, "status": task.Status, "attempts": task.Attempts,
		"max_attempts": task.MaxAttempts, "result": task.Result, "error_code": task.ErrorCode,
		"error_message": task.ErrorMessage, "depth": task.Depth,
		"started_at": task.StartedAt, "finished_at": task.FinishedAt, "created_at": task.CreatedAt,
	}
}

// approvalDecisionRequest 是人工审批决策请求体。
type approvalDecisionRequest struct {
	// Decision 决策取值：approve 或 deny。
	Decision string `json:"decision"`
}

// runApprovals 处理 GET /api/v1/runs/{id}/approvals：列出当前用户 Run 的工具审批。
func (h *platformHTTPHandler) runApprovals(w http.ResponseWriter, r *http.Request) {
	runID, ok := pathUUID(w, r)
	if !ok {
		return
	}
	approvals, err := h.service.Runs.ListApprovals(r.Context(), requestPrincipal(r), runID)
	if err != nil {
		writePlatformError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": mapItems(approvals, approvalResponse)})
}

// decideApproval 处理 POST /api/v1/runs/{id}/approvals/{approval_id}：批准或拒绝工具调用。
func (h *platformHTTPHandler) decideApproval(w http.ResponseWriter, r *http.Request) {
	runID, ok := pathUUID(w, r)
	if !ok {
		return
	}
	approvalID, err := uuid.Parse(r.PathValue("approval_id"))
	if err != nil {
		writeErrorCode(w, http.StatusBadRequest, "invalid_approval_id")
		return
	}
	var request approvalDecisionRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	approval, err := h.service.Runs.DecideApproval(r.Context(), requestPrincipal(r), runID, approvalID, request.Decision)
	if err != nil {
		writePlatformError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, approvalResponse(approval))
}

// approvalResponse 构造工具审批出参；参数与结果均为已脱敏的有限正文。
func approvalResponse(approval biz.ToolApproval) map[string]any {
	return map[string]any{
		"id": approval.ID, "run_id": approval.RunID, "step_id": approval.StepID,
		"tool_call_id": approval.ToolCallID, "tool_name": approval.ToolName, "risk_level": approval.RiskLevel,
		"status": approval.Status, "arguments": approval.Arguments, "idempotency_key": approval.IdempotencyKey,
		"result": approval.ResultJSON, "error_code": approval.ErrorCode, "error_message": approval.ErrorMessage,
		"decided_by": approval.DecidedBy, "decided_at": approval.DecidedAt, "delivered_at": approval.DeliveredAt,
		"created_at": approval.CreatedAt, "updated_at": approval.UpdatedAt,
	}
}

// stepResponse 构造执行步骤出参。
func stepResponse(step biz.ExecutionStep) map[string]any {
	return map[string]any{
		"id": step.ID, "run_id": step.RunID, "step_seq": step.StepSeq, "parent_step_id": step.ParentStepID,
		"kind": step.Kind, "name": step.Name, "status": step.Status, "tool_name": step.ToolName,
		"attempt_no": step.AttemptNo, "risk_level": step.RiskLevel, "idempotency_key": step.IdempotencyKey,
		"error_code": step.ErrorCode, "started_at": step.StartedAt, "finished_at": step.FinishedAt,
	}
}

// payloadResponse 构造脱敏执行载荷出参。
func payloadResponse(payload biz.ExecutionPayload) map[string]any {
	return map[string]any{
		"id": payload.ID, "run_id": payload.RunID, "step_id": payload.StepID, "type": payload.Type,
		"content_json": payload.ContentJSON, "content_text": payload.ContentText,
		"capture_mode": payload.CaptureMode, "truncated": payload.Truncated, "partial": payload.Partial,
		"original_size_bytes": payload.OriginalSizeBytes, "saved_size_bytes": payload.SavedSizeBytes,
		"created_at": payload.CreatedAt,
	}
}

// sseEventTypePattern 限制 SSE event 名称为安全字符，防止 CRLF 注入与非法协议字段。
var sseEventTypePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// safeSSEEventType 返回合法事件名；非法值统一降级为 message，避免污染 SSE 帧。
func safeSSEEventType(eventType string) string {
	if sseEventTypePattern.MatchString(eventType) {
		return eventType
	}
	return "message"
}

// writeSSE 写出单条 SSE 帧；seq 为 0 表示心跳，不带 id 避免污染客户端游标。
// 返回 false 表示慢消费者写入超时或连接已断开，调用方必须停止推送并释放槽位。
func writeSSE(w http.ResponseWriter, seq int64, eventType string, payload map[string]any) bool {
	eventType = safeSSEEventType(eventType)
	metrics.IncSSEEvent(eventType)
	setSSEWriteDeadline(w)
	if seq > 0 {
		if _, err := fmt.Fprintf(w, "id: %d\n", seq); err != nil {
			return false
		}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		encoded = []byte(`{}`)
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, encoded)
	return err == nil
}

// runResponse 构造 Run 出参。
func runResponse(run biz.Run) map[string]any {
	return map[string]any{
		"id": run.ID, "session_id": run.SessionID, "agent_id": run.AgentID, "agent_version": run.AgentVersion,
		"mode": run.Mode, "status": run.Status, "current_step_seq": run.CurrentStepSeq, "attempt_no": run.AttemptNo,
		"config_snapshot": run.ConfigSnapshot, "model_policy_snapshot": run.ModelPolicySnapshot,
		"error_code": run.ErrorCode, "error_message": run.ErrorMessage,
		"deadline_at": run.DeadlineAt, "started_at": run.StartedAt, "finished_at": run.FinishedAt,
		"created_at": run.CreatedAt, "updated_at": run.UpdatedAt,
	}
}
