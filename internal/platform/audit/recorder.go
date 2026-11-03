package audit

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/platform/clock"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

const schemaVersion = 1

type Event struct {
	ActorType      string
	ActorID        string
	OnBehalfUserID string
	Action         string
	ResourceType   string
	ResourceID     string
	Outcome        string
	Reason         string
	RequestID      string
	TraceID        string
	RunID          *uuid.UUID
	Source         string
	Metadata       map[string]any
}

type Recorder struct {
	db    *gorm.DB
	clock clock.Clock
	key   []byte
}

type auditLogModel struct {
	PartitionID      string `gorm:"primaryKey"`
	PartitionSeq     int64  `gorm:"primaryKey"`
	EventID          uuid.UUID
	SchemaVersion    int
	OccurredAt       time.Time
	ActorType        string
	ActorRef         string
	OnBehalfUserRef  string
	Action           string
	ResourceType     string
	ResourceID       string
	Outcome          string
	Reason           string
	RequestID        string
	TraceID          string
	RunID            *uuid.UUID
	Source           string
	Metadata         datatypes.JSON
	PrevEventHash    []byte
	EventHash        []byte
	SignatureVersion int
}

func (auditLogModel) TableName() string {
	return "audit_logs"
}

func NewRecorder(db *gorm.DB, key []byte, timeSource clock.Clock) (*Recorder, error) {
	if db == nil || len(key) < 32 {
		return nil, errors.New("audit database and a 32-byte pseudonym key are required")
	}
	if timeSource == nil {
		timeSource = clock.System{}
	}
	return &Recorder{db: db, clock: timeSource, key: append([]byte(nil), key...)}, nil
}

func (r *Recorder) Record(ctx context.Context, event Event) error {
	if event.Action == "" || event.ResourceType == "" || event.ResourceID == "" {
		return errors.New("audit action and resource are required")
	}
	if event.Outcome != "success" && event.Outcome != "failure" && event.Outcome != "denied" {
		return errors.New("invalid audit outcome")
	}
	if len(event.Reason) > 512 {
		return errors.New("audit reason is too long")
	}
	if event.ActorType == "" {
		event.ActorType = "system"
	}
	if event.Source == "" {
		event.Source = "app"
	}
	metadata, err := sanitizeMetadata(event.Metadata)
	if err != nil {
		return err
	}
	now := r.clock.Now().UTC()
	partition := now.Format("2006-01-02")
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtext(?))", partition).Error; err != nil {
			return fmt.Errorf("lock audit partition: %w", err)
		}
		var previous auditLogModel
		err := tx.Select("partition_seq", "event_hash").Where("partition_id = ?", partition).
			Order("partition_seq DESC").First(&previous).Error
		sequence := int64(1)
		var previousHash []byte
		if err == nil {
			sequence = previous.PartitionSeq + 1
			previousHash = previous.EventHash
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("read audit chain head: %w", err)
		}
		model := auditLogModel{
			PartitionID: partition, PartitionSeq: sequence, EventID: uuid.New(), SchemaVersion: schemaVersion,
			OccurredAt: now, ActorType: event.ActorType, ActorRef: r.pseudonym(event.ActorID),
			OnBehalfUserRef: r.pseudonym(event.OnBehalfUserID), Action: event.Action,
			ResourceType: event.ResourceType, ResourceID: event.ResourceID, Outcome: event.Outcome,
			Reason: event.Reason, RequestID: event.RequestID, TraceID: event.TraceID, RunID: event.RunID,
			Source: event.Source, Metadata: metadata, PrevEventHash: previousHash, SignatureVersion: 1,
		}
		model.EventHash = hashEvent(model)
		if err := tx.Create(&model).Error; err != nil {
			return fmt.Errorf("append audit event: %w", err)
		}
		return nil
	})
}

