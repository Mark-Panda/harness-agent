package biz

import (
	"context"
	"errors"
	"fmt"
	"time"

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

type feishuGroupReplyCompleter interface {
	CompleteGroupReply(ctx context.Context, outbound FeishuOutbound, workerID, text string, now time.Time) error
}

type feishuGroupThreadPromoter interface {
	PromoteGroupConversationThread(ctx context.Context, outbound FeishuOutbound, threadID string) (int, error)
}

func isFeishuSharedGroup(run Run) bool {
	shared, _ := run.ConfigSnapshot["feishu_shared_group"].(bool)
	return shared
}

func (w *FeishuEventWorker) processSharedChat(ctx context.Context, record FeishuEventRecord, event feishu.MessageEvent,
	speakerID, routeOwnerID, policyID uuid.UUID, text, threadID, rootMessageID string, defaultBot bool,
) (string, error) {
	// 默认应用的旧绑定没有 App ID。尊重其停用状态，但共享路径始终使用带应用作用域的绑定。
	if defaultBot {
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
	return w.createReply(ctx, record.AppID, event, threadID, speakerID, projected.SessionID, run.ID)
}
