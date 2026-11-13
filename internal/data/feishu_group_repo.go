package data

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const feishuGroupHistoryLimit = 12

type feishuGroupMessageModel struct {
	ID                  uuid.UUID
	BindingID           uuid.UUID
	SenderUserID        uuid.UUID
	ExternalMessageID   string
	SpeakerOpenID       string
	Content             string
	OccurredAt          time.Time
	ProjectionSessionID *uuid.UUID
	ReplyText           string
	RepliedAt           *time.Time
}

func (feishuGroupMessageModel) TableName() string { return "feishu_group_messages" }

// ProjectGroupMessage 记录签名事件，并把有限的话题历史复制到发言人拥有的单次 Run 会话。
// 同一话题的投影按绑定锁串行提交。重投事件返回既有触发消息和上下文边界。
func (r *FeishuMessageRepository) ProjectGroupMessage(ctx context.Context, binding biz.FeishuConversationBinding, inbound biz.Message) (biz.Message, error) {
	if binding.ID == uuid.Nil || binding.UserID == uuid.Nil || binding.AppID == "" || binding.TenantKey == "" ||
		binding.ChatID == "" || binding.ThreadID == "" || binding.ThreadID == "p2p" || inbound.UserID == uuid.Nil ||
		inbound.ExternalMessageID == "" || inbound.SpeakerOpenID == "" || inbound.CreatedAt.IsZero() {
		return biz.Message{}, fmt.Errorf("%w: group projection requires an app-scoped thread and a speaker", biz.ErrInvalidConfig)
	}
	var projected biz.Message
	err := WithScope(ctx, r.db, Scope{System: true, UserID: inbound.UserID}, func(tx *gorm.DB) error {
		// 与账号清理使用相同锁顺序，避免已解析但尚未处理的事件重建已删除的投影。
		var speaker userModel
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("id").First(&speaker, "id = ? AND status = ?", inbound.UserID, biz.UserStatusActive).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return biz.ErrForbidden
		}
		if err != nil {
			return err
		}
		var active externalBindingModel
		err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND user_id = ? AND session_id = ? AND provider = 'feishu' AND app_id = ? AND tenant_key = ? AND chat_id = ? AND status = ?",
				binding.ID, binding.UserID, binding.SessionID, binding.AppID, binding.TenantKey, binding.ChatID, biz.FeishuBindingActive).
			First(&active).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return biz.ErrForbidden
		}
		if err != nil {
			return err
		}
		var session sessionModel
		if err := tx.First(&session, "id = ? AND user_id = ?", active.SessionID, active.UserID).Error; err != nil {
			return err
		}
		if biz.SessionStatus(session.Status) != biz.SessionActive || biz.SessionSource(session.Source) != biz.SessionSourceFeishu ||
			biz.SessionMemoryPolicy(session.MemoryPolicy) == biz.SessionMemoryPrivate {
			return biz.ErrForbidden
		}

		var current feishuGroupMessageModel
		err = tx.First(&current, "binding_id = ? AND external_message_id = ?", active.ID, inbound.ExternalMessageID).Error
		switch {
		case err == nil:
			if current.SenderUserID != inbound.UserID || current.SpeakerOpenID != inbound.SpeakerOpenID || current.ProjectionSessionID == nil {
				return biz.ErrForbidden
			}
			var trigger messageModel
			if err := tx.First(&trigger, "id = ? AND session_id = ? AND user_id = ? AND deleted_at IS NULL",
				current.ID, *current.ProjectionSessionID, inbound.UserID).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return biz.ErrForbidden
				}
				return err
			}
			projected, err = toBizMessage(trigger)
			return err
		case !errors.Is(err, gorm.ErrRecordNotFound):
			return err
		}

		sessionID := uuid.New()
		projection := sessionModel{ID: sessionID, UserID: inbound.UserID, Title: session.Title,
			Source: string(biz.SessionSourceFeishu), MemoryPolicy: string(biz.SessionMemoryGroup),
			FeishuGroupProjection: true,
			Status:                string(biz.SessionActive), Version: 1, LastMessageAt: &inbound.CreatedAt}
		if err := tx.Create(&projection).Error; err != nil {
			return err
		}
		if err := tx.Create(&sessionMemberModel{SessionID: sessionID, UserID: inbound.UserID, MemberRole: "owner"}).Error; err != nil {
			return err
		}
		current = feishuGroupMessageModel{ID: uuid.New(), BindingID: active.ID, SenderUserID: inbound.UserID,
			ExternalMessageID: inbound.ExternalMessageID, SpeakerOpenID: inbound.SpeakerOpenID,
			Content: inbound.Content, OccurredAt: inbound.CreatedAt, ProjectionSessionID: &sessionID}
		if err := tx.Create(&current).Error; err != nil {
			return err
		}
		projected = inbound
		projected.ID, projected.SessionID = current.ID, sessionID
		return insertFeishuGroupProjection(tx, current, projected)
	})
	return projected, catalogError("project feishu group message", err)
}

