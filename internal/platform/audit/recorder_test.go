package audit

import (
	"context"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Mark-Panda/harness-agent/internal/platform/clock"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

var recorderTestKey = []byte("01234567890123456789012345678901")

func newMockGormDB(t *testing.T) (*gorm.DB, sqlmock.Sqlmock) {
	t.Helper()
	sqlDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatal(err)
	}
	return db, mock
}

func newTestRecorder(t *testing.T, db *gorm.DB, now time.Time) *Recorder {
	t.Helper()
	recorder, err := NewRecorder(db, recorderTestKey, clock.NewFixed(now))
	if err != nil {
		t.Fatal(err)
	}
	return recorder
}

// pseudonymArg 断言写入数据库的 ActorRef 是 HMAC 伪名而非原始用户 ID。
type pseudonymArg struct{ raw string }

func (m pseudonymArg) Match(value driver.Value) bool {
	text, ok := value.(string)
	if !ok || text == m.raw || len(text) != 64 {
		return false
	}
	_, err := hex.DecodeString(text)
	return err == nil
}

func buildChainedEvents(partition string, count int) []auditLogModel {
	occurred := time.Date(2030, time.May, 6, 1, 2, 3, 0, time.UTC)
	events := make([]auditLogModel, 0, count)
	for index := 0; index < count; index++ {
		model := auditLogModel{
			PartitionID: partition, PartitionSeq: int64(index + 1), EventID: uuid.New(),
			SchemaVersion: schemaVersion, OccurredAt: occurred, ActorType: "user", ActorRef: "actor-ref",
			Action: "auth.login", ResourceType: "session", ResourceID: "session-1", Outcome: "success",
			Source: "app", Metadata: datatypes.JSON([]byte(`{"result":"ok"}`)), SignatureVersion: 1,
		}
		if index > 0 {
			model.PrevEventHash = events[index-1].EventHash
		}
		model.EventHash = hashEvent(model)
		events = append(events, model)
	}
	return events
}

func auditLogRows(events []auditLogModel) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{
		"partition_id", "partition_seq", "event_id", "schema_version", "occurred_at", "actor_type",
		"actor_ref", "on_behalf_user_ref", "action", "resource_type", "resource_id", "outcome", "reason",
		"request_id", "trace_id", "run_id", "source", "metadata", "prev_event_hash", "event_hash", "signature_version",
	})
	for _, event := range events {
		var runID any
		if event.RunID != nil {
			runID = event.RunID.String()
		}
		rows.AddRow(
			event.PartitionID, event.PartitionSeq, event.EventID.String(), event.SchemaVersion, event.OccurredAt,
			event.ActorType, event.ActorRef, event.OnBehalfUserRef, event.Action, event.ResourceType,
			event.ResourceID, event.Outcome, event.Reason, event.RequestID, event.TraceID, runID, event.Source,
			[]byte(event.Metadata), event.PrevEventHash, event.EventHash, event.SignatureVersion,
		)
	}
	return rows
}

