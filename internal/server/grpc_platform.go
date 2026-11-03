package server

import (
	"context"
	"math"
	"time"

	harnessv1 "github.com/Mark-Panda/harness-agent/api/harness/v1"
	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/service"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// platformGRPCServer 实现 Protobuf 定义的五类平台服务，转换后复用同一套领域服务。
type platformGRPCServer struct {
	harnessv1.UnimplementedProviderServiceServer
	harnessv1.UnimplementedModelCatalogServiceServer
	harnessv1.UnimplementedModelPolicyServiceServer
	harnessv1.UnimplementedAgentServiceServer
	harnessv1.UnimplementedConversationServiceServer
	platform *service.PlatformService
}

// CreateProvider 处理 gRPC 供应商创建：管理员校验与密钥加密由领域层完成。
func (s *platformGRPCServer) CreateProvider(ctx context.Context, request *harnessv1.CreateProviderRequest) (*harnessv1.CreateProviderResponse, error) {
	principal, err := grpcPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	provider, err := s.platform.Providers.Create(ctx, principal, biz.ProviderInput{
		Name: request.GetName(), BaseURL: request.GetBaseUrl(), APIKey: request.GetApiKey(),
		Status: biz.ProviderStatus(request.GetStatus()), DataRegion: request.GetDataRegion(),
	})
	if err != nil {
		return nil, grpcError(err)
	}
	return &harnessv1.CreateProviderResponse{Provider: toProtoProvider(provider)}, nil
}

// UpdateProvider 处理 gRPC 供应商更新，api_key 为空时保留原密文。
func (s *platformGRPCServer) UpdateProvider(ctx context.Context, request *harnessv1.UpdateProviderRequest) (*harnessv1.UpdateProviderResponse, error) {
	principal, err := grpcPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	providerID, err := parseUUID(request.GetId())
	if err != nil {
		return nil, err
	}
	provider, err := s.platform.Providers.Update(ctx, principal, providerID, biz.ProviderInput{
		Name: request.GetName(), BaseURL: request.GetBaseUrl(), APIKey: request.GetApiKey(),
		Status: biz.ProviderStatus(request.GetStatus()), DataRegion: request.GetDataRegion(),
	})
	if err != nil {
		return nil, grpcError(err)
	}
	return &harnessv1.UpdateProviderResponse{Provider: toProtoProvider(provider)}, nil
}

// GetProvider 处理 gRPC 供应商读取。
func (s *platformGRPCServer) GetProvider(ctx context.Context, request *harnessv1.GetProviderRequest) (*harnessv1.GetProviderResponse, error) {
	principal, err := grpcPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	providerID, err := parseUUID(request.GetId())
	if err != nil {
		return nil, err
	}
	provider, err := s.platform.Providers.Get(ctx, principal, providerID)
	if err != nil {
		return nil, grpcError(err)
	}
	return &harnessv1.GetProviderResponse{Provider: toProtoProvider(provider)}, nil
}

// DeleteProvider 处理 gRPC 供应商删除。
func (s *platformGRPCServer) DeleteProvider(ctx context.Context, request *harnessv1.DeleteProviderRequest) (*harnessv1.DeleteProviderResponse, error) {
	principal, err := grpcPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	providerID, err := parseUUID(request.GetId())
	if err != nil {
		return nil, err
	}
	if err := s.platform.Providers.Delete(ctx, principal, providerID); err != nil {
		return nil, grpcError(err)
	}
	return &harnessv1.DeleteProviderResponse{}, nil
}

// ListProviders 处理 gRPC 供应商列表。
func (s *platformGRPCServer) ListProviders(ctx context.Context, _ *harnessv1.ListProvidersRequest) (*harnessv1.ListProvidersResponse, error) {
	principal, err := grpcPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	providers, err := s.platform.Providers.List(ctx, principal)
	if err != nil {
		return nil, grpcError(err)
	}
	response := &harnessv1.ListProvidersResponse{Items: make([]*harnessv1.Provider, 0, len(providers))}
	for _, provider := range providers {
		response.Items = append(response.Items, toProtoProvider(provider))
	}
	return response, nil
}

// CreateModel 处理 gRPC 模型创建。
func (s *platformGRPCServer) CreateModel(ctx context.Context, request *harnessv1.CreateModelRequest) (*harnessv1.CreateModelResponse, error) {
	principal, err := grpcPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	model, err := fromProtoModel(request.GetProviderId(), "", request.GetRemoteModelName(), request.GetKind(),
		request.GetContextWindowTokens(), request.GetMaxOutputTokens(), request.GetCapabilities(), request.GetInputPricePerMillion(),
		request.GetOutputPricePerMillion(), request.GetDataRegion(), request.GetVerificationSource(), request.GetVerifiedAt(), request.GetStatus())
	if err != nil {
		return nil, err
	}
	model, err = s.platform.Models.Create(ctx, principal, model)
	if err != nil {
		return nil, grpcError(err)
	}
	return &harnessv1.CreateModelResponse{Model: toProtoModel(model)}, nil
}

// UpdateModel 处理 gRPC 模型更新。
func (s *platformGRPCServer) UpdateModel(ctx context.Context, request *harnessv1.UpdateModelRequest) (*harnessv1.UpdateModelResponse, error) {
	principal, err := grpcPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	modelID, err := parseUUID(request.GetId())
	if err != nil {
		return nil, err
	}
	model, err := fromProtoModel(request.GetProviderId(), modelID.String(), request.GetRemoteModelName(), request.GetKind(),
		request.GetContextWindowTokens(), request.GetMaxOutputTokens(), request.GetCapabilities(), request.GetInputPricePerMillion(),
		request.GetOutputPricePerMillion(), request.GetDataRegion(), request.GetVerificationSource(), request.GetVerifiedAt(), request.GetStatus())
	if err != nil {
		return nil, err
	}
	model, err = s.platform.Models.Update(ctx, principal, model)
	if err != nil {
		return nil, grpcError(err)
	}
	return &harnessv1.UpdateModelResponse{Model: toProtoModel(model)}, nil
}

// GetModel 处理 gRPC 模型读取。
func (s *platformGRPCServer) GetModel(ctx context.Context, request *harnessv1.GetModelRequest) (*harnessv1.GetModelResponse, error) {
	principal, err := grpcPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	modelID, err := parseUUID(request.GetId())
	if err != nil {
		return nil, err
	}
	model, err := s.platform.Models.Get(ctx, principal, modelID)
	if err != nil {
		return nil, grpcError(err)
	}
	return &harnessv1.GetModelResponse{Model: toProtoModel(model)}, nil
}

// DeleteModel 处理 gRPC 模型删除。
func (s *platformGRPCServer) DeleteModel(ctx context.Context, request *harnessv1.DeleteModelRequest) (*harnessv1.DeleteModelResponse, error) {
	principal, err := grpcPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	modelID, err := parseUUID(request.GetId())
	if err != nil {
		return nil, err
	}
	if err := s.platform.Models.Delete(ctx, principal, modelID); err != nil {
		return nil, grpcError(err)
	}
	return &harnessv1.DeleteModelResponse{}, nil
}

// ListModels 处理 gRPC 模型列表，支持按供应商过滤。
func (s *platformGRPCServer) ListModels(ctx context.Context, request *harnessv1.ListModelsRequest) (*harnessv1.ListModelsResponse, error) {
	principal, err := grpcPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	var providerID *uuid.UUID
	if request.ProviderId != nil {
		parsed, err := parseUUID(request.GetProviderId())
		if err != nil {
			return nil, err
		}
		providerID = &parsed
	}
	models, err := s.platform.Models.List(ctx, principal, providerID)
	if err != nil {
		return nil, grpcError(err)
	}
	response := &harnessv1.ListModelsResponse{Items: make([]*harnessv1.Model, 0, len(models))}
	for _, model := range models {
		response.Items = append(response.Items, toProtoModel(model))
	}
	return response, nil
}

// CreatePolicy 处理 gRPC 草稿策略创建。
func (s *platformGRPCServer) CreatePolicy(ctx context.Context, request *harnessv1.CreatePolicyRequest) (*harnessv1.CreatePolicyResponse, error) {
	principal, err := grpcPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	policy, err := s.platform.Policies.Create(ctx, principal, biz.ModelPolicy{
		Name: request.GetName(), MinContextTokens: request.GetMinContextTokens(),
		Rules: fromProtoStruct(request.GetRules()), Candidates: fromProtoCandidates(request.GetCandidates()),
	})
	if err != nil {
		return nil, grpcError(err)
	}
	return &harnessv1.CreatePolicyResponse{Policy: toProtoPolicy(policy)}, nil
}

// UpdatePolicy 处理 gRPC 草稿策略更新。
func (s *platformGRPCServer) UpdatePolicy(ctx context.Context, request *harnessv1.UpdatePolicyRequest) (*harnessv1.UpdatePolicyResponse, error) {
	principal, err := grpcPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	policyID, err := parseUUID(request.GetId())
	if err != nil {
		return nil, err
	}
	policy, err := s.platform.Policies.Update(ctx, principal, biz.ModelPolicy{
		ID: policyID, MinContextTokens: request.GetMinContextTokens(),
		Rules: fromProtoStruct(request.GetRules()), Candidates: fromProtoCandidates(request.GetCandidates()),
	})
	if err != nil {
		return nil, grpcError(err)
	}
	return &harnessv1.UpdatePolicyResponse{Policy: toProtoPolicy(policy)}, nil
}

// GetPolicy 处理 gRPC 策略读取。
func (s *platformGRPCServer) GetPolicy(ctx context.Context, request *harnessv1.GetPolicyRequest) (*harnessv1.GetPolicyResponse, error) {
	principal, err := grpcPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	policyID, err := parseUUID(request.GetId())
	if err != nil {
		return nil, err
	}
	policy, err := s.platform.Policies.Get(ctx, principal, policyID)
	if err != nil {
		return nil, grpcError(err)
	}
	return &harnessv1.GetPolicyResponse{Policy: toProtoPolicy(policy)}, nil
}

// DeletePolicy 处理 gRPC 策略删除。
func (s *platformGRPCServer) DeletePolicy(ctx context.Context, request *harnessv1.DeletePolicyRequest) (*harnessv1.DeletePolicyResponse, error) {
	principal, err := grpcPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	policyID, err := parseUUID(request.GetId())
	if err != nil {
		return nil, err
	}
	if err := s.platform.Policies.Delete(ctx, principal, policyID); err != nil {
		return nil, grpcError(err)
	}
	return &harnessv1.DeletePolicyResponse{}, nil
}

// PublishPolicy 处理 gRPC 策略发布。
func (s *platformGRPCServer) PublishPolicy(ctx context.Context, request *harnessv1.PublishPolicyRequest) (*harnessv1.PublishPolicyResponse, error) {
	principal, err := grpcPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	policyID, err := parseUUID(request.GetId())
	if err != nil {
		return nil, err
	}
	policy, err := s.platform.Policies.Publish(ctx, principal, policyID)
	if err != nil {
		return nil, grpcError(err)
	}
	return &harnessv1.PublishPolicyResponse{Policy: toProtoPolicy(policy)}, nil
}

// CreatePolicyVersion 处理 gRPC 策略新版本创建。
func (s *platformGRPCServer) CreatePolicyVersion(ctx context.Context, request *harnessv1.CreatePolicyVersionRequest) (*harnessv1.CreatePolicyVersionResponse, error) {
	principal, err := grpcPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	policyID, err := parseUUID(request.GetId())
	if err != nil {
		return nil, err
	}
	policy, err := s.platform.Policies.CreateVersion(ctx, principal, policyID)
	if err != nil {
		return nil, grpcError(err)
	}
	return &harnessv1.CreatePolicyVersionResponse{Policy: toProtoPolicy(policy)}, nil
}

// ListPolicies 处理 gRPC 策略列表。
func (s *platformGRPCServer) ListPolicies(ctx context.Context, _ *harnessv1.ListPoliciesRequest) (*harnessv1.ListPoliciesResponse, error) {
	principal, err := grpcPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	policies, err := s.platform.Policies.List(ctx, principal)
	if err != nil {
		return nil, grpcError(err)
	}
	response := &harnessv1.ListPoliciesResponse{Items: make([]*harnessv1.ModelPolicy, 0, len(policies))}
	for _, policy := range policies {
		response.Items = append(response.Items, toProtoPolicy(policy))
	}
	return response, nil
}

// CreateAgent 处理 gRPC Agent 草稿创建。
func (s *platformGRPCServer) CreateAgent(ctx context.Context, request *harnessv1.CreateAgentRequest) (*harnessv1.CreateAgentResponse, error) {
	principal, err := grpcPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	agent, err := s.platform.Agents.Create(ctx, principal, biz.Agent{
		Name: request.GetName(), Description: request.GetDescription(), Mode: biz.AgentMode(request.GetMode()),
		ModelPolicyID: optionalUUID(request.ModelPolicyId), ModelPolicyVersion: optionalInt(request.ModelPolicyVersion),
		SkillIDs: parseUUIDList(request.GetSkillIds()), MCPIDs: parseUUIDList(request.GetMcpIds()),
	})
	if err != nil {
		return nil, grpcError(err)
	}
	return &harnessv1.CreateAgentResponse{Agent: toProtoAgent(agent)}, nil
}

// UpdateAgent 处理 gRPC Agent 草稿更新。
func (s *platformGRPCServer) UpdateAgent(ctx context.Context, request *harnessv1.UpdateAgentRequest) (*harnessv1.UpdateAgentResponse, error) {
	principal, err := grpcPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	agentID, err := parseUUID(request.GetId())
	if err != nil {
		return nil, err
	}
	agent, err := s.platform.Agents.Update(ctx, principal, biz.Agent{
		ID: agentID, Description: request.GetDescription(), Mode: biz.AgentMode(request.GetMode()),
		ModelPolicyID: optionalUUID(request.ModelPolicyId), ModelPolicyVersion: optionalInt(request.ModelPolicyVersion),
		SkillIDs: parseUUIDList(request.GetSkillIds()), MCPIDs: parseUUIDList(request.GetMcpIds()),
	})
	if err != nil {
		return nil, grpcError(err)
	}
	return &harnessv1.UpdateAgentResponse{Agent: toProtoAgent(agent)}, nil
}

// GetAgent 处理 gRPC Agent 读取。
func (s *platformGRPCServer) GetAgent(ctx context.Context, request *harnessv1.GetAgentRequest) (*harnessv1.GetAgentResponse, error) {
	principal, err := grpcPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	agentID, err := parseUUID(request.GetId())
	if err != nil {
		return nil, err
	}
	agent, err := s.platform.Agents.Get(ctx, principal, agentID)
	if err != nil {
		return nil, grpcError(err)
	}
	return &harnessv1.GetAgentResponse{Agent: toProtoAgent(agent)}, nil
}

// DeleteAgent 处理 gRPC 草稿删除。
func (s *platformGRPCServer) DeleteAgent(ctx context.Context, request *harnessv1.DeleteAgentRequest) (*harnessv1.DeleteAgentResponse, error) {
	principal, err := grpcPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	agentID, err := parseUUID(request.GetId())
	if err != nil {
		return nil, err
	}
	if err := s.platform.Agents.Delete(ctx, principal, agentID); err != nil {
		return nil, grpcError(err)
	}
	return &harnessv1.DeleteAgentResponse{}, nil
}

// PublishAgent 处理 gRPC Agent 发布。
func (s *platformGRPCServer) PublishAgent(ctx context.Context, request *harnessv1.PublishAgentRequest) (*harnessv1.PublishAgentResponse, error) {
	principal, err := grpcPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	agentID, err := parseUUID(request.GetId())
	if err != nil {
		return nil, err
	}
	agent, err := s.platform.Agents.Publish(ctx, principal, agentID)
	if err != nil {
		return nil, grpcError(err)
	}
	return &harnessv1.PublishAgentResponse{Agent: toProtoAgent(agent)}, nil
}

// CreateAgentVersion 处理 gRPC Agent 新版本创建。
func (s *platformGRPCServer) CreateAgentVersion(ctx context.Context, request *harnessv1.CreateAgentVersionRequest) (*harnessv1.CreateAgentVersionResponse, error) {
	principal, err := grpcPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	agentID, err := parseUUID(request.GetId())
	if err != nil {
		return nil, err
	}
	agent, err := s.platform.Agents.CreateVersion(ctx, principal, agentID)
	if err != nil {
		return nil, grpcError(err)
	}
	return &harnessv1.CreateAgentVersionResponse{Agent: toProtoAgent(agent)}, nil
}

// ListAgents 处理 gRPC Agent 游标分页列表。
func (s *platformGRPCServer) ListAgents(ctx context.Context, request *harnessv1.ListAgentsRequest) (*harnessv1.ListAgentsResponse, error) {
	principal, err := grpcPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	limit := int(request.GetLimit())
	if limit < 1 || limit > 100 {
		return nil, status.Error(codes.InvalidArgument, "invalid_page_limit")
	}
	page, err := s.platform.Agents.List(ctx, principal, limit, request.GetCursor())
	if err != nil {
		return nil, grpcError(err)
	}
	response := &harnessv1.ListAgentsResponse{Items: make([]*harnessv1.Agent, 0, len(page.Items)), NextCursor: page.NextCursor}
	for _, agent := range page.Items {
		response.Items = append(response.Items, toProtoAgent(agent))
	}
	return response, nil
}

// CreateSession 处理 gRPC 会话创建。
func (s *platformGRPCServer) CreateSession(ctx context.Context, request *harnessv1.CreateSessionRequest) (*harnessv1.CreateSessionResponse, error) {
	principal, err := grpcPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	session, err := s.platform.Sessions.Create(ctx, principal, biz.Session{
		AgentID: optionalUUID(request.AgentId), Title: request.GetTitle(), Source: biz.SessionSource(request.GetSource()),
	})
	if err != nil {
		return nil, grpcError(err)
	}
	return &harnessv1.CreateSessionResponse{Session: toProtoSession(session)}, nil
}

// GetSession 处理 gRPC 会话读取。
func (s *platformGRPCServer) GetSession(ctx context.Context, request *harnessv1.GetSessionRequest) (*harnessv1.GetSessionResponse, error) {
	principal, err := grpcPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	sessionID, err := parseUUID(request.GetId())
	if err != nil {
		return nil, err
	}
	session, err := s.platform.Sessions.Get(ctx, principal, sessionID)
	if err != nil {
		return nil, grpcError(err)
	}
	return &harnessv1.GetSessionResponse{Session: toProtoSession(session)}, nil
}

// ArchiveSession 处理 gRPC 会话归档。
func (s *platformGRPCServer) ArchiveSession(ctx context.Context, request *harnessv1.ArchiveSessionRequest) (*harnessv1.ArchiveSessionResponse, error) {
	principal, err := grpcPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	sessionID, err := parseUUID(request.GetId())
	if err != nil {
		return nil, err
	}
	session, err := s.platform.Sessions.Archive(ctx, principal, sessionID)
	if err != nil {
		return nil, grpcError(err)
	}
	return &harnessv1.ArchiveSessionResponse{Session: toProtoSession(session)}, nil
}

// UpdateSessionMemoryPolicy 处理 gRPC 会话记忆策略更新。
func (s *platformGRPCServer) UpdateSessionMemoryPolicy(ctx context.Context, request *harnessv1.UpdateSessionMemoryPolicyRequest) (*harnessv1.UpdateSessionMemoryPolicyResponse, error) {
	principal, err := grpcPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	sessionID, err := parseUUID(request.GetId())
	if err != nil {
		return nil, err
	}
	session, err := s.platform.Sessions.SetMemoryPolicy(ctx, principal, sessionID,
		biz.SessionMemoryPolicy(request.GetMemoryPolicy()))
	if err != nil {
		return nil, grpcError(err)
	}
	return &harnessv1.UpdateSessionMemoryPolicyResponse{Session: toProtoSession(session)}, nil
}

// ListSessions 处理 gRPC 会话游标分页列表。
func (s *platformGRPCServer) ListSessions(ctx context.Context, request *harnessv1.ListSessionsRequest) (*harnessv1.ListSessionsResponse, error) {
	principal, err := grpcPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	limit := int(request.GetLimit())
	if limit < 1 || limit > 100 {
		return nil, status.Error(codes.InvalidArgument, "invalid_page_limit")
	}
	page, err := s.platform.Sessions.List(ctx, principal, limit, request.GetCursor())
	if err != nil {
		return nil, grpcError(err)
	}
	response := &harnessv1.ListSessionsResponse{Items: make([]*harnessv1.Session, 0, len(page.Items)), NextCursor: page.NextCursor}
	for _, session := range page.Items {
		response.Items = append(response.Items, toProtoSession(session))
	}
	return response, nil
}

// AppendMessage 处理 gRPC 用户消息追加。
func (s *platformGRPCServer) AppendMessage(ctx context.Context, request *harnessv1.AppendMessageRequest) (*harnessv1.AppendMessageResponse, error) {
	principal, err := grpcPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	sessionID, err := parseUUID(request.GetSessionId())
	if err != nil {
		return nil, err
	}
	message, err := s.platform.Sessions.AppendMessage(ctx, principal, biz.Message{SessionID: sessionID, Content: request.GetContent()})
	if err != nil {
		return nil, grpcError(err)
	}
	return &harnessv1.AppendMessageResponse{Message: toProtoMessage(message)}, nil
}

// ListMessages 处理 gRPC 会话消息游标分页列表。
func (s *platformGRPCServer) ListMessages(ctx context.Context, request *harnessv1.ListMessagesRequest) (*harnessv1.ListMessagesResponse, error) {
	principal, err := grpcPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	sessionID, err := parseUUID(request.GetSessionId())
	if err != nil {
		return nil, err
	}
	limit := int(request.GetLimit())
	if limit < 1 || limit > 100 {
		return nil, status.Error(codes.InvalidArgument, "invalid_page_limit")
	}
	page, err := s.platform.Sessions.ListMessages(ctx, principal, sessionID, limit, request.GetCursor())
	if err != nil {
		return nil, grpcError(err)
	}
	response := &harnessv1.ListMessagesResponse{Items: make([]*harnessv1.ChatMessage, 0, len(page.Items)), NextCursor: page.NextCursor}
	for _, message := range page.Items {
		response.Items = append(response.Items, toProtoMessage(message))
	}
	return response, nil
}

// parseUUID 解析字符串 ID，非法时返回 InvalidArgument。
func parseUUID(value string) (uuid.UUID, error) {
	id, err := uuid.Parse(value)
	if err != nil || id == uuid.Nil {
		return uuid.Nil, status.Error(codes.InvalidArgument, "invalid_resource_id")
	}
	return id, nil
}

// optionalUUID 解析可选字符串 ID，空值返回 nil。
func optionalUUID(value *string) *uuid.UUID {
	if value == nil || *value == "" {
		return nil
	}
	id, err := uuid.Parse(*value)
	if err != nil {
		return nil
	}
	return &id
}

// optionalInt 转换可选整型指针。
func optionalInt(value *int32) *int {
	if value == nil {
		return nil
	}
	converted := int(*value)
	return &converted
}

// parseUUIDList 解析 ID 列表，忽略非法项由领域层统一报错。
func parseUUIDList(values []string) []uuid.UUID {
	result := make([]uuid.UUID, 0, len(values))
	for _, value := range values {
		if id, err := uuid.Parse(value); err == nil {
			result = append(result, id)
		}
	}
	return result
}

// fromProtoStruct 将 Protobuf Struct 转为领域 map；空值返回 nil。
func fromProtoStruct(value *structpb.Struct) map[string]any {
	if value == nil {
		return nil
	}
	return value.AsMap()
}

// toProtoStruct 将领域 map 转为 Protobuf Struct；nil 返回空对象。
func toProtoStruct(value map[string]any) *structpb.Struct {
	result, err := structpb.NewStruct(value)
	if err != nil {
		return &structpb.Struct{Fields: map[string]*structpb.Value{}}
	}
	return result
}

// fromProtoCandidates 转换策略候选列表。
func fromProtoCandidates(values []*harnessv1.PolicyCandidate) []biz.PolicyCandidate {
	result := make([]biz.PolicyCandidate, 0, len(values))
	for _, value := range values {
		candidate := biz.PolicyCandidate{Priority: int(value.GetPriority()), Constraints: fromProtoStruct(value.GetConstraints())}
		if id, err := uuid.Parse(value.GetModelId()); err == nil {
			candidate.ModelID = id
		}
		result = append(result, candidate)
	}
	return result
}

// fromProtoModel 转换模型入参，非法 UUID 返回 InvalidArgument。
func fromProtoModel(providerID, modelID, remoteName, kind string, contextTokens int64, maxOutput int32, capabilities *harnessv1.ModelCapabilities,
	inputPrice, outputPrice float64, region, verificationSource string, verifiedAt *timestamppb.Timestamp, modelStatus string) (biz.Model, error) {
	providerUUID, err := uuid.Parse(providerID)
	if err != nil {
		return biz.Model{}, status.Error(codes.InvalidArgument, "invalid_provider_id")
	}
	model := biz.Model{
		ProviderID: providerUUID, RemoteModelName: remoteName, Kind: biz.ModelKind(kind),
		ContextWindowTokens: contextTokens, MaxOutputTokens: int(maxOutput), InputPricePerMillion: inputPrice,
		OutputPricePerMillion: outputPrice, DataRegion: region, VerificationSource: verificationSource, Status: biz.ModelStatus(modelStatus),
	}
	if modelID != "" {
		parsed, err := uuid.Parse(modelID)
		if err != nil {
			return biz.Model{}, status.Error(codes.InvalidArgument, "invalid_resource_id")
		}
		model.ID = parsed
	}
	if capabilities != nil {
		model.Capabilities = biz.ModelCapabilities{
			Streaming: capabilities.GetStreaming(), Tools: capabilities.GetTools(), StructuredOutput: capabilities.GetStructuredOutput(),
		}
	}
	if verifiedAt != nil {
		timestamp := verifiedAt.AsTime().UTC()
		model.VerifiedAt = &timestamp
	}
	return model, nil
}

// toProtoProvider 转换供应商出参，不包含任何密钥材料。
func toProtoProvider(provider biz.Provider) *harnessv1.Provider {
	return &harnessv1.Provider{
		Id: provider.ID.String(), Name: provider.Name, ProviderType: provider.ProviderType, BaseUrl: provider.BaseURL,
		KeyVersion: provider.KeyVersion, Status: string(provider.Status), DataRegion: provider.DataRegion,
		CreatedAt: timestamp(provider.CreatedAt), UpdatedAt: timestamp(provider.UpdatedAt),
	}
}

// toProtoModel 转换模型出参。
func toProtoModel(model biz.Model) *harnessv1.Model {
	result := &harnessv1.Model{
		Id: model.ID.String(), ProviderId: model.ProviderID.String(), RemoteModelName: model.RemoteModelName,
		Kind: string(model.Kind), ContextWindowTokens: model.ContextWindowTokens, MaxOutputTokens: clampInt32(model.MaxOutputTokens),
		Capabilities: &harnessv1.ModelCapabilities{
			Streaming: model.Capabilities.Streaming, Tools: model.Capabilities.Tools, StructuredOutput: model.Capabilities.StructuredOutput,
		},
		InputPricePerMillion: model.InputPricePerMillion, OutputPricePerMillion: model.OutputPricePerMillion,
		DataRegion: model.DataRegion, VerificationSource: model.VerificationSource, Status: string(model.Status),
		CreatedAt: timestamp(model.CreatedAt), UpdatedAt: timestamp(model.UpdatedAt),
	}
	if model.VerifiedAt != nil {
		result.VerifiedAt = timestamp(*model.VerifiedAt)
	}
	return result
}

// toProtoPolicy 转换策略出参。
func toProtoPolicy(policy biz.ModelPolicy) *harnessv1.ModelPolicy {
	result := &harnessv1.ModelPolicy{
		Id: policy.ID.String(), Name: policy.Name, Version: clampInt32(policy.Version),
		MinContextTokens: policy.MinContextTokens, Rules: toProtoStruct(policy.Rules), Status: string(policy.Status),
		Candidates: make([]*harnessv1.PolicyCandidate, 0, len(policy.Candidates)),
		CreatedAt:  timestamp(policy.CreatedAt), UpdatedAt: timestamp(policy.UpdatedAt),
	}
	for _, candidate := range policy.Candidates {
		result.Candidates = append(result.Candidates, &harnessv1.PolicyCandidate{
			ModelId: candidate.ModelID.String(), Priority: clampInt32(candidate.Priority), Constraints: toProtoStruct(candidate.Constraints),
		})
	}
	return result
}

// toProtoAgent 转换 Agent 出参，包含发布快照。
func toProtoAgent(agent biz.Agent) *harnessv1.Agent {
	result := &harnessv1.Agent{
		Id: agent.ID.String(), UserId: agent.UserID.String(), Name: agent.Name, Description: agent.Description,
		Mode: string(agent.Mode), Version: clampInt32(agent.Version), ConfigSnapshot: toProtoStruct(agent.ConfigSnapshot),
		SkillIds: uuidStrings(agent.SkillIDs), McpIds: uuidStrings(agent.MCPIDs), Status: string(agent.Status),
		CreatedAt: timestamp(agent.CreatedAt), UpdatedAt: timestamp(agent.UpdatedAt),
	}
	if agent.ModelPolicyID != nil {
		value := agent.ModelPolicyID.String()
		result.ModelPolicyId = &value
	}
	if agent.ModelPolicyVersion != nil {
		value := clampInt32(*agent.ModelPolicyVersion)
		result.ModelPolicyVersion = &value
	}
	return result
}

// toProtoSession 转换会话出参。
func toProtoSession(session biz.Session) *harnessv1.Session {
	result := &harnessv1.Session{
		Id: session.ID.String(), Title: session.Title, Source: string(session.Source), Status: string(session.Status),
		Version: session.Version, CreatedAt: timestamp(session.CreatedAt), UpdatedAt: timestamp(session.UpdatedAt),
		MemoryPolicy: string(session.MemoryPolicy),
	}
	if session.AgentID != nil {
		value := session.AgentID.String()
		result.AgentId = &value
	}
	if session.LastMessageAt != nil {
		result.LastMessageAt = timestamp(*session.LastMessageAt)
	}
	return result
}

// toProtoMessage 转换消息出参。
func toProtoMessage(message biz.Message) *harnessv1.ChatMessage {
	result := &harnessv1.ChatMessage{
		Id: message.ID.String(), SessionId: message.SessionID.String(), Role: string(message.Role), Content: message.Content,
		TokenCount: clampInt32(message.TokenCount), CreatedAt: timestamp(message.CreatedAt),
	}
	result.ToolCalls = make([]*structpb.Struct, 0, len(message.ToolCalls))
	for _, toolCall := range message.ToolCalls {
		result.ToolCalls = append(result.ToolCalls, toProtoStruct(toolCall))
	}
	return result
}

// clampInt32 把内部 int 字段安全转换为 Protobuf int32，越界时饱和而不是回绕。
func clampInt32(value int) int32 {
	if value > math.MaxInt32 {
		return math.MaxInt32
	}
	if value < math.MinInt32 {
		return math.MinInt32
	}
	return int32(value)
}

// uuidStrings 将 UUID 列表转换为字符串列表。
func uuidStrings(values []uuid.UUID) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, value.String())
	}
	return result
}

// timestamp 将时间转为 Protobuf Timestamp，零值返回 nil。
func timestamp(value time.Time) *timestamppb.Timestamp {
	if value.IsZero() {
		return nil
	}
	return timestamppb.New(value.UTC())
}
