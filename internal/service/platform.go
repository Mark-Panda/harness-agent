package service

import (
	"errors"

	"github.com/Mark-Panda/harness-agent/internal/biz"
)

// PlatformService 聚合平台配置与用户对话的应用服务，供 HTTP/gRPC 适配层调用。
type PlatformService struct {
	// Providers 供应商管理用例（仅管理员可写）。
	Providers *biz.ProviderService
	// Models 模型目录管理用例（仅管理员可写）。
	Models *biz.ModelService
	// Policies 版本化路由策略用例（仅管理员可写）。
	Policies *biz.PolicyService
	// Skills 平台级 Skill 模板管理用例（仅管理员可写）。
	Skills *biz.SkillService
	// MCPs 平台级 MCP 接入管理用例（仅管理员可写）。
	MCPs *biz.MCPService
	// Agents 当前用户 Agent 草稿、版本与发布用例。
	Agents *biz.AgentService
	// Sessions 当前用户会话与消息用例。
	Sessions *biz.SessionService
	// Runs 当前用户 Run 创建、取消与事件读取用例。
	Runs *biz.RunService
	// Idempotency 写接口幂等协议服务。
	Idempotency *biz.IdempotencyService
	// Deletions 异步删除编排服务，可为空表示未启用。
	Deletions *biz.DeletionService
	// Exports 会话与长期记忆导出服务，可为空表示未启用。
	Exports *biz.ExportService
	// Routing 模型路由健康与熔断状态查询服务，可为空表示未启用。
	Routing *biz.RoutingHealthService
	// Vectors 长期记忆向量能力状态查询服务，可为空表示未装配。
	Vectors *biz.VectorStatusService
	// Plans 计划查询、暂停、恢复与人工终止服务，可为空表示未启用。
	Plans *biz.PlanService
	// FeishuIngest 飞书事件接入服务；为空表示飞书未启用，回调路由不会注册。
	FeishuIngest *biz.FeishuIngestService
	// FeishuBindings 飞书话题解绑服务；为空表示飞书未启用，解绑路由不会注册。
	FeishuBindings *biz.FeishuBindingService
	// FeishuBots 飞书机器人注册管理服务，仅管理员可写。
	FeishuBots *biz.FeishuBotService
	// FeishuBotBindings 助手绑定飞书机器人的用户侧服务。
	FeishuBotBindings *biz.FeishuBotBindingService
}

// WithFeishuBots 附加飞书机器人注册与助手绑定能力。
func (s *PlatformService) WithFeishuBots(bots *biz.FeishuBotService, bindings *biz.FeishuBotBindingService) *PlatformService {
	s.FeishuBots = bots
	s.FeishuBotBindings = bindings
	return s
}

// WithFeishuIngest 附加飞书事件接入能力。
func (s *PlatformService) WithFeishuIngest(ingest *biz.FeishuIngestService) *PlatformService {
	s.FeishuIngest = ingest
	return s
}

// WithFeishuBindings 附加飞书话题解绑能力。
func (s *PlatformService) WithFeishuBindings(bindings *biz.FeishuBindingService) *PlatformService {
	s.FeishuBindings = bindings
	return s
}

// WithPlans 附加 Plan-and-Execute 管理能力。
func (s *PlatformService) WithPlans(plans *biz.PlanService) *PlatformService {
	s.Plans = plans
	return s
}

// WithDeletion 附加删除编排与导出能力；缺失时对应 HTTP 路由不会注册。
func (s *PlatformService) WithDeletion(deletions *biz.DeletionService, exports *biz.ExportService) *PlatformService {
	s.Deletions = deletions
	s.Exports = exports
	return s
}

// WithVectorStatus 附加长期记忆向量状态查询能力。
func (s *PlatformService) WithVectorStatus(vectors *biz.VectorStatusService) *PlatformService {
	s.Vectors = vectors
	return s
}

// WithRoutingHealth 附加模型路由健康查询能力。
func (s *PlatformService) WithRoutingHealth(routing *biz.RoutingHealthService) *PlatformService {
	s.Routing = routing
	return s
}

// NewPlatformService 构造平台应用服务，任一依赖缺失时返回错误，避免空指针路由。
func NewPlatformService(providers *biz.ProviderService, models *biz.ModelService, policies *biz.PolicyService, skills *biz.SkillService, mcps *biz.MCPService, agents *biz.AgentService, sessions *biz.SessionService, runs *biz.RunService, idempotency *biz.IdempotencyService) (*PlatformService, error) {
	if providers == nil || models == nil || policies == nil || skills == nil || mcps == nil || agents == nil || sessions == nil || runs == nil || idempotency == nil {
		return nil, errors.New("platform application services are required")
	}
	return &PlatformService{Providers: providers, Models: models, Policies: policies, Skills: skills, MCPs: mcps, Agents: agents, Sessions: sessions, Runs: runs, Idempotency: idempotency}, nil
}
