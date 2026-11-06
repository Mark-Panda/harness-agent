package data

import (
	"context"
	"fmt"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// attachmentModel 对应 attachments 表：附件元数据，二进制在 attachment_contents。
type attachmentModel struct {
	// ID 附件主键。
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// UserID 所属用户，RLS 隔离键。
	UserID uuid.UUID `gorm:"type:uuid"`
	// SessionID 所属会话。
	SessionID uuid.UUID `gorm:"type:uuid"`
	// MessageID 绑定的消息，未发送时为空。
	MessageID *uuid.UUID `gorm:"type:uuid"`
	// FileName 原始文件名。
	FileName string
	// MimeType 规范化媒体类型。
	MimeType string
	// SizeBytes 文件字节数。
	SizeBytes int64
	// SHA256 内容哈希。
	SHA256 string
	// TextExtracted 是否抽取到文本。
	TextExtracted bool
	// TextChars 抽取文本字符数。
	TextChars int
	// TextTruncated 抽取文本是否截断。
	TextTruncated bool
	// CreatedAt 上传时间。
	CreatedAt time.Time
}

// TableName 指定附件表名。
func (attachmentModel) TableName() string { return "attachments" }

// attachmentContentModel 对应 attachment_contents 表：二进制与抽取文本。
type attachmentContentModel struct {
	// AttachmentID 附件主键，与元数据一对一。
	AttachmentID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// Data 二进制内容。
	Data []byte
	// TextContent 抽取文本。
	TextContent string
}

// TableName 指定附件内容表名。
func (attachmentContentModel) TableName() string { return "attachment_contents" }

// AttachmentRepository 是附件持久化的 GORM 实现。
type AttachmentRepository struct {
	db *gorm.DB
}

// NewAttachmentRepository 构造附件仓库。
func NewAttachmentRepository(db *gorm.DB) *AttachmentRepository {
	return &AttachmentRepository{db: db}
}

// Create 在同一事务内写入元数据与内容。
func (r *AttachmentRepository) Create(ctx context.Context, attachment biz.Attachment, data []byte, text string) (biz.Attachment, error) {
	if attachment.ID == uuid.Nil || attachment.UserID == uuid.Nil || attachment.SessionID == uuid.Nil {
		return biz.Attachment{}, fmt.Errorf("%w: attachment identity is required", biz.ErrInvalidConfig)
	}
	now := attachment.CreatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	record := attachmentModel{
		ID: attachment.ID, UserID: attachment.UserID, SessionID: attachment.SessionID,
		FileName: attachment.FileName, MimeType: attachment.MimeType, SizeBytes: attachment.SizeBytes,
		SHA256: attachment.SHA256, TextExtracted: attachment.TextExtracted, TextChars: attachment.TextChars,
		TextTruncated: attachment.TextTruncated, CreatedAt: now,
	}
	err := WithScope(ctx, r.db, Scope{UserID: attachment.UserID}, func(tx *gorm.DB) error {
		if err := tx.Create(&record).Error; err != nil {
			return err
		}
		return tx.Create(&attachmentContentModel{AttachmentID: attachment.ID, Data: data, TextContent: text}).Error
	})
	if err != nil {
		return biz.Attachment{}, catalogError("create attachment", err)
	}
	return toBizAttachment(record), nil
}

// Get 读取附件元数据。
func (r *AttachmentRepository) Get(ctx context.Context, userID, attachmentID uuid.UUID) (biz.Attachment, error) {
	var record attachmentModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Where("id = ? AND user_id = ?", attachmentID, userID).First(&record).Error
	})
	if err != nil {
		return biz.Attachment{}, catalogError("get attachment", err)
	}
	return toBizAttachment(record), nil
}

// Open 读取附件元数据与二进制内容。
func (r *AttachmentRepository) Open(ctx context.Context, userID, attachmentID uuid.UUID) (biz.Attachment, []byte, error) {
	var record attachmentModel
	var content attachmentContentModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		if err := tx.Where("id = ? AND user_id = ?", attachmentID, userID).First(&record).Error; err != nil {
			return err
		}
		return tx.Where("attachment_id = ?", attachmentID).First(&content).Error
	})
	if err != nil {
		return biz.Attachment{}, nil, catalogError("open attachment", err)
	}
	return toBizAttachment(record), content.Data, nil
}