func (r *Recorder) VerifyPartition(ctx context.Context, partition string) error {
	var events []auditLogModel
	if err := r.db.WithContext(ctx).Where("partition_id = ?", partition).Order("partition_seq").Find(&events).Error; err != nil {
		return fmt.Errorf("read audit chain: %w", err)
	}
	for index, event := range events {
		if event.PartitionSeq != int64(index+1) {
			return fmt.Errorf("audit sequence gap at %d", event.PartitionSeq)
		}
		if index > 0 && !bytes.Equal(event.PrevEventHash, events[index-1].EventHash) {
			return fmt.Errorf("audit previous hash mismatch at %d", event.PartitionSeq)
		}
		if !bytes.Equal(event.EventHash, hashEvent(event)) {
			return fmt.Errorf("audit event hash mismatch at %d: stored=%x computed=%x metadata=%s occurred=%s", event.PartitionSeq, event.EventHash, hashEvent(event), event.Metadata, event.OccurredAt)
		}
	}
	return nil
}

// Checkpoint 是审计分区某个序号的可验证锚点，用于快速验链与外部存证。
type Checkpoint struct {
	// PartitionID 审计分区。
	PartitionID string
	// Seq 锚定的分区序号。
	Seq int64
	// EventHash 该序号事件的 Hash Chain 值。
	EventHash []byte
	// Signature 使用审计伪名密钥计算的 HMAC，防止检查点被伪造。
	Signature []byte
	// CreatedAt 检查点创建时间（UTC）。
	CreatedAt time.Time
}

// auditCheckpointModel 对应 audit_checkpoints 表。
type auditCheckpointModel struct {
	PartitionID      string `gorm:"primaryKey"`
	PartitionSeq     int64  `gorm:"primaryKey"`
	EventHash        []byte
	Signature        []byte
	SignatureVersion int
	CreatedAt        time.Time
}

// TableName 指定审计检查点表名。
func (auditCheckpointModel) TableName() string {
	return "audit_checkpoints"
}

// CreateCheckpoint 读取分区当前链头并写入带签名检查点。
func (r *Recorder) CreateCheckpoint(ctx context.Context, partition string) (Checkpoint, error) {
	var checkpoint Checkpoint
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var head auditLogModel
		if err := tx.Select("partition_seq", "event_hash").Where("partition_id = ?", partition).
			Order("partition_seq DESC").First(&head).Error; err != nil {
			return fmt.Errorf("read audit chain head: %w", err)
		}
		now := r.clock.Now().UTC()
		checkpoint = Checkpoint{
			PartitionID: partition, Seq: head.PartitionSeq,
			EventHash: append([]byte(nil), head.EventHash...), CreatedAt: now,
		}
		checkpoint.Signature = r.checkpointSignature(checkpoint.PartitionID, checkpoint.Seq, checkpoint.EventHash)
		record := auditCheckpointModel{
			PartitionID: checkpoint.PartitionID, PartitionSeq: checkpoint.Seq,
			EventHash: checkpoint.EventHash, Signature: checkpoint.Signature,
			SignatureVersion: 1, CreatedAt: checkpoint.CreatedAt,
		}
		return tx.Create(&record).Error
	})
	if err != nil {
		return Checkpoint{}, fmt.Errorf("create audit checkpoint: %w", err)
	}
	return checkpoint, nil
}

// VerifyCheckpoint 校验检查点签名与锚点事件哈希，确认检查点未被伪造或错位。
func (r *Recorder) VerifyCheckpoint(ctx context.Context, checkpoint Checkpoint) error {
	if checkpoint.PartitionID == "" || checkpoint.Seq <= 0 || len(checkpoint.EventHash) != 32 {
		return errors.New("audit checkpoint is incomplete")
	}
	if !hmac.Equal(checkpoint.Signature, r.checkpointSignature(checkpoint.PartitionID, checkpoint.Seq, checkpoint.EventHash)) {
		return fmt.Errorf("audit checkpoint signature mismatch for %s#%d", checkpoint.PartitionID, checkpoint.Seq)
	}
	var event auditLogModel
	if err := r.db.WithContext(ctx).Select("partition_seq", "event_hash").
		Where("partition_id = ? AND partition_seq = ?", checkpoint.PartitionID, checkpoint.Seq).
		First(&event).Error; err != nil {
		return fmt.Errorf("read audit checkpoint event: %w", err)
	}
	if !bytes.Equal(event.EventHash, checkpoint.EventHash) {
		return fmt.Errorf("audit checkpoint hash mismatch for %s#%d", checkpoint.PartitionID, checkpoint.Seq)
	}
	return nil
}

