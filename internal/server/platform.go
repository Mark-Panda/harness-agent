package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/service"
	"github.com/google/uuid"
)

// platformHTTPHandler 承载平台配置、Agent 与会话的 HTTP 接口。
// 所有路由均经过 Bearer 鉴权中间件，用户身份只来自 Token。
type platformHTTPHandler struct {
	service *service.PlatformService
}

// registerPlatformRoutes 注册 /api/v1 平台与用户接口，全部要求已认证。
func registerPlatformRoutes(mux *http.ServeMux, auth *authHTTPHandler, platform *service.PlatformService) {
	handler := &platformHTTPHandler{service: platform}
	handle := func(pattern string, endpoint http.HandlerFunc) {
		mux.HandleFunc(pattern, auth.requireAuth(endpoint))
	}
	// 写接口额外支持 Idempotency-Key：同键同指纹回放原结果，冲突返回 409。
	handleWrite := func(pattern, resourceType string, endpoint http.HandlerFunc) {
		mux.HandleFunc(pattern, auth.requireAuth(withIdempotency(platform.Idempotency, resourceType, endpoint)))
	}
	// 管理员接口：Provider、模型目录、路由策略、Skill 与 MCP。
	handleWrite("POST /api/v1/providers", "llm_provider", handler.createProvider)
	handle("GET /api/v1/providers", handler.listProviders)
	handle("GET /api/v1/providers/{id}", handler.getProvider)
	handle("GET /api/v1/providers/{id}/models", handler.listRemoteModels)
	handle("PUT /api/v1/providers/{id}", handler.updateProvider)
	handle("DELETE /api/v1/providers/{id}", handler.deleteProvider)
	handleWrite("POST /api/v1/models", "llm_model", handler.createModel)
	handle("GET /api/v1/models", handler.listModels)
	handle("GET /api/v1/models/{id}", handler.getModel)
	handle("PUT /api/v1/models/{id}", handler.updateModel)
	handle("DELETE /api/v1/models/{id}", handler.deleteModel)
	handleWrite("POST /api/v1/model-policies", "model_policy", handler.createPolicy)
	handle("GET /api/v1/model-policies", handler.listPolicies)
	handle("GET /api/v1/model-policies/{id}", handler.getPolicy)
	handle("PUT /api/v1/model-policies/{id}", handler.updatePolicy)
	handle("DELETE /api/v1/model-policies/{id}", handler.deletePolicy)
	handleWrite("POST /api/v1/model-policies/{id}/versions", "model_policy", handler.createPolicyVersion)
	handleWrite("POST /api/v1/model-policies/{id}/publish", "model_policy", handler.publishPolicy)
	// 管理员接口：平台级 Skill 模板与 MCP 工具接入。
	handleWrite("POST /api/v1/skills", "skill", handler.createSkill)
	handle("GET /api/v1/skills", handler.listSkills)
	handle("GET /api/v1/skills/{id}", handler.getSkill)
	handle("PUT /api/v1/skills/{id}", handler.updateSkill)
	handle("DELETE /api/v1/skills/{id}", handler.deleteSkill)
	handleWrite("POST /api/v1/mcps", "mcp", handler.createMCP)
	handle("GET /api/v1/mcps", handler.listMCPs)
	handle("GET /api/v1/mcps/{id}", handler.getMCP)
	handle("PUT /api/v1/mcps/{id}", handler.updateMCP)
	handle("DELETE /api/v1/mcps/{id}", handler.deleteMCP)
	handleWrite("POST /api/v1/mcps/{id}/test", "mcp", handler.testMCP)
	handle("GET /api/v1/mcps/{id}/tools", handler.listMCPTools)
	// 用户接口：Agent 与会话消息，按 Token 中的 user_id 隔离。
	handleWrite("POST /api/v1/agents", "agent", handler.createAgent)
	handle("GET /api/v1/agents", handler.listAgents)
	handle("GET /api/v1/agents/{id}", handler.getAgent)
	handle("PUT /api/v1/agents/{id}", handler.updateAgent)
	handle("DELETE /api/v1/agents/{id}", handler.deleteAgent)
	handleWrite("POST /api/v1/agents/{id}/versions", "agent", handler.createAgentVersion)
	handleWrite("POST /api/v1/agents/{id}/publish", "agent", handler.publishAgent)
	handleWrite("POST /api/v1/agents/{id}/unpublish", "agent", handler.unpublishAgent)
	handleWrite("POST /api/v1/agents/{id}/archive", "agent", handler.archiveAgent)
	handleWrite("POST /api/v1/sessions", "session", handler.createSession)
	handle("GET /api/v1/sessions", handler.listSessions)
	handle("GET /api/v1/sessions/{id}", handler.getSession)
	handleWrite("POST /api/v1/sessions/{id}/archive", "session", handler.archiveSession)
	handleWrite("PUT /api/v1/sessions/{id}/memory-policy", "session", handler.updateSessionMemoryPolicy)
	handleWrite("POST /api/v1/sessions/{id}/messages", "message", handler.appendMessage)
	handle("GET /api/v1/sessions/{id}/messages", handler.listMessages)
	// 飞书话题生命周期：仅装配飞书能力时注册，仅会话所有者可操作。
	if platform.FeishuBindings != nil {
		handleWrite("POST /api/v1/sessions/{id}/feishu-unbind", "feishu_binding", handler.unbindFeishu)
		handleWrite("POST /api/v1/sessions/{id}/feishu-close", "feishu_binding", handler.closeFeishu)
		handleWrite("POST /api/v1/sessions/{id}/feishu-archive", "feishu_binding", handler.archiveFeishu)
		handleWrite("POST /api/v1/sessions/{id}/feishu-restore", "feishu_binding", handler.restoreFeishu)
	}
	// Run 接口：创建、查询、取消与 SSE 事件回放。
	handleWrite("POST /api/v1/sessions/{id}/runs", "run", handler.createRun)
	handle("GET /api/v1/sessions/{id}/runs", handler.listRuns)
	handle("GET /api/v1/runs/{id}", handler.getRun)
	handleWrite("POST /api/v1/runs/{id}/cancel", "run", handler.cancelRun)
	handle("GET /api/v1/runs/{id}/events", handler.runEvents)
	// 执行树接口：返回步骤事实，按需附带脱敏载荷。
	handle("GET /api/v1/runs/{id}/steps", handler.runSteps)
	// 模型尝试接口：展示 Attempt、切换原因与费用，仅管理员可见健康聚合。
	handle("GET /api/v1/runs/{id}/attempts", handler.runAttempts)
	// 管理员 Break-Glass：凭书面理由跨用户读取脱敏执行记录，强制写入审计事件。
	handle("POST /api/v1/admin/break-glass", handler.breakGlass)
	// 管理员全局运行审计：按用户与来源筛选。
	handle("GET /api/v1/admin/runs", handler.listAdminRuns)
	// 管理员全局助手视图：跨用户只读列表，不返回会话与配置快照明细。
	handle("GET /api/v1/admin/agents", handler.listAdminAgents)
	// 管理员跨用户删除助手版本：历史记录保留，仅解除助手归属。
	handle("DELETE /api/v1/admin/agents/{id}", handler.adminDeleteAgent)
	// Supervisor 执行树：展示 Worker 子任务、尝试次数、结果与失败原因。
	handle("GET /api/v1/runs/{id}/supervisor-tasks", handler.runSupervisorTasks)
	if platform.Routing != nil {
		handle("GET /api/v1/model-routing/health", handler.listRoutingHealth)
	}
	if platform.Plans != nil {
		handle("GET /api/v1/runs/{id}/plan", handler.getPlan)
		handle("GET /api/v1/runs/{id}/plan/versions", handler.listPlanVersions)
		handleWrite("POST /api/v1/runs/{id}/plan/pause", "plan", handler.pausePlan)
		handleWrite("POST /api/v1/runs/{id}/plan/resume", "plan", handler.resumePlan)
		handleWrite("POST /api/v1/runs/{id}/plan/cancel", "plan", handler.cancelPlan)
		handleWrite("POST /api/v1/runs/{id}/plan/revise", "plan", handler.revisePlan)
	}
	// 审批接口：中高风险工具未获批准不得执行，决策后自动恢复 Run。
	handle("GET /api/v1/runs/{id}/approvals", handler.runApprovals)
	handleWrite("POST /api/v1/runs/{id}/approvals/{approval_id}", "tool_approval", handler.decideApproval)
	// 导出与异步删除接口：仅在装配了对应服务时注册，避免测试路由空指针。
	if platform.Deletions != nil {
		handleWrite("POST /api/v1/sessions/{id}/deletion", "deletion_request", handler.requestSessionDeletion)
		handleWrite("POST /api/v1/memory/deletion", "deletion_request", handler.requestMemoryDeletion)
		handleWrite("POST /api/v1/account/deletion", "deletion_request", handler.requestAccountDeletion)
		handle("GET /api/v1/deletions", handler.listDeletions)
		handle("GET /api/v1/deletions/{id}", handler.getDeletion)
	}
	if platform.Exports != nil {
		handle("GET /api/v1/sessions/{id}/export", handler.exportSession)
		handle("GET /api/v1/memory/export", handler.exportMemory)
		handle("GET /api/v1/account/export", handler.exportAccount)
	}
	// 飞书机器人注册与助手绑定：注册仅管理员可写，绑定要求助手已发布。
	if platform.FeishuBots != nil && platform.FeishuBotBindings != nil {
		handle("GET /api/v1/admin/feishu-bots", handler.listFeishuBots)
		handleWrite("POST /api/v1/admin/feishu-bots", "feishu_bot", handler.createFeishuBot)
		handle("GET /api/v1/admin/feishu-bots/{id}", handler.getFeishuBot)
		handleWrite("PUT /api/v1/admin/feishu-bots/{id}", "feishu_bot", handler.updateFeishuBot)
		handleWrite("DELETE /api/v1/admin/feishu-bots/{id}", "feishu_bot", handler.deleteFeishuBot)
		handleWrite("POST /api/v1/admin/feishu-bots/{id}/test", "feishu_bot", handler.testFeishuBot)
		handle("GET /api/v1/feishu/bots", handler.listFeishuBotOptions)
		handleWrite("POST /api/v1/agents/{id}/feishu-bot", "agent", handler.bindFeishuBot)
		handleWrite("DELETE /api/v1/agents/{id}/feishu-bot", "agent", handler.unbindFeishuBot)
	}
	// 飞书事件回调由请求自身签名与 Verification Token 校验，禁止套用用户 JWT 鉴权。
	if platform.FeishuIngest != nil {
		feishuHandler := &feishuHTTPHandler{ingest: platform.FeishuIngest}
		mux.HandleFunc("POST /api/v1/feishu/events", feishuHandler.events)
		// 每个机器人使用独立回调地址，接入层按路径中的机器人 ID 选择应用凭据验签。
		mux.HandleFunc("POST /api/v1/feishu/events/{botID}", feishuHandler.events)
	}
}