// ListBySession 列出会话附件元数据。
func (r *AttachmentRepository) ListBySession(ctx context.Context, userID, sessionID uuid.UUID) ([]biz.Attachment, error) {
	var records []attachmentModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Where("user_id = ? AND session_id = ?", userID, sessionID).
			Order("created_at ASC, id ASC").Find(&records).Error
	})
	if err != nil {
		return nil, catalogError("list attachments", err)
	}
	items := make([]biz.Attachment, 0, len(records))
	for _, record := range records {
		items = append(items, toBizAttachment(record))
	}
	return items, nil
}

// ListTextContexts 读取已绑定消息的附件抽取文本，供执行器注入上下文。
// 结果按创建时间倒序返回：调用方的注入预算有限，最新附件应优先获得预算。
func (r *AttachmentRepository) ListTextContexts(ctx context.Context, userID, sessionID uuid.UUID) ([]biz.AttachmentText, error) {
	type row struct {
		MessageID   uuid.UUID
		FileName    string
		TextContent string
	}
	var rows []row
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Table("attachments").
			Select("attachments.message_id, attachments.file_name, attachment_contents.text_content").
			Joins("JOIN attachment_contents ON attachment_contents.attachment_id = attachments.id").
			Where("attachments.user_id = ? AND attachments.session_id = ? AND attachments.message_id IS NOT NULL", userID, sessionID).
			Where("attachment_contents.text_content <> ''").
			// 注入预算有限，按最新优先排序，保证当前轮次的附件先拿到预算，
			// 不会因为历史大附件把用户刚上传的文件挤掉。
			Order("attachments.created_at DESC, attachments.id DESC").
			Scan(&rows).Error
	})
	if err != nil {
		return nil, catalogError("list attachment texts", err)
	}
	items := make([]biz.AttachmentText, 0, len(rows))
	for _, item := range rows {
		items = append(items, biz.AttachmentText{MessageID: item.MessageID, FileName: item.FileName, Text: item.TextContent})
	}
	return items, nil
}

// Validate 校验附件归属、会话与未绑定状态。
func (r *AttachmentRepository) Validate(ctx context.Context, userID, sessionID uuid.UUID, attachmentIDs []uuid.UUID) error {
	if len(attachmentIDs) == 0 {
		return nil
	}
	var count int64
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Model(&attachmentModel{}).
			Where("user_id = ? AND session_id = ? AND message_id IS NULL AND id IN ?", userID, sessionID, attachmentIDs).
			Count(&count).Error
	})
	if err != nil {
		return catalogError("validate attachments", err)
	}
	if count != int64(len(attachmentIDs)) {
		return fmt.Errorf("%w: attachments are missing or already bound to a message", biz.ErrResourceConflict)
	}
	return nil
}

// AttachToMessage 绑定未关联消息的附件，数量不匹配时返回冲突。
func (r *AttachmentRepository) AttachToMessage(ctx context.Context, userID, sessionID, messageID uuid.UUID, attachmentIDs []uuid.UUID) error {
	if len(attachmentIDs) == 0 {
		return nil
	}
	var affected int64
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		result := tx.Model(&attachmentModel{}).
			Where("user_id = ? AND session_id = ? AND message_id IS NULL AND id IN ?", userID, sessionID, attachmentIDs).
			Update("message_id", messageID)
		if result.Error != nil {
			return result.Error
		}
		affected = result.RowsAffected
		return nil
	})
	if err != nil {
		return catalogError("attach attachments", err)
	}
	if affected != int64(len(attachmentIDs)) {
		return fmt.Errorf("%w: attachments are missing or already bound to a message", biz.ErrResourceConflict)
	}
	return nil
}

// CountBySession 统计会话附件数量。
func (r *AttachmentRepository) CountBySession(ctx context.Context, userID, sessionID uuid.UUID) (int64, error) {
	var count int64
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Model(&attachmentModel{}).
			Where("user_id = ? AND session_id = ?", userID, sessionID).
			Count(&count).Error
	})
	if err != nil {
		return 0, catalogError("count attachments", err)
	}
	return count, nil
}

// toBizAttachment 将数据库行转换为领域附件。
func toBizAttachment(record attachmentModel) biz.Attachment {
	return biz.Attachment{
		ID: record.ID, UserID: record.UserID, SessionID: record.SessionID, MessageID: record.MessageID,
		FileName: record.FileName, MimeType: record.MimeType, SizeBytes: record.SizeBytes, SHA256: record.SHA256,
		TextExtracted: record.TextExtracted, TextChars: record.TextChars, TextTruncated: record.TextTruncated,
		CreatedAt: record.CreatedAt,
	}
}
