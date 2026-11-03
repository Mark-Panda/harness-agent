package biz

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ErrBudgetExceeded 表示用户、Agent 或 Provider 的每日预算已耗尽，新的模型调用必须被拒绝。
var ErrBudgetExceeded = errors.New("budget exceeded")

// BudgetScope 是预算聚合维度。
type BudgetScope string

const (
	// BudgetScopeUser 用户级预算。
	BudgetScopeUser BudgetScope = "user"
	// BudgetScopeAgent Agent 级预算。
	BudgetScopeAgent BudgetScope = "agent"
	// BudgetScopeProvider 供应商级预算。
	BudgetScopeProvider BudgetScope = "provider"
)

// BudgetUsage 是一个统计窗口内已消耗的 Token 与费用。
type BudgetUsage struct {
	// InputTokens 累计输入 Token。
	InputTokens int64
	// OutputTokens 累计输出 Token。
	OutputTokens int64
	// EstimatedCost 累计估算费用。
	EstimatedCost float64
}

// TotalTokens 返回输入与输出 Token 之和。
func (u BudgetUsage) TotalTokens() int64 { return u.InputTokens + u.OutputTokens }

// BudgetPolicy 是每日预算上限；零值表示该维度不限制。
type BudgetPolicy struct {
	// UserDailyTokens 单用户每日 Token 上限。
	UserDailyTokens int64
	// UserDailyCost 单用户每日费用上限。
	UserDailyCost float64
	// AgentDailyTokens 单 Agent 每日 Token 上限。
	AgentDailyTokens int64
	// AgentDailyCost 单 Agent 每日费用上限。
	AgentDailyCost float64
	// ProviderDailyTokens 单供应商每日 Token 上限。
	ProviderDailyTokens int64
	// ProviderDailyCost 单供应商每日费用上限。
	ProviderDailyCost float64
	// AlertThreshold 告警阈值占上限比例，默认 0.8。
	AlertThreshold float64
}

// Normalize 补齐告警阈值，避免零值导致阈值告警失效。
func (p BudgetPolicy) Normalize() BudgetPolicy {
	if p.AlertThreshold <= 0 || p.AlertThreshold > 1 {
		p.AlertThreshold = 0.8
	}
	if p.UserDailyTokens < 0 {
		p.UserDailyTokens = 0
	}
	if p.AgentDailyTokens < 0 {
		p.AgentDailyTokens = 0
	}
	if p.ProviderDailyTokens < 0 {
		p.ProviderDailyTokens = 0
	}
	if p.UserDailyCost < 0 {
		p.UserDailyCost = 0
	}
	if p.AgentDailyCost < 0 {
		p.AgentDailyCost = 0
	}
	if p.ProviderDailyCost < 0 {
		p.ProviderDailyCost = 0
	}
	return p
}

// BudgetAlert 描述一个接近或超过预算的维度，供事件、日志与告警消费。
type BudgetAlert struct {
	// Scope 预算维度。
	Scope BudgetScope
	// ScopeID 维度实体 ID。
	ScopeID uuid.UUID
	// Usage 当前窗口已用量。
	Usage BudgetUsage
	// LimitTokens Token 上限，零表示不限制。
	LimitTokens int64
	// LimitCost 费用上限，零表示不限制。
	LimitCost float64
	// Exceeded 是否已超过上限。
	Exceeded bool
}

// BudgetRepository 读取每日预算聚合；计数由模型调用结算时原子累加。
type BudgetRepository interface {
	// GetUsage 读取指定维度在窗口内的累计用量。
	GetUsage(ctx context.Context, scope BudgetScope, scopeID uuid.UUID, windowStart time.Time) (BudgetUsage, error)
}

// BudgetChecker 是预算校验端口，供 Run 创建与模型 Attempt 调用前复用。
type BudgetChecker interface {
	// Check 校验用户、Agent 与供应商预算；返回接近阈值的告警，超限时返回 ErrBudgetExceeded。
	Check(ctx context.Context, userID uuid.UUID, agentID, providerID *uuid.UUID) ([]BudgetAlert, error)
}

// BudgetService 实现每日预算校验，读取失败由调用方决定降级策略。
type BudgetService struct {
	repo   BudgetRepository
	policy BudgetPolicy
	clock  func() time.Time
}