// providerRequest 是供应商创建/更新的请求体，api_key 只写不读。
type providerRequest struct {
	// Name 供应商名称。
	Name string `json:"name"`
	// BaseURL HTTPS 基础地址。
	BaseURL string `json:"base_url"`
	// APIKey 明文密钥，仅写入，响应不返回。
	APIKey string `json:"api_key"`
	// Status 启停状态。
	Status biz.ProviderStatus `json:"status"`
	// DataRegion 数据区域。
	DataRegion string `json:"data_region"`
}

// modelRequest 是模型创建/更新的请求体。
type modelRequest struct {
	// ProviderID 所属供应商 ID。
	ProviderID uuid.UUID `json:"provider_id"`
	// RemoteModelName 供应商侧真实模型名。
	RemoteModelName string `json:"remote_model_name"`
	// Kind 模型用途：chat/embedding/rerank。
	Kind biz.ModelKind `json:"kind"`
	// ContextWindowTokens 上下文窗口，chat 必须大于等于 1M。
	ContextWindowTokens int64 `json:"context_window_tokens"`
	// MaxOutputTokens 最大输出 Token 数。
	MaxOutputTokens int `json:"max_output_tokens"`
	// Capabilities 协议能力。
	Capabilities biz.ModelCapabilities `json:"capabilities"`
	// InputPricePerMillion 每百万输入 Token 价格。
	InputPricePerMillion float64 `json:"input_price_per_million"`
	// OutputPricePerMillion 每百万输出 Token 价格。
	OutputPricePerMillion float64 `json:"output_price_per_million"`
	// DataRegion 数据区域。
	DataRegion string `json:"data_region"`
	// VerificationSource 元数据核验来源。
	VerificationSource string `json:"verification_source"`
	// VerifiedAt 核验时间（UTC）。
	VerifiedAt *time.Time `json:"verified_at"`
	// Status 启停状态。
	Status biz.ModelStatus `json:"status"`
}

// policyRequest 是策略创建/更新的请求体。
type policyRequest struct {
	// Name 策略名称。
	Name string `json:"name"`
	// MinContextTokens 最小上下文窗口，不得低于 1M。
	MinContextTokens int64 `json:"min_context_tokens"`
	// Rules 路由规则。
	Rules map[string]any `json:"rules"`
	// Candidates 有序候选模型。
	Candidates []biz.PolicyCandidate `json:"candidates"`
}