func insertFeishuGroupProjection(tx *gorm.DB, current feishuGroupMessageModel, projected biz.Message) error {
	var history []feishuGroupMessageModel
	if err := tx.Where("binding_id = ? AND (occurred_at, external_message_id) < (?, ?)", current.BindingID, current.OccurredAt, current.ExternalMessageID).
		Order("occurred_at DESC, external_message_id DESC").Limit(feishuGroupHistoryLimit - 1).Find(&history).Error; err != nil {
		return err
	}
	type turn struct {
		sourceID, messageID     uuid.UUID
		role                    biz.MessageRole
		content, speaker, extID string
		at                      time.Time
	}
	turns := make([]turn, 0, 2*len(history))
	for _, item := range history {
		turns = append(turns, turn{item.ID, uuid.New(), biz.MessageUser, item.Content, item.SpeakerOpenID, item.ExternalMessageID, item.OccurredAt})
		if item.RepliedAt != nil && item.RepliedAt.Before(current.OccurredAt) && item.ReplyText != "" {
			turns = append(turns, turn{item.ID, uuid.New(), biz.MessageAssistant, item.ReplyText, "", "", *item.RepliedAt})
		}
	}
	slices.SortStableFunc(turns, func(a, b turn) int {
		if order := a.at.Compare(b.at); order != 0 {
			return order
		}
		if a.extID < b.extID {
			return -1
		}
		if a.extID > b.extID {
			return 1
		}
		return 0
	})
	// 同毫秒消息需要稳定顺序，且触发消息必须位于投影最后。
	boundary := current.OccurredAt.Truncate(time.Microsecond)
	for index := len(turns) - 1; index >= 0; index-- {
		at := turns[index].at.Truncate(time.Microsecond)
		if !at.Before(boundary) {
			at = boundary.Add(-time.Microsecond)
		}
		turns[index].at = at
		boundary = at
	}
	turns = append(turns, turn{current.ID, projected.ID, biz.MessageUser,
		current.Content, current.SpeakerOpenID, current.ExternalMessageID, current.OccurredAt})
	rows := make([]map[string]any, 0, len(turns))
	for _, item := range turns {
		rows = append(rows, map[string]any{
			"id": item.messageID, "user_id": projected.UserID, "session_id": projected.SessionID,
			"role": item.role, "content": item.content, "speaker_open_id": item.speaker, "external_message_id": item.extID,
			"created_at": item.at, "feishu_group_message_id": item.sourceID,
		})
	}
	if err := tx.Table("messages").Create(rows).Error; err != nil {
		return err
	}
	return appendOutbox(tx, biz.OutboxEvent{UserID: projected.UserID, EventType: "message.created",
		AggregateType: "message", AggregateID: projected.ID.String(), DedupeKey: "message.created:" + projected.ID.String(),
		Payload: map[string]any{}})
}

var _ biz.FeishuGroupMessageStore = (*FeishuMessageRepository)(nil)

