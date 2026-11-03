package data

import (
	"context"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
)

// RunSnapshotRepository 组合用户私有 Agent 仓库与平台模型目录，为 Run 创建提供只读快照来源。
type RunSnapshotRepository struct {
	// agents 用户私有 Agent 与策略读取仓库。
	agents *AgentRepository
	// catalog 平台级供应商目录。
	catalog *CatalogRepository
}

// NewRunSnapshotRepository 构造 Run 快照仓库；两个依赖都必须非空。
func NewRunSnapshotRepository(agents *AgentRepository, catalog *CatalogRepository) *RunSnapshotRepository {
	return &RunSnapshotRepository{agents: agents, catalog: catalog}
}

// GetAgent 读取当前用户的 Agent 版本。
func (r *RunSnapshotRepository) GetAgent(ctx context.Context, userID, agentID uuid.UUID) (biz.Agent, error) {
	return r.agents.GetAgent(ctx, userID, agentID)
}

// GetPolicy 读取 Agent 引用的模型策略。
func (r *RunSnapshotRepository) GetPolicy(ctx context.Context, policyID uuid.UUID) (biz.ModelPolicy, error) {
	return r.agents.GetPolicy(ctx, policyID)
}

// ListModelsByIDs 批量读取策略候选模型。
func (r *RunSnapshotRepository) ListModelsByIDs(ctx context.Context, modelIDs []uuid.UUID) ([]biz.Model, error) {
	return r.agents.ListModelsByIDs(ctx, modelIDs)
}

// GetProvider 读取候选模型所属供应商的非密元数据。
func (r *RunSnapshotRepository) GetProvider(ctx context.Context, providerID uuid.UUID) (biz.Provider, error) {
	return r.catalog.GetProvider(ctx, providerID)
}

// 编译期断言快照来源实现。
var _ biz.RunSnapshotRepository = (*RunSnapshotRepository)(nil)