// agentRequest 是 Agent 创建/更新的请求体，不接受 user_id。
type agentRequest struct {
	// Name Agent 名称。
	Name string `json:"name"`
	// Description Agent 描述。
	Description string `json:"description"`
	// Mode 编排模式。
	Mode biz.AgentMode `json:"mode"`
	// ModelPolicyID 策略 ID，与版本成对出现。
	ModelPolicyID *uuid.UUID `json:"model_policy_id"`
	// ModelPolicyVersion 策略版本。
	ModelPolicyVersion *int `json:"model_policy_version"`
	// SkillIDs 绑定的 Skill。
	SkillIDs []uuid.UUID `json:"skill_ids"`
	// MCPIDs 绑定的 MCP。
	MCPIDs []uuid.UUID `json:"mcp_ids"`
}

// sessionRequest 是创建会话的请求体。
type sessionRequest struct {
	// AgentID 绑定的已发布 Agent。
	AgentID *uuid.UUID `json:"agent_id"`
	// Title 会话标题。
	Title string `json:"title"`
	// Source 来源渠道。
	Source biz.SessionSource `json:"source"`
}

// messageRequest 是追加消息的请求体；角色与用户身份由服务端强制。
type messageRequest struct {
	// Content 消息正文。
	Content string `json:"content"`
}

// memoryPolicyRequest 是更新会话记忆策略的请求体。
type memoryPolicyRequest struct {
	// MemoryPolicy 目标策略：private 私有、group 群聊话题、disabled 禁用记忆。
	MemoryPolicy biz.SessionMemoryPolicy `json:"memory_policy"`
}

// skillRequest 是 Skill 模板创建/更新的请求体。
type skillRequest struct {
	// Name Skill 名称。
	Name string `json:"name"`
	// Description Skill 描述。
	Description string `json:"description"`
	// PromptTemplate 提示词模板。
	PromptTemplate string `json:"prompt_template"`
	// Parameters 参数定义 JSON Schema。
	Parameters map[string]any `json:"parameters"`
	// Status 生命周期状态。
	Status biz.SkillStatus `json:"status"`
}

// mcpRequest 是 MCP 创建/更新的请求体，credentials 只写不读。
type mcpRequest struct {
	// Name MCP 名称。
	Name string `json:"name"`
	// Endpoint 远端 HTTPS 地址，需通过 SSRF 准入。
	Endpoint string `json:"endpoint"`
	// AuthType 鉴权类型。
	AuthType string `json:"auth_type"`
	// Credentials 鉴权凭证明文，仅写入时使用。
	Credentials string `json:"credentials"`
	// RiskLevel 风险等级。
	RiskLevel string `json:"risk_level"`
	// Status 启停状态。
	Status biz.MCPStatus `json:"status"`
	// ToolTimeouts 按原始工具名或前缀覆盖单次超时，单位秒。
	ToolTimeouts map[string]int `json:"tool_timeouts"`
}

// feishuBotRequest 是飞书机器人创建/更新请求体，密钥字段只写不读。
type feishuBotRequest struct {
	// Name 机器人展示名称。
	Name string `json:"name"`
	// AppID 飞书应用 App ID。
	AppID string `json:"app_id"`
	// AppSecret 应用密钥，更新时留空表示保留原值。
	AppSecret string `json:"app_secret"`
	// VerificationToken 事件订阅 Verification Token。
	VerificationToken string `json:"verification_token"`
	// EncryptKey 事件加密密钥。
	EncryptKey string `json:"encrypt_key"`
	// BotOpenID 机器人 open_id。
	BotOpenID string `json:"bot_open_id"`
	// Status 启停状态。
	Status biz.FeishuBotStatus `json:"status"`
}

// feishuBotBindRequest 是助手绑定机器人的请求体。
type feishuBotBindRequest struct {
	// BotID 目标机器人 ID。
	BotID uuid.UUID `json:"bot_id"`
}

// createSkill 处理 POST /api/v1/skills：创建 Skill 草稿。
func (h *platformHTTPHandler) createSkill(w http.ResponseWriter, r *http.Request) {
	var request skillRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	skill, err := h.service.Skills.Create(r.Context(), requestPrincipal(r), biz.SkillTemplate{
		Name: request.Name, Description: request.Description, PromptTemplate: request.PromptTemplate,
		Parameters: request.Parameters, Status: request.Status,
	})
	writePlatformResult(w, http.StatusCreated, skillResponse(skill), err)
}

// updateSkill 处理 PUT /api/v1/skills/{id}：更新 Skill 模板。
func (h *platformHTTPHandler) updateSkill(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	var request skillRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	skill, err := h.service.Skills.Update(r.Context(), requestPrincipal(r), biz.SkillTemplate{
		ID: id, Name: request.Name, Description: request.Description, PromptTemplate: request.PromptTemplate,
		Parameters: request.Parameters, Status: request.Status,
	})
	writePlatformResult(w, http.StatusOK, skillResponse(skill), err)
}

// deleteSkill 处理 DELETE /api/v1/skills/{id}：删除 Skill 模板。
func (h *platformHTTPHandler) deleteSkill(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	writePlatformResult(w, http.StatusNoContent, nil, h.service.Skills.Delete(r.Context(), requestPrincipal(r), id))
}

// getSkill 处理 GET /api/v1/skills/{id}：读取 Skill 模板。
func (h *platformHTTPHandler) getSkill(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	skill, err := h.service.Skills.Get(r.Context(), requestPrincipal(r), id)
	writePlatformResult(w, http.StatusOK, skillResponse(skill), err)
}

// listSkills 处理 GET /api/v1/skills：列出全部 Skill 模板。
func (h *platformHTTPHandler) listSkills(w http.ResponseWriter, r *http.Request) {
	skills, err := h.service.Skills.List(r.Context(), requestPrincipal(r))
	writePlatformResult(w, http.StatusOK, map[string]any{"items": mapItems(skills, skillResponse)}, err)
}

// createMCP 处理 POST /api/v1/mcps：创建 MCP 并加密凭证，端点通过 SSRF 准入。
func (h *platformHTTPHandler) createMCP(w http.ResponseWriter, r *http.Request) {
	var request mcpRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	mcp, err := h.service.MCPs.Create(r.Context(), requestPrincipal(r), mcpInput(request))
	writePlatformResult(w, http.StatusCreated, mcpResponse(mcp), err)
}

// updateMCP 处理 PUT /api/v1/mcps/{id}：更新 MCP，凭证为空时保留原密文。
func (h *platformHTTPHandler) updateMCP(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	var request mcpRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	mcp, err := h.service.MCPs.Update(r.Context(), requestPrincipal(r), id, mcpInput(request))
	writePlatformResult(w, http.StatusOK, mcpResponse(mcp), err)
}

// deleteMCP 处理 DELETE /api/v1/mcps/{id}：删除 MCP。
func (h *platformHTTPHandler) deleteMCP(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	writePlatformResult(w, http.StatusNoContent, nil, h.service.MCPs.Delete(r.Context(), requestPrincipal(r), id))
}

