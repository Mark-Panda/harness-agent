package server

import (
	"net/http"

	"github.com/Mark-Panda/harness-agent/internal/biz"
)

// getPlan 处理 GET /api/v1/runs/{id}/plan：返回最新计划与步骤。
func (h *platformHTTPHandler) getPlan(w http.ResponseWriter, r *http.Request) {
	runID, ok := pathUUID(w, r)
	if !ok {
		return
	}
	plan, err := h.service.Plans.GetByRun(r.Context(), requestPrincipal(r), runID)
	writePlatformResult(w, http.StatusOK, planResponse(plan), err)
}

// listPlanVersions 处理 GET /api/v1/runs/{id}/plan/versions：返回计划版本历史。
func (h *platformHTTPHandler) listPlanVersions(w http.ResponseWriter, r *http.Request) {
	runID, ok := pathUUID(w, r)
	if !ok {
		return
	}
	plans, err := h.service.Plans.ListVersions(r.Context(), requestPrincipal(r), runID)
	writePlatformResult(w, http.StatusOK, map[string]any{"items": mapItems(plans, planResponse)}, err)
}

// pausePlan 处理 POST /api/v1/runs/{id}/plan/pause：暂停执行中的计划。
func (h *platformHTTPHandler) pausePlan(w http.ResponseWriter, r *http.Request) {
	runID, ok := pathUUID(w, r)
	if !ok {
		return
	}
	plan, err := h.service.Plans.Pause(r.Context(), requestPrincipal(r), runID)
	writePlatformResult(w, http.StatusOK, planResponse(plan), err)
}

// resumePlan 处理 POST /api/v1/runs/{id}/plan/resume：恢复暂停的计划并入队 Run。
func (h *platformHTTPHandler) resumePlan(w http.ResponseWriter, r *http.Request) {
	runID, ok := pathUUID(w, r)
	if !ok {
		return
	}
	plan, err := h.service.Plans.Resume(r.Context(), requestPrincipal(r), runID)
	writePlatformResult(w, http.StatusOK, planResponse(plan), err)
}

// cancelPlan 处理 POST /api/v1/runs/{id}/plan/cancel：人工终止计划并取消 Run。
func (h *platformHTTPHandler) cancelPlan(w http.ResponseWriter, r *http.Request) {
	runID, ok := pathUUID(w, r)
	if !ok {
		return
	}
	plan, err := h.service.Plans.Cancel(r.Context(), requestPrincipal(r), runID)
	writePlatformResult(w, http.StatusOK, planResponse(plan), err)
}

// revisePlan 处理 POST /api/v1/runs/{id}/plan/revise：人工修改暂停计划并创建新版本。
func (h *platformHTTPHandler) revisePlan(w http.ResponseWriter, r *http.Request) {
	runID, ok := pathUUID(w, r)
	if !ok {
		return
	}
	var draft biz.PlanDraft
	if !decodeJSON(w, r, &draft) {
		return
	}
	plan, err := h.service.Plans.Revise(r.Context(), requestPrincipal(r), runID, draft)
	writePlatformResult(w, http.StatusCreated, planResponse(plan), err)
}

// planResponse 构造计划出参，包含步骤状态、尝试次数与失败原因。
func planResponse(plan biz.Plan) map[string]any {
	return map[string]any{
		"id": plan.ID, "run_id": plan.RunID, "session_id": plan.SessionID, "version": plan.Version,
		"parent_plan_id": plan.ParentPlanID, "status": plan.Status, "goal": plan.Goal,
		"config_snapshot": plan.ConfigSnapshot, "max_steps": plan.MaxSteps,
		"total_input_tokens": plan.TotalInputTokens, "total_output_tokens": plan.TotalOutputTokens,
		"estimated_cost": plan.EstimatedCost, "deadline_at": plan.DeadlineAt,
		"error_code": plan.ErrorCode, "error_message": plan.ErrorMessage, "revision": plan.Revision,
		"created_at": plan.CreatedAt, "updated_at": plan.UpdatedAt, "finished_at": plan.FinishedAt,
		"steps": mapItems(plan.Steps, planStepResponse),
	}
}

// planStepResponse 构造计划步骤出参。
func planStepResponse(step biz.PlanStep) map[string]any {
	return map[string]any{
		"id": step.ID, "plan_id": step.PlanID, "step_no": step.StepNo, "title": step.Title,
		"description": step.Description, "depends_on": step.DependsOn, "tool_name": step.ToolName,
		"arguments": step.Arguments, "idempotency_key": step.IdempotencyKey, "status": step.Status,
		"attempts": step.Attempts, "max_attempts": step.MaxAttempts, "execution_step_id": step.ExecutionStepID,
		"result": step.Result, "error_code": step.ErrorCode, "error_message": step.ErrorMessage,
		"started_at": step.StartedAt, "finished_at": step.FinishedAt,
	}
}
