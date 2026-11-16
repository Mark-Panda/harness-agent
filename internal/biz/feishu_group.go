package biz

import (
	"context"
	"errors"
	"fmt"

	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
	"github.com/Mark-Panda/harness-agent/pkg/feishu"
	"github.com/Mark-Panda/harness-agent/pkg/logging"
	"github.com/google/uuid"
)

// FeishuGroupMessageStore 只接收已授权的飞书群事件，并为当前发言人创建独立的上下文投影。
type FeishuGroupMessageStore interface {
	ProjectGroupMessage(ctx context.Context, binding FeishuConversationBinding, inbound Message) (Message, error)
}

type feishuGroupRunHistoryReader interface {
	ListFeishuGroupRunHistory(ctx context.Context, userID, sessionID, runID uuid.UUID) ([]Message, error)
}

func isFeishuSharedGroup(run Run) bool {
	shared, _ := run.ConfigSnapshot["feishu_shared_group"].(bool)
	return shared
}

// lowRiskOnlyRegistry 只暴露低风险工具，用于共享群聊等无法确定审批责任人的场景。
type lowRiskOnlyRegistry struct {
	inner react.Registry
}

func (r lowRiskOnlyRegistry) Lookup(name string) (react.Tool, bool) {
	tool, ok := r.inner.Lookup(name)
	if !ok || tool.RiskLevel() != "low" {
		return nil, false
	}
	return tool, true
}

func (r lowRiskOnlyRegistry) List() []react.ToolSpec {
	specs := make([]react.ToolSpec, 0)
	for _, spec := range r.inner.List() {
		if _, ok := r.Lookup(spec.Name); !ok {
			continue
		}
		specs = append(specs, spec)
	}
	return specs
}

// restrictSharedGroupTools 把共享群聊的工具集合收窄到低风险工具。
// 群内任一成员都能触发调用，高风险工具等价于把凭据与副作用共享给整个群；
// 共享群也没有明确的审批责任人，因此在工具发现阶段直接剔除，而不是依赖审批兜底。
func restrictSharedGroupTools(tools react.Registry) react.Registry {
	if tools == nil {
		return nil
	}
	filtered := lowRiskOnlyRegistry{inner: tools}
	if len(filtered.List()) == 0 {
		return nil
	}
	return filtered
}

func (w *FeishuEventWorker) processSharedChat(ctx context.Context, record FeishuEventRecord, event feishu.MessageEvent,
	speakerID, routeOwnerID, policyID uuid.UUID, text, threadID, rootMessageID string, botID uuid.UUID,
) (string, error) {
	// 默认应用的旧绑定没有 App ID。尊重其停用状态，但共享路径始终使用带应用作用域的绑定。
	if botID == uuid.Nil {
		legacy, found, err := w.findConversation(ctx, "", record.TenantKey, event.ChatID, threadID, rootMessageID)
		if err != nil {
			return "", err
		}
		if found && (legacy.UserID != routeOwnerID || legacy.Status != FeishuBindingActive) {
			return FeishuEventIgnored, nil
		}
		if found {
			session, err := w.sessions.GetSession(ctx, legacy.UserID, legacy.SessionID)
			if err != nil {
				return "", err
			}
			if session.Status != SessionActive {
				return FeishuEventIgnored, nil
			}
		}
	}
	binding, found, err := w.findConversation(ctx, record.AppID, record.TenantKey, event.ChatID, threadID, rootMessageID)
	if err != nil {
		return "", err
	}
	if !found {
		binding, err = w.createConversation(ctx, routeOwnerID, record.AppID, record.TenantKey, nil, event, threadID, rootMessageID)
		if err != nil {
			return "", err
		}
	}
	if binding.UserID != routeOwnerID || binding.Status != FeishuBindingActive {
		return FeishuEventIgnored, nil
	}
	projected, err := w.groupMessages.ProjectGroupMessage(ctx, binding, Message{
		UserID: speakerID, Role: MessageUser, Content: text,
		ExternalMessageID: event.MessageID, SpeakerOpenID: event.SenderOpenID, CreatedAt: event.CreatedAt(w.clock()),
	})
	if errors.Is(err, ErrForbidden) || errors.Is(err, ErrResourceConflict) {
		return FeishuEventIgnored, nil
	}
	if err != nil {
		return "", err
	}
	principal, err := NewPrincipal(speakerID, RoleUser)
	if err != nil {
		return "", err
	}
	run, err := w.groupRuns.CreateFeishuGroup(ctx, principal, policyID, CreateRunInput{
		SessionID: projected.SessionID, Mode: AgentModeReact,
		IdempotencyKey:   "feishu.group:" + projected.ID.String(),
		TriggerMessageID: &projected.ID,
	})
	if errors.Is(err, ErrBudgetExceeded) {
		return FeishuEventIgnored, nil
	}
	if err != nil {
		return "", err
	}
	selectedPolicy, _ := run.ModelPolicySnapshot["policy_id"].(string)
	if run.ID == uuid.Nil || run.UserID != speakerID || run.SessionID != projected.SessionID ||
		run.Mode != AgentModeReact || run.AgentID != nil || run.AgentVersion != nil ||
		run.TriggerMessageID == nil || *run.TriggerMessageID != projected.ID ||
		!isFeishuSharedGroup(run) || selectedPolicy != policyID.String() {
		return "", fmt.Errorf("%w: shared feishu run does not match its authorized projection", ErrResourceConflict)
	}
	ctx = logging.WithRunID(ctx, run.ID.String())
	return w.createReply(ctx, record.AppID, botID, event, threadID, speakerID, projected.SessionID, run.ID)
}