// getMCP 处理 GET /api/v1/mcps/{id}：读取 MCP 元数据。
func (h *platformHTTPHandler) getMCP(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	mcp, err := h.service.MCPs.Get(r.Context(), requestPrincipal(r), id)
	writePlatformResult(w, http.StatusOK, mcpResponse(mcp), err)
}

// listMCPs 处理 GET /api/v1/mcps：列出全部 MCP。
func (h *platformHTTPHandler) listMCPs(w http.ResponseWriter, r *http.Request) {
	mcps, err := h.service.MCPs.List(r.Context(), requestPrincipal(r))
	writePlatformResult(w, http.StatusOK, map[string]any{"items": mapItems(mcps, mcpResponse)}, err)
}

// testMCP 处理 POST /api/v1/mcps/{id}/test：验证 MCP 连通性并返回工具数量。
func (h *platformHTTPHandler) testMCP(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	result, err := h.service.MCPs.Inspect(r.Context(), requestPrincipal(r), id)
	if err != nil {
		writePlatformResult(w, http.StatusOK, nil, err)
		return
	}
	writePlatformResult(w, http.StatusOK, map[string]any{
		"ok": result.OK, "message": result.Message, "tool_count": len(result.Tools),
	}, nil)
}

// listMCPTools 处理 GET /api/v1/mcps/{id}/tools：列出 MCP 暴露的工具与参数 Schema。
func (h *platformHTTPHandler) listMCPTools(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	result, err := h.service.MCPs.Inspect(r.Context(), requestPrincipal(r), id)
	if err != nil {
		writePlatformResult(w, http.StatusOK, nil, err)
		return
	}
	items := make([]map[string]any, 0, len(result.Tools))
	for _, tool := range result.Tools {
		items = append(items, map[string]any{
			"name": tool.Name, "description": tool.Description, "input_schema": tool.InputSchema,
		})
	}
	writePlatformResult(w, http.StatusOK, map[string]any{
		"ok": result.OK, "message": result.Message, "items": items, "count": len(items),
	}, nil)
}

// createFeishuBot 处理 POST /api/v1/admin/feishu-bots：管理员登记飞书机器人。
func (h *platformHTTPHandler) createFeishuBot(w http.ResponseWriter, r *http.Request) {
	var request feishuBotRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	bot, err := h.service.FeishuBots.Create(r.Context(), requestPrincipal(r), feishuBotInput(request))
	writePlatformResult(w, http.StatusCreated, feishuBotResponse(bot), err)
}

// updateFeishuBot 处理 PUT /api/v1/admin/feishu-bots/{id}：更新机器人配置。
func (h *platformHTTPHandler) updateFeishuBot(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	var request feishuBotRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	bot, err := h.service.FeishuBots.Update(r.Context(), requestPrincipal(r), id, feishuBotInput(request))
	writePlatformResult(w, http.StatusOK, feishuBotResponse(bot), err)
}

// deleteFeishuBot 处理 DELETE /api/v1/admin/feishu-bots/{id}：删除机器人并解除绑定。
func (h *platformHTTPHandler) deleteFeishuBot(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	writePlatformResult(w, http.StatusNoContent, nil, h.service.FeishuBots.Delete(r.Context(), requestPrincipal(r), id))
}

// getFeishuBot 处理 GET /api/v1/admin/feishu-bots/{id}：读取机器人元数据。
func (h *platformHTTPHandler) getFeishuBot(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	bot, err := h.service.FeishuBots.Get(r.Context(), requestPrincipal(r), id)
	writePlatformResult(w, http.StatusOK, feishuBotResponse(bot), err)
}

// testFeishuBot 处理 POST /api/v1/admin/feishu-bots/{id}/test：验证应用凭据与飞书连通性。
func (h *platformHTTPHandler) testFeishuBot(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	result, err := h.service.FeishuBots.Test(r.Context(), requestPrincipal(r), id)
	if err != nil {
		writePlatformResult(w, http.StatusOK, nil, err)
		return
	}
	writePlatformResult(w, http.StatusOK, map[string]any{"ok": result.OK, "message": result.Message}, nil)
}

// listFeishuBots 处理 GET /api/v1/admin/feishu-bots：列出机器人及绑定助手。
func (h *platformHTTPHandler) listFeishuBots(w http.ResponseWriter, r *http.Request) {
	bots, err := h.service.FeishuBots.List(r.Context(), requestPrincipal(r))
	writePlatformResult(w, http.StatusOK, map[string]any{"items": mapItems(bots, feishuBotResponse)}, err)
}

// listFeishuBotOptions 处理 GET /api/v1/feishu/bots：返回当前用户可绑定的机器人。
func (h *platformHTTPHandler) listFeishuBotOptions(w http.ResponseWriter, r *http.Request) {
	options, err := h.service.FeishuBotBindings.List(r.Context(), requestPrincipal(r))
	writePlatformResult(w, http.StatusOK, map[string]any{"items": mapItems(options, feishuBotOptionResponse)}, err)
}

// bindFeishuBot 处理 POST /api/v1/agents/{id}/feishu-bot：把已发布助手绑定到机器人。
func (h *platformHTTPHandler) bindFeishuBot(w http.ResponseWriter, r *http.Request) {
	agentID, ok := pathUUID(w, r)
	if !ok {
		return
	}
	var request feishuBotBindRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	err := h.service.FeishuBotBindings.Bind(r.Context(), requestPrincipal(r), agentID, request.BotID)
	if err != nil {
		writePlatformResult(w, http.StatusOK, nil, err)
		return
	}
	writePlatformResult(w, http.StatusOK, map[string]string{"status": "bound"}, nil)
}

// unbindFeishuBot 处理 DELETE /api/v1/agents/{id}/feishu-bot：解除助手机器人绑定。
func (h *platformHTTPHandler) unbindFeishuBot(w http.ResponseWriter, r *http.Request) {
	agentID, ok := pathUUID(w, r)
	if !ok {
		return
	}
	writePlatformResult(w, http.StatusNoContent, nil, h.service.FeishuBotBindings.Unbind(r.Context(), requestPrincipal(r), agentID))
}

// createProvider 处理 POST /api/v1/providers：创建供应商并加密保存密钥，响应脱敏。
func (h *platformHTTPHandler) createProvider(w http.ResponseWriter, r *http.Request) {
	var request providerRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	provider, err := h.service.Providers.Create(r.Context(), requestPrincipal(r), providerInput(request))
	writePlatformResult(w, http.StatusCreated, providerResponse(provider), err)
}

// updateProvider 处理 PUT /api/v1/providers/{id}：更新供应商，可选轮换密钥。
func (h *platformHTTPHandler) updateProvider(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	var request providerRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	provider, err := h.service.Providers.Update(r.Context(), requestPrincipal(r), id, providerInput(request))
	writePlatformResult(w, http.StatusOK, providerResponse(provider), err)
}

