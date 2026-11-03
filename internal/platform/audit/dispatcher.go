package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/platform/clock"
	"github.com/Mark-Panda/harness-agent/pkg/metrics"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type Dispatcher struct {
	db       *gorm.DB
	recorder *Recorder
	clock    clock.Clock
}

type outboxRecord struct {
	ID            uuid.UUID `gorm:"primaryKey"`
	UserID        *uuid.UUID
	EventType     string
	AggregateType string
	AggregateID   string
	Payload       datatypes.JSON
	Status        string
	Attempts      int
}

func (outboxRecord) TableName() string {
	return "outbox_events"
}

func NewDispatcher(db *gorm.DB, recorder *Recorder, timeSource clock.Clock) (*Dispatcher, error) {
	if db == nil || recorder == nil {
		return nil, errors.New("outbox database and audit recorder are required")
	}
	if timeSource == nil {
		timeSource = clock.System{}
	}
	return &Dispatcher{db: db, recorder: recorder, clock: timeSource}, nil
}

func (d *Dispatcher) DispatchOnce(ctx context.Context, limit int) (int, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	published := 0
	for range limit {
		ok, err := d.dispatchNext(ctx)
		if err != nil {
			return published, err
		}
		if !ok {
			break
		}
		published++
	}
	return published, nil
}

func (d *Dispatcher) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if _, err := d.DispatchOnce(ctx, 100); err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "outbox dispatch failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (d *Dispatcher) dispatchNext(ctx context.Context) (bool, error) {
	var event outboxRecord
	err := d.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT set_config('app.system', 'on', true)").Error; err != nil {
			return err
		}
		result := tx.Raw(
			"SELECT id, user_id, event_type, aggregate_type, aggregate_id, payload, status, attempts FROM outbox_events WHERE status = ? AND available_at <= ? ORDER BY created_at, id LIMIT 1 FOR UPDATE SKIP LOCKED",
			"pending", d.clock.Now().UTC(),
		).Scan(&event)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return nil
		}
		if err := d.publish(ctx, tx, event); err != nil {
			return err
		}
		return tx.Model(&outboxRecord{}).Where("id = ?", event.ID).Updates(map[string]any{
			"status": "published", "published_at": d.clock.Now().UTC(), "attempts": event.Attempts + 1,
		}).Error
	})
	if err != nil {
		if event.ID != uuid.Nil {
			metrics.IncOutbox(event.EventType, "failure")
			_ = d.recordFailure(context.Background(), event.ID, err)
		}
		return false, err
	}
	if event.ID != uuid.Nil {
		metrics.IncOutbox(event.EventType, "success")
	}
	return event.ID != uuid.Nil, nil
}

func (d *Dispatcher) publish(ctx context.Context, tx *gorm.DB, event outboxRecord) error {
	var payload map[string]any
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return fmt.Errorf("decode outbox payload: %w", err)
	}
	actorID := ""
	actorType := "system"
	if event.UserID != nil {
		actorID = event.UserID.String()
		actorType = "user"
	}
	outcome, _ := payload["outcome"].(string)
	if outcome == "" {
		outcome = "success"
	}
	reason, _ := payload["reason"].(string)
	requestID, _ := payload["request_id"].(string)
	traceID, _ := payload["trace_id"].(string)
	recorder := &Recorder{db: tx, key: d.recorder.key, clock: d.recorder.clock}
	return recorder.Record(ctx, Event{
		ActorType: actorType, ActorID: actorID, Action: event.EventType,
		ResourceType: event.AggregateType, ResourceID: event.AggregateID,
		Outcome: outcome, Reason: reason, RequestID: requestID, TraceID: traceID,
		Source: "outbox", Metadata: payload,
	})
}

func (d *Dispatcher) recordFailure(ctx context.Context, eventID uuid.UUID, cause error) error {
	now := d.clock.Now().UTC()
	return d.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT set_config('app.system', 'on', true)").Error; err != nil {
			return err
		}
		var event outboxRecord
		if err := tx.First(&event, "id = ?", eventID).Error; err != nil {
			return err
		}
		attempts := event.Attempts + 1
		backoff := time.Duration(1<<min(attempts, 10)) * time.Second
		status := "pending"
		if attempts >= 8 {
			status = "failed"
		}
		message := cause.Error()
		if len(message) > 512 {
			message = message[:512]
		}
		return tx.Model(&outboxRecord{}).Where("id = ?", eventID).Updates(map[string]any{
			"status": status, "attempts": attempts, "available_at": now.Add(backoff), "last_error": message,
		}).Error
	})
}