// NewBudgetService 构造预算服务；计数仓储必填。
func NewBudgetService(repo BudgetRepository, policy BudgetPolicy, clock func() time.Time) (*BudgetService, error) {
	if repo == nil {
		return nil, invalid("budget repository is required")
	}
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &BudgetService{repo: repo, policy: policy.Normalize(), clock: clock}, nil
}

// WindowStart 返回给定时间的 UTC 自然日零点，作为预算窗口起点。
func (s *BudgetService) WindowStart(now time.Time) time.Time {
	utc := now.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
}

// Check 校验用户、Agent 与供应商预算。
// 任一维度超限时返回 ErrBudgetExceeded；接近阈值但未超限时返回告警，不阻断调用。
func (s *BudgetService) Check(ctx context.Context, userID uuid.UUID, agentID, providerID *uuid.UUID) ([]BudgetAlert, error) {
	if userID == uuid.Nil {
		return nil, ErrForbidden
	}
	// 所有上限均为零时视为未启用预算，避免每次模型调用产生无意义的聚合查询。
	if s.policy.UserDailyTokens == 0 && s.policy.UserDailyCost == 0 &&
		s.policy.AgentDailyTokens == 0 && s.policy.AgentDailyCost == 0 &&
		s.policy.ProviderDailyTokens == 0 && s.policy.ProviderDailyCost == 0 {
		return nil, nil
	}
	window := s.WindowStart(s.clock())
	alerts := make([]BudgetAlert, 0, 3)
	exceeded := false

	usage, err := s.repo.GetUsage(ctx, BudgetScopeUser, userID, window)
	if err != nil {
		return nil, err
	}
	if alert, over := s.evaluate(BudgetScopeUser, userID, usage, s.policy.UserDailyTokens, s.policy.UserDailyCost); alert != nil {
		alerts = append(alerts, *alert)
		if over {
			exceeded = true
		}
	}
	if agentID != nil && *agentID != uuid.Nil {
		usage, err := s.repo.GetUsage(ctx, BudgetScopeAgent, *agentID, window)
		if err != nil {
			return nil, err
		}
		if alert, over := s.evaluate(BudgetScopeAgent, *agentID, usage, s.policy.AgentDailyTokens, s.policy.AgentDailyCost); alert != nil {
			alerts = append(alerts, *alert)
			if over {
				exceeded = true
			}
		}
	}
	if providerID != nil && *providerID != uuid.Nil {
		usage, err := s.repo.GetUsage(ctx, BudgetScopeProvider, *providerID, window)
		if err != nil {
			return nil, err
		}
		if alert, over := s.evaluate(BudgetScopeProvider, *providerID, usage, s.policy.ProviderDailyTokens, s.policy.ProviderDailyCost); alert != nil {
			alerts = append(alerts, *alert)
			if over {
				exceeded = true
			}
		}
	}
	if exceeded {
		return alerts, fmt.Errorf("%w: daily budget exhausted", ErrBudgetExceeded)
	}
	return alerts, nil
}

// evaluate 评估单一维度；返回需要告警时非空，以及是否超过硬上限。
func (s *BudgetService) evaluate(scope BudgetScope, scopeID uuid.UUID, usage BudgetUsage, tokenLimit int64, costLimit float64) (*BudgetAlert, bool) {
	if tokenLimit <= 0 && costLimit <= 0 {
		return nil, false
	}
	tokenRatio := 0.0
	if tokenLimit > 0 {
		tokenRatio = float64(usage.TotalTokens()) / float64(tokenLimit)
	}
	costRatio := 0.0
	if costLimit > 0 {
		costRatio = usage.EstimatedCost / costLimit
	}
	ratio := tokenRatio
	if costRatio > ratio {
		ratio = costRatio
	}
	exceeded := (tokenLimit > 0 && usage.TotalTokens() >= tokenLimit) || (costLimit > 0 && usage.EstimatedCost >= costLimit)
	if !exceeded && ratio < s.policy.AlertThreshold {
		return nil, false
	}
	return &BudgetAlert{
		Scope: scope, ScopeID: scopeID, Usage: usage,
		LimitTokens: tokenLimit, LimitCost: costLimit, Exceeded: exceeded,
	}, exceeded
}