// deleteProvider 处理 DELETE /api/v1/providers/{id}：删除供应商。
func (h *platformHTTPHandler) deleteProvider(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	writePlatformResult(w, http.StatusNoContent, nil, h.service.Providers.Delete(r.Context(), requestPrincipal(r), id))
}

// getProvider 处理 GET /api/v1/providers/{id}：读取供应商元数据。
func (h *platformHTTPHandler) getProvider(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	provider, err := h.service.Providers.Get(r.Context(), requestPrincipal(r), id)
	writePlatformResult(w, http.StatusOK, providerResponse(provider), err)
}

// listProviders 处理 GET /api/v1/providers：列出全部供应商。
func (h *platformHTTPHandler) listProviders(w http.ResponseWriter, r *http.Request) {
	providers, err := h.service.Providers.List(r.Context(), requestPrincipal(r))
	writePlatformResult(w, http.StatusOK, map[string]any{"items": mapItems(providers, providerResponse)}, err)
}

// listRemoteModels 处理 GET /api/v1/providers/{id}/models：从供应商站点拉取可用模型。
func (h *platformHTTPHandler) listRemoteModels(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	items, err := h.service.Providers.ListRemoteModels(r.Context(), requestPrincipal(r), id)
	writePlatformResult(w, http.StatusOK, map[string]any{"items": items}, err)
}

// createModel 处理 POST /api/v1/models：创建模型，1M+ 与核验准入由领域层强制。
func (h *platformHTTPHandler) createModel(w http.ResponseWriter, r *http.Request) {
	var request modelRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	model, err := h.service.Models.Create(r.Context(), requestPrincipal(r), modelInput(request))
	writePlatformResult(w, http.StatusCreated, modelResponse(model), err)
}

// updateModel 处理 PUT /api/v1/models/{id}：更新模型元数据。
func (h *platformHTTPHandler) updateModel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	var request modelRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	model := modelInput(request)
	model.ID = id
	updated, err := h.service.Models.Update(r.Context(), requestPrincipal(r), model)
	writePlatformResult(w, http.StatusOK, modelResponse(updated), err)
}

// deleteModel 处理 DELETE /api/v1/models/{id}：删除模型。
func (h *platformHTTPHandler) deleteModel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	writePlatformResult(w, http.StatusNoContent, nil, h.service.Models.Delete(r.Context(), requestPrincipal(r), id))
}

// getModel 处理 GET /api/v1/models/{id}：读取模型。
func (h *platformHTTPHandler) getModel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	model, err := h.service.Models.Get(r.Context(), requestPrincipal(r), id)
	writePlatformResult(w, http.StatusOK, modelResponse(model), err)
}

// listModels 处理 GET /api/v1/models：按可选 provider_id 过滤列出模型。
func (h *platformHTTPHandler) listModels(w http.ResponseWriter, r *http.Request) {
	var providerID *uuid.UUID
	if raw := r.URL.Query().Get("provider_id"); raw != "" {
		parsed, err := uuid.Parse(raw)
		if err != nil {
			writeErrorCode(w, http.StatusBadRequest, "invalid_provider_id")
			return
		}
		providerID = &parsed
	}
	models, err := h.service.Models.List(r.Context(), requestPrincipal(r), providerID)
	writePlatformResult(w, http.StatusOK, map[string]any{"items": mapItems(models, modelResponse)}, err)
}

// createPolicy 处理 POST /api/v1/model-policies：创建草稿策略。
func (h *platformHTTPHandler) createPolicy(w http.ResponseWriter, r *http.Request) {
	var request policyRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	policy, err := h.service.Policies.Create(r.Context(), requestPrincipal(r), policyInput(request))
	writePlatformResult(w, http.StatusCreated, policyResponse(policy), err)
}

// updatePolicy 处理 PUT /api/v1/model-policies/{id}：更新草稿策略。
func (h *platformHTTPHandler) updatePolicy(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	var request policyRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	policy := policyInput(request)
	policy.ID = id
	updated, err := h.service.Policies.Update(r.Context(), requestPrincipal(r), policy)
	writePlatformResult(w, http.StatusOK, policyResponse(updated), err)
}

// deletePolicy 处理 DELETE /api/v1/model-policies/{id}：删除策略。
func (h *platformHTTPHandler) deletePolicy(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	writePlatformResult(w, http.StatusNoContent, nil, h.service.Policies.Delete(r.Context(), requestPrincipal(r), id))
}

// getPolicy 处理 GET /api/v1/model-policies/{id}：读取策略及候选。
func (h *platformHTTPHandler) getPolicy(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	policy, err := h.service.Policies.Get(r.Context(), requestPrincipal(r), id)
	writePlatformResult(w, http.StatusOK, policyResponse(policy), err)
}

// listPolicies 处理 GET /api/v1/model-policies：列出全部策略。
func (h *platformHTTPHandler) listPolicies(w http.ResponseWriter, r *http.Request) {
	policies, err := h.service.Policies.List(r.Context(), requestPrincipal(r))
	writePlatformResult(w, http.StatusOK, map[string]any{"items": mapItems(policies, policyResponse)}, err)
}

// createPolicyVersion 处理 POST /api/v1/model-policies/{id}/versions：创建新草稿版本。
func (h *platformHTTPHandler) createPolicyVersion(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	policy, err := h.service.Policies.CreateVersion(r.Context(), requestPrincipal(r), id)
	writePlatformResult(w, http.StatusCreated, policyResponse(policy), err)
}

// publishPolicy 处理 POST /api/v1/model-policies/{id}/publish：发布草稿策略。
func (h *platformHTTPHandler) publishPolicy(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	policy, err := h.service.Policies.Publish(r.Context(), requestPrincipal(r), id)
	writePlatformResult(w, http.StatusOK, policyResponse(policy), err)
}

// createAgent 处理 POST /api/v1/agents：为当前用户创建 Agent 草稿。
func (h *platformHTTPHandler) createAgent(w http.ResponseWriter, r *http.Request) {
	var request agentRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	agent, err := h.service.Agents.Create(r.Context(), requestPrincipal(r), agentInput(request))
	writePlatformResult(w, http.StatusCreated, agentResponse(agent), err)
}

// updateAgent 处理 PUT /api/v1/agents/{id}：更新当前用户 Agent 草稿。
func (h *platformHTTPHandler) updateAgent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	var request agentRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	agent := agentInput(request)
	agent.ID = id
	updated, err := h.service.Agents.Update(r.Context(), requestPrincipal(r), agent)
	writePlatformResult(w, http.StatusOK, agentResponse(updated), err)
}

// deleteAgent 处理 DELETE /api/v1/agents/{id}：删除当前用户草稿版本。
func (h *platformHTTPHandler) deleteAgent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	writePlatformResult(w, http.StatusNoContent, nil, h.service.Agents.Delete(r.Context(), requestPrincipal(r), id))
}