// PromoteOutboundThread locks the current outbound claim while updating its topic binding.
func (r *FeishuOutboundRepository) PromoteOutboundThread(ctx context.Context, outbound biz.FeishuOutbound, threadID string, now time.Time) (int, error) {
	var affected int64
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		var record feishuOutboundModel
		if err := checkFeishuOutboundClaimResult(feishuOutboundClaim(tx, outbound, now).
			Clauses(clause.Locking{Strength: "UPDATE"}).Find(&record)); err != nil {
			return err
		}
		if threadID == "" || record.ThreadID == "" || record.ThreadID != record.ReplyToMessageID || record.ThreadID == threadID {
			return nil
		}

		bindingAppID := record.AppID
		if record.DefaultRoute {
			// 默认应用的普通会话使用旧的空应用作用域。
			bindingAppID = ""
		}
		result := tx.Model(&externalBindingModel{}).
			Where("provider = 'feishu' AND user_id = ? AND session_id = ? AND app_id = ? AND chat_id = ? AND thread_id = ? AND status = ?",
				record.UserID, record.SessionID, bindingAppID, record.ChatID, record.ThreadID, biz.FeishuBindingActive).
			Update("thread_id", threadID)
		affected = result.RowsAffected
		if result.Error != nil || affected != 0 {
			return result.Error
		}

		// 共享群聊的投影与正式绑定属于不同会话。按入站消息定位该绑定。
		result = tx.Exec(`
UPDATE external_conversation_bindings AS binding
SET thread_id = ?, updated_at = NOW()
FROM feishu_group_messages AS source
WHERE source.binding_id = binding.id AND source.projection_session_id = ? AND source.sender_user_id = ?
  AND source.external_message_id = ? AND binding.provider = 'feishu' AND binding.app_id = ?
  AND binding.chat_id = ? AND binding.thread_id = ? AND binding.status = 'active'`,
			threadID, record.SessionID, record.UserID, record.ReplyToMessageID,
			record.AppID, record.ChatID, record.ThreadID)
		affected = result.RowsAffected
		return result.Error
	})
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return 0, nil
	}
	return int(affected), catalogError("promote feishu outbound thread", err)
}

// ListFeishuGroupRunHistory 读取已验证的投影与当前 Run 自身产物，忽略 Web 写入的消息及 Redis 缓存。
func (r *SessionRepository) ListFeishuGroupRunHistory(ctx context.Context, userID, sessionID, runID uuid.UUID) ([]biz.Message, error) {
	var rows []messageModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Where("user_id = ? AND session_id = ? AND deleted_at IS NULL AND (feishu_group_message_id IS NOT NULL OR run_id = ?)",
			userID, sessionID, runID).Order("created_at, id").Find(&rows).Error
	})
	if err != nil {
		return nil, catalogError("load feishu group projection", err)
	}
	messages := make([]biz.Message, 0, len(rows))
	for _, row := range rows {
		message, err := toBizMessage(row)
		if err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, nil
}

// CompleteGroupReply 将已发送的可见正文与外发完成状态原子提交，供后续同话题 Run 读取。
func (r *FeishuOutboundRepository) CompleteGroupReply(ctx context.Context, outbound biz.FeishuOutbound, text string, now time.Time) error {
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		var record feishuOutboundModel
		result := feishuOutboundClaim(tx, outbound, now).Model(&record).
			Clauses(clause.Returning{}).Updates(feishuOutboundCompletion(now))
		if err := checkFeishuOutboundClaimResult(result); err != nil {
			return err
		}
		projected := tx.Model(&feishuGroupMessageModel{}).
			Where("projection_session_id = ? AND sender_user_id = ? AND external_message_id = ?",
				record.SessionID, record.UserID, record.ReplyToMessageID).
			Updates(map[string]any{"reply_text": text, "replied_at": now})
		if projected.Error != nil {
			return projected.Error
		}
		if projected.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return nil
	})
	return catalogError("complete feishu group reply", err)
}
