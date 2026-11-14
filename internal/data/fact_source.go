package data

import (
	"context"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ListFactSourceMessages returns only messages that may be sent for extraction
// under the current session policy and, for a Feishu topic, speaker identity.
func (r *SessionRepository) ListFactSourceMessages(ctx context.Context, task biz.FactTask) ([]biz.Message, error) {
	var messages []biz.Message
	err := WithScope(ctx, r.db, Scope{UserID: task.UserID}, func(tx *gorm.DB) error {
		var err error
		messages, err = loadFactSourceMessages(tx, task)
		return err
	})
	return messages, catalogError("list fact source messages", err)
}

// loadFactSourceMessages locks the eligibility rows and source messages until
// the surrounding transaction ends. For speaker tasks it leaves the RLS scope
// set to the verified fact recipient.
func loadFactSourceMessages(tx *gorm.DB, task biz.FactTask) ([]biz.Message, error) {
	speaker := task.SpeakerOpenID != ""
	if task.UserID == uuid.Nil || task.SessionID == uuid.Nil || len(task.MessageIDs) == 0 || len(task.MessageIDs) > 64 ||
		speaker != (task.FactOwnerID != uuid.Nil) {
		return nil, biz.ErrFactSourceInvalid
	}
	policy := privateMemorySession
	if speaker {
		policy = `s.memory_policy = 'group' AND s.source = 'feishu' AND NOT s.feishu_group_projection`
	}
	var eligible bool
	if err := tx.Raw(`SELECT true FROM sessions s WHERE s.user_id = ? AND s.id = ? AND s.status = 'active' AND `+
		policy+` FOR SHARE OF s`, task.UserID, task.SessionID).Scan(&eligible).Error; err != nil {
		return nil, err
	}
	if !eligible {
		return nil, biz.ErrFactSourceInvalid
	}

	var records []messageModel
	query := tx.Where(`user_id = ? AND session_id = ? AND id IN ? AND deleted_at IS NULL
		AND feishu_group_message_id IS NULL AND content ~ '[^[:space:]]'`, task.UserID, task.SessionID, task.MessageIDs)
	// 用户清理记忆后，清理前的消息不得再次参与事实提取或重建画像。
	query = query.Where(memoryClearedFilter, task.UserID)
	if speaker {
		query = query.Where("role = 'user' AND speaker_open_id = ?", task.SpeakerOpenID)
	} else {
		query = query.Where("role IN ('user', 'assistant')")
	}
	if err := query.Clauses(clause.Locking{Strength: "SHARE"}).
		Order("created_at ASC, id ASC").Find(&records).Error; err != nil {
		return nil, err
	}
	if len(records) != len(task.MessageIDs) {
		return nil, biz.ErrFactSourceInvalid
	}
	messages, err := toBizMessages(records)
	if err != nil {
		return nil, err
	}
	if biz.SummarySourceFingerprint(messages) != task.Fingerprint {
		return nil, biz.ErrFactSourceInvalid
	}
	if speaker {
		if err := authorizeFactSpeaker(tx, task); err != nil {
			return nil, err
		}
	}
	return messages, nil
}

func authorizeFactSpeaker(tx *gorm.DB, task biz.FactTask) error {
	var tenants []string
	if err := tx.Model(&externalBindingModel{}).Clauses(clause.Locking{Strength: "SHARE"}).
		Where("user_id = ? AND session_id = ? AND provider = 'feishu' AND thread_id <> 'p2p' AND status = ?",
			task.UserID, task.SessionID, biz.FeishuBindingActive).
		Pluck("tenant_key", &tenants).Error; err != nil {
		return err
	}
	if len(tenants) == 0 {
		return biz.ErrFactSourceInvalid
	}
	// Read only the recipient's identity and keep that RLS scope for any write.
	if task.FactOwnerID != task.UserID {
		if err := tx.Exec("SELECT set_config('app.user_id', ?, true)", task.FactOwnerID.String()).Error; err != nil {
			return err
		}
	}
	var eligible bool
	if err := tx.Raw(`SELECT true FROM user_identities i JOIN users u ON u.id = i.user_id
		WHERE i.user_id = ? AND i.provider = 'feishu' AND i.tenant_key IN ?
		AND i.external_subject = ? AND u.status = 'active'
		LIMIT 1 FOR SHARE OF i, u`, task.FactOwnerID, tenants, task.SpeakerOpenID).Scan(&eligible).Error; err != nil {
		return err
	}
	if !eligible {
		return biz.ErrFactSourceInvalid
	}
	return nil
}