// getAgent 处理 GET /api/v1/agents/{id}：读取当前用户 Agent。
func (h *platformHTTPHandler) getAgent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	agent, err := h.service.Agents.Get(r.Context(), requestPrincipal(r), id)
	writePlatformResult(w, http.StatusOK, agentResponse(agent), err)
}

// listAgents 处理 GET /api/v1/agents：游标分页列出当前用户 Agent，默认隐藏已归档版本。
func (h *platformHTTPHandler) listAgents(w http.ResponseWriter, r *http.Request) {
	limit, cursor, ok := pageRequest(w, r)
	if !ok {
		return
	}
	rawInclude := r.URL.Query().Get("include_archived")
	includeArchived := rawInclude == "1" || strings.EqualFold(rawInclude, "true")
	page, err := h.service.Agents.List(r.Context(), requestPrincipal(r), includeArchived, limit, cursor)
	writePlatformResult(w, http.StatusOK, map[string]any{"items": mapItems(page.Items, agentResponse), "next_cursor": page.NextCursor}, err)
}

// listAdminAgents 处理 GET /api/v1/admin/agents：管理员跨用户只读查看助手列表。
func (h *platformHTTPHandler) listAdminAgents(w http.ResponseWriter, r *http.Request) {
	limit, cursor, ok := pageRequest(w, r)
	if !ok {
		return
	}
	filter := biz.AdminAgentFilter{Status: biz.AgentStatus(r.URL.Query().Get("status"))}
	if raw := r.URL.Query().Get("user_id"); raw != "" {
		parsed, err := uuid.Parse(raw)
		if err != nil {
			writeErrorCode(w, http.StatusBadRequest, "invalid_user_id")
			return
		}
		filter.UserID = &parsed
	}
	page, err := h.service.Agents.ListAll(r.Context(), requestPrincipal(r), filter, limit, cursor)
	if err != nil {
		writePlatformResult(w, http.StatusOK, nil, err)
		return
	}
	items := make([]map[string]any, 0, len(page.Items))
	for _, summary := range page.Items {
		items = append(items, adminAgentResponse(summary))
	}
	writePlatformResult(w, http.StatusOK, map[string]any{"items": items, "next_cursor": page.NextCursor}, nil)
}

// adminDeleteAgent 处理 DELETE /api/v1/admin/agents/{id}：管理员跨用户删除助手版本。
func (h *platformHTTPHandler) adminDeleteAgent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	err := h.service.Agents.AdminDelete(r.Context(), requestPrincipal(r), id)
	writePlatformResult(w, http.StatusOK, map[string]any{"deleted": true}, err)
}

// createAgentVersion 处理 POST /api/v1/agents/{id}/versions：创建新草稿版本。
func (h *platformHTTPHandler) createAgentVersion(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	agent, err := h.service.Agents.CreateVersion(r.Context(), requestPrincipal(r), id)
	writePlatformResult(w, http.StatusCreated, agentResponse(agent), err)
}

// publishAgent 处理 POST /api/v1/agents/{id}/publish：发布并固化配置快照。
func (h *platformHTTPHandler) publishAgent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	agent, err := h.service.Agents.Publish(r.Context(), requestPrincipal(r), id)
	writePlatformResult(w, http.StatusOK, agentResponse(agent), err)
}

// unpublishAgent 处理 POST /api/v1/agents/{id}/unpublish：已发布版本回退为可编辑草稿。
func (h *platformHTTPHandler) unpublishAgent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	agent, err := h.service.Agents.Unpublish(r.Context(), requestPrincipal(r), id)
	writePlatformResult(w, http.StatusOK, agentResponse(agent), err)
}

// archiveAgent 处理 POST /api/v1/agents/{id}/archive：归档旧版本并从默认列表隐藏。
func (h *platformHTTPHandler) archiveAgent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	agent, err := h.service.Agents.Archive(r.Context(), requestPrincipal(r), id)
	writePlatformResult(w, http.StatusOK, agentResponse(agent), err)
}

// createSession 处理 POST /api/v1/sessions：为当前用户创建会话。
func (h *platformHTTPHandler) createSession(w http.ResponseWriter, r *http.Request) {
	var request sessionRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	session, err := h.service.Sessions.Create(r.Context(), requestPrincipal(r), biz.Session{AgentID: request.AgentID, Title: request.Title, Source: request.Source})
	writePlatformResult(w, http.StatusCreated, sessionResponse(session), err)
}

// getSession 处理 GET /api/v1/sessions/{id}：读取当前用户会话。
func (h *platformHTTPHandler) getSession(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	session, err := h.service.Sessions.Get(r.Context(), requestPrincipal(r), id)
	writePlatformResult(w, http.StatusOK, sessionResponse(session), err)
}

// listSessions 处理 GET /api/v1/sessions：游标分页列出当前用户会话。
func (h *platformHTTPHandler) listSessions(w http.ResponseWriter, r *http.Request) {
	limit, cursor, ok := pageRequest(w, r)
	if !ok {
		return
	}
	page, err := h.service.Sessions.List(r.Context(), requestPrincipal(r), limit, cursor)
	writePlatformResult(w, http.StatusOK, map[string]any{"items": mapItems(page.Items, sessionResponse), "next_cursor": page.NextCursor}, err)
}

// archiveSession 处理 POST /api/v1/sessions/{id}/archive：归档活跃会话。
func (h *platformHTTPHandler) archiveSession(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	session, err := h.service.Sessions.Archive(r.Context(), requestPrincipal(r), id)
	writePlatformResult(w, http.StatusOK, sessionResponse(session), err)
}

// updateSessionMemoryPolicy 处理 PUT /api/v1/sessions/{id}/memory-policy：更新会话记忆策略。
func (h *platformHTTPHandler) updateSessionMemoryPolicy(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	var request memoryPolicyRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	session, err := h.service.Sessions.SetMemoryPolicy(r.Context(), requestPrincipal(r), id, request.MemoryPolicy)
	writePlatformResult(w, http.StatusOK, sessionResponse(session), err)
}

// unbindFeishu 处理 POST /api/v1/sessions/{id}/feishu-unbind：解绑该会话的飞书话题。
// 解绑后同一飞书话题的下一条 @ 消息会创建全新会话。
func (h *platformHTTPHandler) unbindFeishu(w http.ResponseWriter, r *http.Request) {
	h.changeFeishuBinding(w, r, "released", h.service.FeishuBindings.Unbind)
}

// closeFeishu 处理 POST /api/v1/sessions/{id}/feishu-close：关闭飞书话题绑定。
// 关闭后话题键仍被占用，只是不再自动回复，可通过恢复接口重新启用。
func (h *platformHTTPHandler) closeFeishu(w http.ResponseWriter, r *http.Request) {
	h.changeFeishuBinding(w, r, "affected", h.service.FeishuBindings.Close)
}

