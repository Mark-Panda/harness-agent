package audit

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Mark-Panda/harness-agent/internal/platform/clock"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func newTestDispatcher(t *testing.T, now time.Time) (*Dispatcher, sqlmock.Sqlmock) {
	t.Helper()
	db, mock := newMockGormDB(t)
	recorder := newTestRecorder(t, db, now)
	dispatcher, err := NewDispatcher(db, recorder, clock.NewFixed(now))
	if err != nil {
		t.Fatal(err)
	}
	return dispatcher, mock
}

func outboxRow(id uuid.UUID, userID *uuid.UUID, payload string, attempts int) *sqlmock.Rows {
	var user any
	if userID != nil {
		user = userID.String()
	}
	return sqlmock.NewRows([]string{
		"id", "user_id", "event_type", "aggregate_type", "aggregate_id", "payload", "status", "attempts",
	}).AddRow(id.String(), user, "auth.user.registered", "user", id.String(), []byte(payload), "pending", attempts)
}

func expectEmptyOutboxCycle(mock sqlmock.Sqlmock) {
	mock.ExpectBegin()
	mock.ExpectExec("set_config").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("FROM outbox_events").WillReturnRows(sqlmock.NewRows([]string{
		"id", "user_id", "event_type", "aggregate_type", "aggregate_id", "payload", "status", "attempts",
	}))
	mock.ExpectCommit()
}

func expectFailureSettlement(mock sqlmock.Sqlmock, eventID uuid.UUID, attemptsBefore int, wantAttempts int, wantStatus string) {
	mock.ExpectBegin()
	mock.ExpectExec("set_config").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`FROM "outbox_events"`).WillReturnRows(sqlmock.NewRows([]string{
		"id", "user_id", "event_type", "aggregate_type", "aggregate_id", "payload", "status", "attempts",
	}).AddRow(eventID.String(), nil, "auth.user.registered", "user", eventID.String(), []byte("{}"), "pending", attemptsBefore))
	mock.ExpectExec(`UPDATE "outbox_events"`).WithArgs(
		wantAttempts, sqlmock.AnyArg(), sqlmock.AnyArg(), wantStatus, eventID.String(),
	).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
}

func TestNewDispatcherValidation(t *testing.T) {
	recorder := &Recorder{key: recorderTestKey}
	if _, err := NewDispatcher(nil, recorder, nil); err == nil {
		t.Fatal("nil database must be rejected")
	}
	if _, err := NewDispatcher(&gorm.DB{}, nil, nil); err == nil {
		t.Fatal("nil recorder must be rejected")
	}
	dispatcher, err := NewDispatcher(&gorm.DB{}, recorder, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := dispatcher.clock.(clock.System); !ok {
		t.Fatalf("nil clock must default to clock.System, got %T", dispatcher.clock)
	}
}

func TestDispatchOnceReturnsZeroWhenQueueEmpty(t *testing.T) {
	dispatcher, mock := newTestDispatcher(t, time.Now())
	expectEmptyOutboxCycle(mock)
	published, err := dispatcher.DispatchOnce(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if published != 0 {
		t.Fatalf("published = %d, want 0", published)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDispatchOncePublishesPendingEvent(t *testing.T) {
	now := time.Date(2030, time.May, 6, 8, 0, 0, 0, time.UTC)
	dispatcher, mock := newTestDispatcher(t, now)
	eventID := uuid.New()
	userID := uuid.New()
	mock.ExpectBegin()
	mock.ExpectExec("set_config").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("FROM outbox_events").WillReturnRows(outboxRow(eventID, &userID,
		`{"outcome":"success","reason":"deployed","request_id":"req-9","trace_id":"trace-9","provider":"test","password":"drop-me"}`, 0))
	mock.ExpectExec("SAVEPOINT").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("pg_advisory_xact_lock").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`FROM "audit_logs"`).WillReturnRows(sqlmock.NewRows([]string{"partition_seq", "event_hash"}))
	mock.ExpectExec(`INSERT INTO "audit_logs"`).WithArgs(
		"2030-05-06", int64(1), sqlmock.AnyArg(), schemaVersion, now, "user", pseudonymArg{raw: userID.String()}, "",
		"auth.user.registered", "user", eventID.String(), "success", "deployed", "req-9", "trace-9",
		nil, "outbox", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), 1,
	).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE "outbox_events"`).WithArgs(
		1, now, "published", eventID.String(),
	).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	published, err := dispatcher.DispatchOnce(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if published != 1 {
		t.Fatalf("published = %d, want 1", published)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDispatchOnceRetriesOnUndecodablePayload(t *testing.T) {
	dispatcher, mock := newTestDispatcher(t, time.Now())
	eventID := uuid.New()
	mock.ExpectBegin()
	mock.ExpectExec("set_config").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("FROM outbox_events").WillReturnRows(outboxRow(eventID, nil, `{invalid-json`, 0))
	mock.ExpectRollback()
	expectFailureSettlement(mock, eventID, 0, 1, "pending")
	published, err := dispatcher.DispatchOnce(context.Background(), 10)
	if err == nil || !strings.Contains(err.Error(), "decode outbox payload") {
		t.Fatalf("expected decode error, got %v", err)
	}
	if published != 0 {
		t.Fatalf("published = %d, want 0", published)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDispatchOnceMarksEventFailedAfterEightAttempts(t *testing.T) {
	dispatcher, mock := newTestDispatcher(t, time.Now())
	eventID := uuid.New()
	mock.ExpectBegin()
	mock.ExpectExec("set_config").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("FROM outbox_events").WillReturnRows(outboxRow(eventID, nil, `{invalid-json`, 7))
	mock.ExpectRollback()
	expectFailureSettlement(mock, eventID, 7, 8, "failed")
	if _, err := dispatcher.DispatchOnce(context.Background(), 10); err == nil {
		t.Fatal("expected publish failure")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDispatchOnceRollsBackNestedTransactionOnAuditFailure(t *testing.T) {
	dispatcher, mock := newTestDispatcher(t, time.Now())
	eventID := uuid.New()
	mock.ExpectBegin()
	mock.ExpectExec("set_config").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("FROM outbox_events").WillReturnRows(outboxRow(eventID, nil, `{"outcome":"success"}`, 0))
	mock.ExpectExec("SAVEPOINT").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("pg_advisory_xact_lock").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`FROM "audit_logs"`).WillReturnError(errors.New("audit store down"))
	mock.ExpectExec("ROLLBACK TO SAVEPOINT").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()
	expectFailureSettlement(mock, eventID, 0, 1, "pending")
	_, err := dispatcher.DispatchOnce(context.Background(), 10)
	if err == nil || !strings.Contains(err.Error(), "read audit chain head") {
		t.Fatalf("expected audit failure, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDispatchOncePropagatesOutboxQueryFailure(t *testing.T) {
	dispatcher, mock := newTestDispatcher(t, time.Now())
	mock.ExpectBegin()
	mock.ExpectExec("set_config").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("FROM outbox_events").WillReturnError(errors.New("outbox down"))
	mock.ExpectRollback()
	published, err := dispatcher.DispatchOnce(context.Background(), 10)
	if err == nil || !strings.Contains(err.Error(), "outbox down") {
		t.Fatalf("expected outbox error, got %v", err)
	}
	if published != 0 {
		t.Fatalf("published = %d, want 0", published)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDispatcherRunStopsOnContextCancel(t *testing.T) {
	dispatcher, mock := newTestDispatcher(t, time.Now())
	expectEmptyOutboxCycle(mock)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	dispatcher.Run(ctx, 0) // 间隔非法时必须回退到默认 1s，取消前只跑一轮。
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