func TestSanitizeMetadataDropsSecretsAndNestedContent(t *testing.T) {
	metadata, err := sanitizeMetadata(map[string]any{
		"provider": "openai",
		"result":   "success",
		"password": "secret",
		"tool":     "echo",
		"nested":   map[string]any{"private": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err := json.Unmarshal(metadata, &values); err != nil {
		t.Fatal(err)
	}
	if values["provider"] != "openai" || values["result"] != "success" || values["tool"] != "echo" {
		t.Fatalf("allowed metadata missing: %#v", values)
	}
	if _, ok := values["password"]; ok {
		t.Fatal("secret metadata was retained")
	}
	if _, ok := values["nested"]; ok {
		t.Fatal("nested content was retained")
	}
}

func TestPseudonymUsesKeyedHash(t *testing.T) {
	recorder := &Recorder{key: []byte("01234567890123456789012345678901")}
	first := recorder.pseudonym("user-1")
	second := recorder.pseudonym("user-1")
	if first == "user-1" || first != second {
		t.Fatal("pseudonym must be deterministic and keyed")
	}
	if got := recorder.pseudonym(""); got != "" {
		t.Fatalf("empty input must produce empty pseudonym, got %q", got)
	}
}

func TestNewRecorderValidation(t *testing.T) {
	if _, err := NewRecorder(nil, recorderTestKey, nil); err == nil {
		t.Fatal("nil database must be rejected")
	}
	if _, err := NewRecorder(&gorm.DB{}, []byte("short"), nil); err == nil {
		t.Fatal("key shorter than 32 bytes must be rejected")
	}
	recorder, err := NewRecorder(&gorm.DB{}, recorderTestKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := recorder.clock.(clock.System); !ok {
		t.Fatalf("nil clock must default to clock.System, got %T", recorder.clock)
	}
}

func TestRecordRejectsInvalidEvents(t *testing.T) {
	recorder, err := NewRecorder(&gorm.DB{}, recorderTestKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	base := Event{Action: "auth.login", ResourceType: "session", ResourceID: "s1", Outcome: "success"}
	cases := map[string]Event{
		"missing action":        {ResourceType: "session", ResourceID: "s1", Outcome: "success"},
		"missing resource type": {Action: "auth.login", ResourceID: "s1", Outcome: "success"},
		"missing resource id":   {Action: "auth.login", ResourceType: "session", Outcome: "success"},
		"invalid outcome": func() Event {
			event := base
			event.Outcome = "unknown"
			return event
		}(),
		"reason too long": func() Event {
			event := base
			event.Reason = strings.Repeat("x", 513)
			return event
		}(),
		"unmarshalable metadata": func() Event {
			event := base
			event.Metadata = map[string]any{"provider": json.Number("not-a-number")}
			return event
		}(),
	}
	for name, event := range cases {
		t.Run(name, func(t *testing.T) {
			if err := recorder.Record(context.Background(), event); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestRecordAppendsEventToEmptyChain(t *testing.T) {
	db, mock := newMockGormDB(t)
	now := time.Date(2030, time.May, 6, 10, 0, 0, 0, time.UTC)
	recorder := newTestRecorder(t, db, now)
	runID := uuid.New()
	mock.ExpectBegin()
	mock.ExpectExec("pg_advisory_xact_lock").WithArgs("2030-05-06").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`FROM "audit_logs"`).WillReturnRows(sqlmock.NewRows([]string{"partition_seq", "event_hash"}))
	mock.ExpectExec(`INSERT INTO "audit_logs"`).WithArgs(
		"2030-05-06", int64(1), sqlmock.AnyArg(), schemaVersion, now, "user", pseudonymArg{raw: "user-1"},
		pseudonymArg{raw: "admin-9"}, "auth.login", "session", "s1", "success", "ok", "req-1", "trace-1",
		sqlmock.AnyArg(), "app", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), 1,
	).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if err := recorder.Record(context.Background(), Event{
		ActorType: "user", ActorID: "user-1", OnBehalfUserID: "admin-9", Action: "auth.login",
		ResourceType: "session", ResourceID: "s1", Outcome: "success", Reason: "ok",
		RequestID: "req-1", TraceID: "trace-1", RunID: &runID,
		Metadata: map[string]any{"provider": "openai", "password": "drop-me"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRecordChainsOntoExistingHead(t *testing.T) {
	db, mock := newMockGormDB(t)
	now := time.Date(2030, time.May, 6, 10, 0, 0, 0, time.UTC)
	recorder := newTestRecorder(t, db, now)
	previousHash := make([]byte, 32)
	previousHash[0] = 0xaa
	mock.ExpectBegin()
	mock.ExpectExec("pg_advisory_xact_lock").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`FROM "audit_logs"`).WillReturnRows(
		sqlmock.NewRows([]string{"partition_seq", "event_hash"}).AddRow(int64(7), previousHash))
	mock.ExpectExec(`INSERT INTO "audit_logs"`).WithArgs(
		"2030-05-06", int64(8), sqlmock.AnyArg(), schemaVersion, now, "system", "", "",
		"auth.login", "session", "s1", "failure", "", "", "", nil, "app",
		sqlmock.AnyArg(), previousHash, sqlmock.AnyArg(), 1,
	).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if err := recorder.Record(context.Background(), Event{
		Action: "auth.login", ResourceType: "session", ResourceID: "s1", Outcome: "failure",
	}); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRecordWrapsLockFailure(t *testing.T) {
	db, mock := newMockGormDB(t)
	recorder := newTestRecorder(t, db, time.Now())
	mock.ExpectBegin()
	mock.ExpectExec("pg_advisory_xact_lock").WillReturnError(errors.New("lock boom"))
	mock.ExpectRollback()
	err := recorder.Record(context.Background(), Event{Action: "a", ResourceType: "r", ResourceID: "id", Outcome: "success"})
	if err == nil || !strings.Contains(err.Error(), "lock audit partition") {
		t.Fatalf("expected lock error, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRecordWrapsChainHeadFailure(t *testing.T) {
	db, mock := newMockGormDB(t)
	recorder := newTestRecorder(t, db, time.Now())
	mock.ExpectBegin()
	mock.ExpectExec("pg_advisory_xact_lock").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`FROM "audit_logs"`).WillReturnError(errors.New("read boom"))
	mock.ExpectRollback()
	err := recorder.Record(context.Background(), Event{Action: "a", ResourceType: "r", ResourceID: "id", Outcome: "success"})
	if err == nil || !strings.Contains(err.Error(), "read audit chain head") {
		t.Fatalf("expected chain head error, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRecordWrapsInsertFailure(t *testing.T) {
	db, mock := newMockGormDB(t)
	recorder := newTestRecorder(t, db, time.Now())
	mock.ExpectBegin()
	mock.ExpectExec("pg_advisory_xact_lock").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`FROM "audit_logs"`).WillReturnRows(sqlmock.NewRows([]string{"partition_seq", "event_hash"}))
	mock.ExpectExec(`INSERT INTO "audit_logs"`).WillReturnError(errors.New("insert boom"))
	mock.ExpectRollback()
	err := recorder.Record(context.Background(), Event{Action: "a", ResourceType: "r", ResourceID: "id", Outcome: "denied"})
	if err == nil || !strings.Contains(err.Error(), "append audit event") {
		t.Fatalf("expected append error, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyPartitionAcceptsValidChain(t *testing.T) {
	db, mock := newMockGormDB(t)
	recorder := newTestRecorder(t, db, time.Now())
	events := buildChainedEvents("2030-05-06", 3)
	mock.ExpectQuery(`FROM "audit_logs"`).WillReturnRows(auditLogRows(events))
	if err := recorder.VerifyPartition(context.Background(), "2030-05-06"); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyPartitionDetectsSequenceGap(t *testing.T) {
	db, mock := newMockGormDB(t)
	recorder := newTestRecorder(t, db, time.Now())
	events := buildChainedEvents("2030-05-06", 1)
	events[0].PartitionSeq = 2
	mock.ExpectQuery(`FROM "audit_logs"`).WillReturnRows(auditLogRows(events))
	// 链首不是 1 时必须能找到 seq-1 的边界检查点，这里模拟缺失。
	mock.ExpectQuery(`FROM "audit_checkpoints"`).WillReturnRows(
		sqlmock.NewRows([]string{"partition_id", "partition_seq", "event_hash", "signature", "signature_version", "created_at"}))
	err := recorder.VerifyPartition(context.Background(), "2030-05-06")
	if err == nil || !strings.Contains(err.Error(), "audit sequence gap at 2") {
		t.Fatalf("expected sequence gap error, got %v", err)
	}
}

func TestVerifyPartitionAcceptsRetentionAnchor(t *testing.T) {
	db, mock := newMockGormDB(t)
	now := time.Now()
	recorder := newTestRecorder(t, db, now)
	full := buildChainedEvents("2030-05-06", 6)
	// 模拟保留期清理：只剩 seq 6，链首由 seq 5 的签名边界检查点锚定。
	events := full[5:]
	anchorHash := events[0].PrevEventHash
	signature := recorder.checkpointSignature("2030-05-06", 5, anchorHash)
	mock.ExpectQuery(`FROM "audit_logs"`).WillReturnRows(auditLogRows(events))
	mock.ExpectQuery(`FROM "audit_checkpoints"`).WillReturnRows(sqlmock.NewRows([]string{
		"partition_id", "partition_seq", "event_hash", "signature", "signature_version", "created_at",
	}).AddRow("2030-05-06", int64(5), anchorHash, signature, 1, now))
	if err := recorder.VerifyPartition(context.Background(), "2030-05-06"); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyPartitionDetectsPrevHashMismatch(t *testing.T) {
	db, mock := newMockGormDB(t)
	recorder := newTestRecorder(t, db, time.Now())
	events := buildChainedEvents("2030-05-06", 2)
	events[1].PrevEventHash = make([]byte, 32)
	mock.ExpectQuery(`FROM "audit_logs"`).WillReturnRows(auditLogRows(events))
	err := recorder.VerifyPartition(context.Background(), "2030-05-06")
	if err == nil || !strings.Contains(err.Error(), "audit previous hash mismatch") {
		t.Fatalf("expected previous hash mismatch, got %v", err)
	}
}

func TestVerifyPartitionDetectsEventHashMismatch(t *testing.T) {
	db, mock := newMockGormDB(t)
	recorder := newTestRecorder(t, db, time.Now())
	events := buildChainedEvents("2030-05-06", 1)
	events[0].EventHash = make([]byte, 32)
	mock.ExpectQuery(`FROM "audit_logs"`).WillReturnRows(auditLogRows(events))
	err := recorder.VerifyPartition(context.Background(), "2030-05-06")
	if err == nil || !strings.Contains(err.Error(), "audit event hash mismatch") {
		t.Fatalf("expected event hash mismatch, got %v", err)
	}
}

func TestVerifyPartitionWrapsReadFailure(t *testing.T) {
	db, mock := newMockGormDB(t)
	recorder := newTestRecorder(t, db, time.Now())
	mock.ExpectQuery(`FROM "audit_logs"`).WillReturnError(errors.New("db down"))
	err := recorder.VerifyPartition(context.Background(), "2030-05-06")
	if err == nil || !strings.Contains(err.Error(), "read audit chain") {
		t.Fatalf("expected read error, got %v", err)
	}
}

func TestCreateCheckpointAnchorsChainHead(t *testing.T) {
	db, mock := newMockGormDB(t)
	now := time.Date(2030, time.May, 6, 12, 0, 0, 0, time.UTC)
	recorder := newTestRecorder(t, db, now)
	headHash := make([]byte, 32)
	headHash[0] = 0xbb
	mock.ExpectBegin()
	mock.ExpectQuery(`FROM "audit_logs"`).WillReturnRows(
		sqlmock.NewRows([]string{"partition_seq", "event_hash"}).AddRow(int64(3), headHash))
	mock.ExpectExec(`INSERT INTO "audit_checkpoints"`).WithArgs(
		"2030-05-06", int64(3), headHash, sqlmock.AnyArg(), 1, now,
	).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	checkpoint, err := recorder.CreateCheckpoint(context.Background(), "2030-05-06")
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.PartitionID != "2030-05-06" || checkpoint.Seq != 3 {
		t.Fatalf("unexpected checkpoint anchor: %+v", checkpoint)
	}
	if !equalBytes(checkpoint.EventHash, headHash) {
		t.Fatal("checkpoint must anchor the chain head hash")
	}
	if len(checkpoint.Signature) != 32 || !checkpoint.CreatedAt.Equal(now) {
		t.Fatalf("checkpoint signature/created_at invalid: %+v", checkpoint)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCreateCheckpointWrapsHeadFailure(t *testing.T) {
	db, mock := newMockGormDB(t)
	recorder := newTestRecorder(t, db, time.Now())
	mock.ExpectBegin()
	mock.ExpectQuery(`FROM "audit_logs"`).WillReturnError(errors.New("no rows"))
	mock.ExpectRollback()
	if _, err := recorder.CreateCheckpoint(context.Background(), "2030-05-06"); err == nil ||
		!strings.Contains(err.Error(), "create audit checkpoint") {
		t.Fatalf("expected wrapped head failure, got %v", err)
	}
}

func TestCreateCheckpointWrapsInsertFailure(t *testing.T) {
	db, mock := newMockGormDB(t)
	recorder := newTestRecorder(t, db, time.Now())
	mock.ExpectBegin()
	mock.ExpectQuery(`FROM "audit_logs"`).WillReturnRows(
		sqlmock.NewRows([]string{"partition_seq", "event_hash"}).AddRow(int64(1), make([]byte, 32)))
	mock.ExpectExec(`INSERT INTO "audit_checkpoints"`).WillReturnError(errors.New("insert boom"))
	mock.ExpectRollback()
	if _, err := recorder.CreateCheckpoint(context.Background(), "2030-05-06"); err == nil ||
		!strings.Contains(err.Error(), "create audit checkpoint") {
		t.Fatalf("expected wrapped insert failure, got %v", err)
	}
}

func TestVerifyCheckpointRejectsIncomplete(t *testing.T) {
	recorder, err := NewRecorder(&gorm.DB{}, recorderTestKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	valid := Checkpoint{PartitionID: "p", Seq: 1, EventHash: make([]byte, 32)}
	for name, checkpoint := range map[string]Checkpoint{
		"missing partition": {Seq: 1, EventHash: make([]byte, 32)},
		"invalid seq":       {PartitionID: "p", Seq: 0, EventHash: make([]byte, 32)},
		"short hash":        {PartitionID: "p", Seq: 1, EventHash: make([]byte, 16)},
	} {
		t.Run(name, func(t *testing.T) {
			if err := recorder.VerifyCheckpoint(context.Background(), checkpoint); err == nil {
				t.Fatal("incomplete checkpoint must be rejected")
			}
		})
	}
	if err := recorder.VerifyCheckpoint(context.Background(), valid); err == nil {
		t.Fatal("unsigned checkpoint must fail signature verification")
	}
}

func TestVerifyCheckpointAcceptsValidCheckpoint(t *testing.T) {
	db, mock := newMockGormDB(t)
	recorder := newTestRecorder(t, db, time.Now())
	headHash := make([]byte, 32)
	headHash[0] = 0xcc
	checkpoint := Checkpoint{
		PartitionID: "2030-05-06", Seq: 4, EventHash: headHash,
		Signature: recorder.checkpointSignature("2030-05-06", 4, headHash),
	}
	mock.ExpectQuery(`FROM "audit_logs"`).WillReturnRows(
		sqlmock.NewRows([]string{"partition_seq", "event_hash"}).AddRow(int64(4), headHash))
	if err := recorder.VerifyCheckpoint(context.Background(), checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyCheckpointDetectsHashMismatch(t *testing.T) {
	db, mock := newMockGormDB(t)
	recorder := newTestRecorder(t, db, time.Now())
	headHash := make([]byte, 32)
	checkpoint := Checkpoint{
		PartitionID: "2030-05-06", Seq: 4, EventHash: headHash,
		Signature: recorder.checkpointSignature("2030-05-06", 4, headHash),
	}
	stored := make([]byte, 32)
	stored[0] = 0x01
	mock.ExpectQuery(`FROM "audit_logs"`).WillReturnRows(
		sqlmock.NewRows([]string{"partition_seq", "event_hash"}).AddRow(int64(4), stored))
	err := recorder.VerifyCheckpoint(context.Background(), checkpoint)
	if err == nil || !strings.Contains(err.Error(), "audit checkpoint hash mismatch") {
		t.Fatalf("expected hash mismatch, got %v", err)
	}
}

func TestVerifyCheckpointWrapsReadFailure(t *testing.T) {
	db, mock := newMockGormDB(t)
	recorder := newTestRecorder(t, db, time.Now())
	headHash := make([]byte, 32)
	checkpoint := Checkpoint{
		PartitionID: "2030-05-06", Seq: 4, EventHash: headHash,
		Signature: recorder.checkpointSignature("2030-05-06", 4, headHash),
	}
	mock.ExpectQuery(`FROM "audit_logs"`).WillReturnError(errors.New("db down"))
	err := recorder.VerifyCheckpoint(context.Background(), checkpoint)
	if err == nil || !strings.Contains(err.Error(), "read audit checkpoint event") {
		t.Fatalf("expected read error, got %v", err)
	}
}

func TestCheckpointSignatureIsDeterministicAndKeyed(t *testing.T) {
	headHash := make([]byte, 32)
	first := &Recorder{key: recorderTestKey}
	second := &Recorder{key: append([]byte(nil), recorderTestKey...)}
	other := &Recorder{key: []byte("12345678901234567890123456789012")}
	if !equalBytes(first.checkpointSignature("p", 1, headHash), second.checkpointSignature("p", 1, headHash)) {
		t.Fatal("signature must be deterministic for the same key")
	}
	if equalBytes(first.checkpointSignature("p", 1, headHash), other.checkpointSignature("p", 1, headHash)) {
		t.Fatal("signature must differ for a different key")
	}
	if equalBytes(first.checkpointSignature("p", 1, headHash), first.checkpointSignature("p", 2, headHash)) {
		t.Fatal("signature must differ for a different sequence")
	}
}

func TestSanitizeMetadataEdgeCases(t *testing.T) {
	metadata, err := sanitizeMetadata(nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(metadata) != "{}" {
		t.Fatalf("empty metadata must serialize to {}, got %s", metadata)
	}
	metadata, err = sanitizeMetadata(map[string]any{
		"error_code": 42, "model": "gpt-x", "tool": "echo", "result": true,
		"policy_version": int64(2), "family_id": float64(1.5), "session_id": "s", "agent_id": "a",
		"nested": []string{"x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err := json.Unmarshal(metadata, &values); err != nil {
		t.Fatal(err)
	}
	if len(values) != 8 {
		t.Fatalf("all scalar allowlist keys must be retained, got %#v", values)
	}
	if _, err := sanitizeMetadata(map[string]any{"provider": json.Number("oops")}); err == nil {
		t.Fatal("invalid json.Number must surface a marshal error")
	}
}

func TestHashEventIsDeterministicAndSensitive(t *testing.T) {
	events := buildChainedEvents("2030-05-06", 1)
	first := hashEvent(events[0])
	if !equalBytes(first, hashEvent(events[0])) {
		t.Fatal("hashEvent must be deterministic")
	}
	tampered := events[0]
	tampered.Action = "tampered"
	if equalBytes(first, hashEvent(tampered)) {
		t.Fatal("hashEvent must change when a field changes")
	}
	noMetadata := events[0]
	noMetadata.Metadata = nil
	if equalBytes(first, hashEvent(noMetadata)) {
		t.Fatal("hashEvent must change when metadata changes")
	}
}

func TestTableNames(t *testing.T) {
	if (auditLogModel{}).TableName() != "audit_logs" {
		t.Fatal("audit log table name mismatch")
	}
	if (auditCheckpointModel{}).TableName() != "audit_checkpoints" {
		t.Fatal("checkpoint table name mismatch")
	}
	if (outboxRecord{}).TableName() != "outbox_events" {
		t.Fatal("outbox table name mismatch")
	}
}

func equalBytes(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