// archiveFeishu 处理 POST /api/v1/sessions/{id}/feishu-archive：归档飞书话题绑定。
func (h *platformHTTPHandler) archiveFeishu(w http.ResponseWriter, r *http.Request) {
	h.changeFeishuBinding(w, r, "affected", h.service.FeishuBindings.Archive)
}

// restoreFeishu 处理 POST /api/v1/sessions/{id}/feishu-restore：恢复已关闭或已归档的绑定。
func (h *platformHTTPHandler) restoreFeishu(w http.ResponseWriter, r *http.Request) {
	h.changeFeishuBinding(w, r, "affected", h.service.FeishuBindings.Restore)
}

// changeFeishuBinding 统一解析路径 ID 并执行绑定状态变更，返回受影响的绑定数。
// 所有变更均在校验会话归属后按 user_id 限定，避免跨用户操作。
func (h *platformHTTPHandler) changeFeishuBinding(
	w http.ResponseWriter,
	r *http.Request,
	resultKey string,
	change func(context.Context, biz.Principal, uuid.UUID) (int, error),
) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	affected, err := change(r.Context(), requestPrincipal(r), id)
	writePlatformResult(w, http.StatusOK, map[string]any{resultKey: affected}, err)
}

// appendMessage 处理 POST /api/v1/sessions/{id}/messages：追加用户消息，正文上限 4MB。
func (h *platformHTTPHandler) appendMessage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	var request messageRequest
	if !decodeJSON(w, r, &request, 4*1024*1024+64*1024) {
		return
	}
	message, err := h.service.Sessions.AppendMessage(r.Context(), requestPrincipal(r), biz.Message{SessionID: id, Content: request.Content})
	writePlatformResult(w, http.StatusCreated, messageResponse(message), err)
}

// listMessages 处理 GET /api/v1/sessions/{id}/messages：游标分页列出会话消息。
func (h *platformHTTPHandler) listMessages(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	limit, cursor, ok := pageRequest(w, r)
	if !ok {
		return
	}
	page, err := h.service.Sessions.ListMessages(r.Context(), requestPrincipal(r), id, limit, cursor)
	writePlatformResult(w, http.StatusOK, map[string]any{"items": mapItems(page.Items, messageResponse), "next_cursor": page.NextCursor}, err)
}

// requestPrincipal 从 Context 读取鉴权中间件注入的身份。
func requestPrincipal(r *http.Request) biz.Principal {
	principal, _ := biz.PrincipalFromContext(r.Context())
	return principal
}

// pathUUID 解析路径参数 {id}，非法 UUID 返回 400。
func pathUUID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErrorCode(w, http.StatusBadRequest, "invalid_resource_id")
		return uuid.Nil, false
	}
	return id, true
}

// pageRequest 解析分页参数：limit 限定 1-100，cursor 最长 2048 字节。
func pageRequest(w http.ResponseWriter, r *http.Request) (int, string, bool) {
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 100 {
			writeErrorCode(w, http.StatusBadRequest, "invalid_page_limit")
			return 0, "", false
		}
		limit = parsed
	}
	cursor := r.URL.Query().Get("cursor")
	if len(cursor) > 2048 {
		writeErrorCode(w, http.StatusBadRequest, "invalid_page_cursor")
		return 0, "", false
	}
	return limit, cursor, true
}

// writePlatformResult 统一写出成功结果或映射领域错误。
func writePlatformResult(w http.ResponseWriter, status int, result any, err error) {
	if err != nil {
		writePlatformError(w, err)
		return
	}
	if status == http.StatusNoContent {
		w.WriteHeader(status)
		return
	}
	writeJSON(w, status, result)
}

// writePlatformError 将领域错误映射为稳定 HTTP 状态与业务错误码。
func writePlatformError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, biz.ErrForbidden):
		writeErrorCode(w, http.StatusForbidden, "forbidden")
	case errors.Is(err, biz.ErrQuotaExceeded):
		writeErrorCode(w, http.StatusForbidden, "quota_exceeded")
	case errors.Is(err, biz.ErrBudgetExceeded):
		writeErrorCode(w, http.StatusTooManyRequests, "budget_exceeded")
	case errors.Is(err, biz.ErrResourceNotFound):
		writeErrorCode(w, http.StatusNotFound, "resource_not_found")
	case errors.Is(err, biz.ErrResourceConflict):
		writeErrorCode(w, http.StatusConflict, "resource_conflict")
	case errors.Is(err, biz.ErrInvalidConfig):
		writeErrorCode(w, http.StatusBadRequest, "invalid_configuration")
	case errors.Is(err, context.DeadlineExceeded):
		// 数据库/依赖调用超时属于暂时性不可用：返回 503 与 Retry-After，提示客户端稍后重试。
		w.Header().Set("Retry-After", "5")
		writeErrorCode(w, http.StatusServiceUnavailable, "dependency_timeout")
	default:
		writeErrorCode(w, http.StatusInternalServerError, "internal_error")
	}
}

// providerInput 将 HTTP 请求体转换为供应商领域入参。
func providerInput(request providerRequest) biz.ProviderInput {
	return biz.ProviderInput{Name: request.Name, BaseURL: request.BaseURL, APIKey: request.APIKey, Status: request.Status, DataRegion: request.DataRegion}
}

// modelInput 将 HTTP 请求体转换为模型领域实体。
func modelInput(request modelRequest) biz.Model {
	return biz.Model{
		ProviderID: request.ProviderID, RemoteModelName: request.RemoteModelName, Kind: request.Kind,
		ContextWindowTokens: request.ContextWindowTokens, MaxOutputTokens: request.MaxOutputTokens,
		Capabilities: request.Capabilities, InputPricePerMillion: request.InputPricePerMillion,
		OutputPricePerMillion: request.OutputPricePerMillion, DataRegion: request.DataRegion,
		VerificationSource: request.VerificationSource, VerifiedAt: request.VerifiedAt, Status: request.Status,
	}
}

// policyInput 将 HTTP 请求体转换为策略领域实体。
func policyInput(request policyRequest) biz.ModelPolicy {
	return biz.ModelPolicy{Name: request.Name, MinContextTokens: request.MinContextTokens, Rules: request.Rules, Candidates: request.Candidates}
}

// agentInput 将 HTTP 请求体转换为 Agent 领域实体（user_id 不来自客户端）。
func agentInput(request agentRequest) biz.Agent {
	return biz.Agent{Name: request.Name, Description: request.Description, Mode: request.Mode, ModelPolicyID: request.ModelPolicyID, ModelPolicyVersion: request.ModelPolicyVersion, SkillIDs: request.SkillIDs, MCPIDs: request.MCPIDs}
}