// checkpointSignature 计算检查点 HMAC：分区、序号与事件哈希共同参与签名。
func (r *Recorder) checkpointSignature(partition string, seq int64, eventHash []byte) []byte {
	hash := hmac.New(sha256.New, r.key)
	_, _ = hash.Write([]byte(partition))
	_, _ = hash.Write([]byte(":"))
	_, _ = hash.Write([]byte(strconv.FormatInt(seq, 10)))
	_, _ = hash.Write([]byte(":"))
	_, _ = hash.Write(eventHash)
	return hash.Sum(nil)
}

func (r *Recorder) pseudonym(value string) string {
	if value == "" {
		return ""
	}
	hash := hmac.New(sha256.New, r.key)
	_, _ = hash.Write([]byte(value))
	return hex.EncodeToString(hash.Sum(nil))
}

func sanitizeMetadata(input map[string]any) (datatypes.JSON, error) {
	allowedKeys := map[string]bool{
		"error_code": true, "provider": true, "model": true, "tool": true, "result": true,
		"policy_version": true, "family_id": true, "session_id": true, "agent_id": true,
	}
	output := make(map[string]any)
	for key, value := range input {
		if !allowedKeys[key] {
			continue
		}
		switch value.(type) {
		case string, bool, int, int32, int64, float32, float64, json.Number:
			output[key] = value
		}
	}
	if len(output) == 0 {
		return datatypes.JSON([]byte("{}")), nil
	}
	metadata, err := json.Marshal(output)
	if err != nil {
		return nil, fmt.Errorf("marshal audit metadata: %w", err)
	}
	return datatypes.JSON(metadata), nil
}

func hashEvent(model auditLogModel) []byte {
	var metadata any
	if len(model.Metadata) > 0 {
		_ = json.Unmarshal(model.Metadata, &metadata)
	}
	canonical := struct {
		PartitionID      string     `json:"partition_id"`
		PartitionSeq     int64      `json:"partition_seq"`
		EventID          uuid.UUID  `json:"event_id"`
		SchemaVersion    int        `json:"schema_version"`
		OccurredAt       time.Time  `json:"occurred_at"`
		ActorType        string     `json:"actor_type"`
		ActorRef         string     `json:"actor_ref"`
		OnBehalfUserRef  string     `json:"on_behalf_user_ref"`
		Action           string     `json:"action"`
		ResourceType     string     `json:"resource_type"`
		ResourceID       string     `json:"resource_id"`
		Outcome          string     `json:"outcome"`
		Reason           string     `json:"reason"`
		RequestID        string     `json:"request_id"`
		TraceID          string     `json:"trace_id"`
		RunID            *uuid.UUID `json:"run_id"`
		Source           string     `json:"source"`
		Metadata         any        `json:"metadata"`
		PrevEventHash    []byte     `json:"prev_event_hash"`
		SignatureVersion int        `json:"signature_version"`
	}{
		PartitionID: model.PartitionID, PartitionSeq: model.PartitionSeq, EventID: model.EventID,
		SchemaVersion: model.SchemaVersion, OccurredAt: model.OccurredAt.UTC(), ActorType: model.ActorType,
		ActorRef: model.ActorRef, OnBehalfUserRef: model.OnBehalfUserRef, Action: model.Action,
		ResourceType: model.ResourceType, ResourceID: model.ResourceID, Outcome: model.Outcome,
		Reason: model.Reason, RequestID: model.RequestID, TraceID: model.TraceID, RunID: model.RunID,
		Source: model.Source, Metadata: model.Metadata, PrevEventHash: model.PrevEventHash,
		SignatureVersion: model.SignatureVersion,
	}
	value, _ := json.Marshal(canonical)
	hash := sha256.Sum256(value)
	return hash[:]
}
