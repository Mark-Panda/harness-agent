package biz_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/service/mocks"
	"github.com/google/uuid"
	"go.uber.org/mock/gomock"
)

// TestIdempotencyServiceBeginValidation 验证幂等键、指纹与资源类型的入参校验。
func TestIdempotencyServiceBeginValidation(t *testing.T) {
	ctrl := gomock.NewController(t)
	repo := mocks.NewMockIdempotencyRepository(ctrl)
	service, err := biz.NewIdempotencyService(repo)
	if err != nil {
		t.Fatal(err)
	}
	userID := uuid.New()
	hash := make([]byte, 32)
	repo.EXPECT().Begin(gomock.Any(), userID, "key-1", hash, "session", gomock.Any()).DoAndReturn(
		func(_ context.Context, _ uuid.UUID, _ string, _ []byte, _ string, expiresAt time.Time) (biz.IdempotencyOutcome, error) {
			if !expiresAt.After(time.Now().UTC()) {
				t.Fatalf("idempotency record must expire in the future: %v", expiresAt)
			}
			return biz.IdempotencyOutcome{}, nil
		},
	)
	if _, err := service.Begin(context.Background(), userID, "key-1", "session", hash); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Begin(context.Background(), uuid.Nil, "key-1", "session", hash); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("anonymous begin error = %v", err)
	}
	if _, err := service.Begin(context.Background(), userID, "", "session", hash); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("empty key error = %v", err)
	}
	if _, err := service.Begin(context.Background(), userID, "key-1", "session", []byte("short")); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("short fingerprint error = %v", err)
	}
	if _, err := service.Begin(context.Background(), userID, "key-1", "", hash); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("empty resource type error = %v", err)
	}
	if _, err := biz.NewIdempotencyService(nil); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("nil dependency error = %v", err)
	}
}

// TestIdempotencyServiceCompleteAndReplay 验证响应信封写入与回放解析。
func TestIdempotencyServiceCompleteAndReplay(t *testing.T) {
	ctrl := gomock.NewController(t)
	repo := mocks.NewMockIdempotencyRepository(ctrl)
	service, _ := biz.NewIdempotencyService(repo)
	userID := uuid.New()
	repo.EXPECT().Complete(gomock.Any(), userID, "key-2", gomock.Any(), "resource-id").DoAndReturn(
		func(_ context.Context, _ uuid.UUID, _ string, envelope []byte, resourceID string) error {
			if resourceID != "resource-id" {
				t.Fatalf("resource id was not forwarded: %q", resourceID)
			}
			return nil
		},
	)
	if err := service.Complete(context.Background(), userID, "key-2", 201, []byte(`{"id":"resource-id"}`), "resource-id"); err != nil {
		t.Fatal(err)
	}
	if err := service.Complete(context.Background(), userID, "key-2", 400, []byte(`{}`), ""); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("non-2xx completion error = %v", err)
	}
	repo.EXPECT().Fail(gomock.Any(), userID, "key-3").Return(nil)
	if err := service.Fail(context.Background(), userID, "key-3"); err != nil {
		t.Fatal(err)
	}

	envelope := []byte(`{"status":201,"body":{"id":"abc"}}`)
	outcome, err := biz.ReplayOutcome(envelope)
	if err != nil || !outcome.Replay || outcome.Status != 201 || string(outcome.Body) != `{"id":"abc"}` {
		t.Fatalf("ReplayOutcome() = %#v, %v", outcome, err)
	}
	if _, err := biz.ReplayOutcome([]byte(`{"status":500,"body":{}}`)); !errors.Is(err, biz.ErrIdempotencyConflict) {
		t.Fatalf("invalid stored response error = %v", err)
	}
	if _, err := biz.ReplayOutcome(nil); !errors.Is(err, biz.ErrIdempotencyConflict) {
		t.Fatalf("empty stored response error = %v", err)
	}
}