// mcpInput 将 HTTP 请求体转换为 MCP 入参。
func mcpInput(request mcpRequest) biz.MCPInput {
	return biz.MCPInput{
		Name: request.Name, Endpoint: request.Endpoint, AuthType: request.AuthType,
		Credentials: request.Credentials, RiskLevel: request.RiskLevel, Status: request.Status,
		ToolTimeouts: request.ToolTimeouts,
	}
}

// skillResponse 构造 Skill 出参。
func skillResponse(skill biz.SkillTemplate) map[string]any {
	return map[string]any{
		"id": skill.ID, "name": skill.Name, "version": skill.Version, "description": skill.Description,
		"prompt_template": skill.PromptTemplate, "parameters": skill.Parameters, "status": skill.Status,
		"created_at": skill.CreatedAt, "updated_at": skill.UpdatedAt,
	}
}

// mcpResponse 构造 MCP 出参，刻意不包含凭证密文与明文。
func mcpResponse(mcp biz.MCP) map[string]any {
	return map[string]any{
		"id": mcp.ID, "name": mcp.Name, "endpoint": mcp.Endpoint, "auth_type": mcp.AuthType,
		"key_version": mcp.KeyVersion, "risk_level": mcp.RiskLevel, "status": mcp.Status,
		"tool_timeouts": mcp.ToolTimeouts,
		"created_at":    mcp.CreatedAt, "updated_at": mcp.UpdatedAt,
	}
}

// feishuBotInput 把请求体转换为领域入参。
func feishuBotInput(request feishuBotRequest) biz.FeishuBotInput {
	return biz.FeishuBotInput{
		Name: request.Name, AppID: request.AppID, AppSecret: request.AppSecret,
		VerificationToken: request.VerificationToken, EncryptKey: request.EncryptKey,
		BotOpenID: request.BotOpenID, Status: request.Status,
	}
}

// feishuBotResponse 构造机器人出参，刻意不包含应用密钥、Verification Token 与 Encrypt Key。
func feishuBotResponse(bot biz.FeishuBot) map[string]any {
	return map[string]any{
		"id": bot.ID, "name": bot.Name, "app_id": bot.AppID, "bot_open_id": bot.BotOpenID,
		"key_version": bot.KeyVersion, "status": bot.Status,
		"bound_user_id": bot.BoundUserID, "bound_username": bot.BoundUsername,
		"bound_agent_id": bot.BoundAgentID, "bound_agent_name": bot.BoundAgentName,
		"bound_agent_version": bot.BoundAgentVersion,
		"created_at":          bot.CreatedAt, "updated_at": bot.UpdatedAt,
	}
}

// feishuBotOptionResponse 构造普通用户可见的机器人选项出参。
func feishuBotOptionResponse(option biz.FeishuBotOption) map[string]any {
	return map[string]any{
		"id": option.ID, "name": option.Name, "bot_open_id": option.BotOpenID, "status": option.Status,
		"bound_agent_id": option.BoundAgentID, "bound_agent_name": option.BoundAgentName, "available": option.Available,
	}
}

// providerResponse 构造供应商出参，刻意不包含密文与明文密钥。
func providerResponse(provider biz.Provider) map[string]any {
	return map[string]any{
		"id": provider.ID, "name": provider.Name, "provider_type": provider.ProviderType, "base_url": provider.BaseURL,
		"key_version": provider.KeyVersion, "status": provider.Status, "data_region": provider.DataRegion,
		"created_at": provider.CreatedAt, "updated_at": provider.UpdatedAt,
	}
}

// modelResponse 构造模型出参。
func modelResponse(model biz.Model) map[string]any {
	return map[string]any{
		"id": model.ID, "provider_id": model.ProviderID, "remote_model_name": model.RemoteModelName, "kind": model.Kind,
		"context_window_tokens": model.ContextWindowTokens, "max_output_tokens": model.MaxOutputTokens,
		"capabilities": model.Capabilities, "input_price_per_million": model.InputPricePerMillion,
		"output_price_per_million": model.OutputPricePerMillion, "data_region": model.DataRegion,
		"verification_source": model.VerificationSource, "verified_at": model.VerifiedAt, "status": model.Status,
		"created_at": model.CreatedAt, "updated_at": model.UpdatedAt,
	}
}

// policyResponse 构造策略出参。
func policyResponse(policy biz.ModelPolicy) map[string]any {
	return map[string]any{
		"id": policy.ID, "name": policy.Name, "version": policy.Version, "min_context_tokens": policy.MinContextTokens,
		"rules": policy.Rules, "status": policy.Status, "candidates": policy.Candidates,
		"created_at": policy.CreatedAt, "updated_at": policy.UpdatedAt,
	}
}

// agentResponse 构造 Agent 出参，包含发布后的配置快照。
func agentResponse(agent biz.Agent) map[string]any {
	return map[string]any{
		"id": agent.ID, "user_id": agent.UserID, "name": agent.Name, "description": agent.Description, "mode": agent.Mode,
		"model_policy_id": agent.ModelPolicyID, "model_policy_version": agent.ModelPolicyVersion, "version": agent.Version,
		"config_snapshot": agent.ConfigSnapshot, "skill_ids": agent.SkillIDs, "mcp_ids": agent.MCPIDs, "status": agent.Status,
		"created_at": agent.CreatedAt, "updated_at": agent.UpdatedAt,
	}
}

// adminAgentResponse 构造管理员全局助手出参；只返回审计摘要，不暴露配置快照与绑定明细。
func adminAgentResponse(summary biz.AdminAgent) map[string]any {
	agent := summary.Agent
	return map[string]any{
		"id": agent.ID, "user_id": agent.UserID, "username": summary.Username,
		"name": agent.Name, "description": agent.Description, "mode": agent.Mode, "status": agent.Status,
		"version": agent.Version, "model_policy_id": agent.ModelPolicyID, "model_policy_version": agent.ModelPolicyVersion,
		"created_at": agent.CreatedAt, "updated_at": agent.UpdatedAt,
	}
}

// sessionResponse 构造会话出参。
func sessionResponse(session biz.Session) map[string]any {
	return map[string]any{
		"id": session.ID, "agent_id": session.AgentID, "title": session.Title, "source": session.Source,
		"status": session.Status, "version": session.Version, "last_message_at": session.LastMessageAt,
		"memory_policy": session.MemoryPolicy,
		"created_at":    session.CreatedAt, "updated_at": session.UpdatedAt,
	}
}

// messageResponse 构造消息出参。
func messageResponse(message biz.Message) map[string]any {
	return map[string]any{
		"id": message.ID, "session_id": message.SessionID, "role": message.Role, "content": message.Content,
		"tool_calls": message.ToolCalls, "token_count": message.TokenCount, "created_at": message.CreatedAt,
	}
}

// mapItems 批量转换列表项为出参结构。
func mapItems[T any](items []T, convert func(T) map[string]any) []map[string]any {
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		result = append(result, convert(item))
	}
	return result
}
