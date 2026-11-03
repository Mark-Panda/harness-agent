package data

import (
	"encoding/json"
	"errors"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// outboxEventModel 对应 outbox_events 表：待发布的审计/领域事件。
type outboxEventModel struct {
	// ID 事件主键。
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// UserID 事件归属用户。
	UserID uuid.UUID `gorm:"type:uuid"`
	// EventType 事件类型，同时作为审计 action。
	EventType string
	// AggregateType 聚合类型。
	AggregateType string
	// AggregateID 聚合标识。
	AggregateID string
	// DedupeKey 去重键，唯一索引保证事件只发布一次。
	DedupeKey string
	// Payload 审计元数据 JSON（白名单字段）。
	Payload datatypes.JSON
	// Status 发布状态：pending/published/failed。
	Status string
	// Attempts 已尝试发布次数。
	Attempts int
}

// TableName 指定 Outbox 表名。
func (outboxEventModel) TableName() string {
	return "outbox_events"
}

// appendOutbox 在调用方事务内追加一条 Outbox 事件，保证业务写入与审计事件原子提交。
// 关键步骤：按 dedupe_key 冲突忽略，确保重试不会产生重复审计。
func appendOutbox(tx *gorm.DB, event biz.OutboxEvent) error {
	if event.UserID == uuid.Nil || event.EventType == "" || event.AggregateType == "" || event.AggregateID == "" || event.DedupeKey == "" {
		return errors.New("outbox user, event type, aggregate and dedupe key are required")
	}
	if event.Payload == nil {
		event.Payload = map[string]any{}
	}
	payload, err := json.Marshal(event.Payload)
	if err != nil {
		return err
	}
	model := outboxEventModel{
		ID: uuid.New(), UserID: event.UserID, EventType: event.EventType,
		AggregateType: event.AggregateType, AggregateID: event.AggregateID,
		DedupeKey: event.DedupeKey, Payload: datatypes.JSON(payload), Status: "pending",
	}
	return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "dedupe_key"}}, DoNothing: true}).Create(&model).Error
}
