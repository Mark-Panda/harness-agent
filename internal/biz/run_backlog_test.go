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

// capacityQueue 是带排队容量预检的入队桩，记录预检与入队次数。
type capacityQueue struct {
	hasCapacity   bool
	capacityCalls int
	enqueueCalls  int
}

// EnqueueRun 记录入队调用。
func (q *capacityQueue) EnqueueRun(context.Context, uuid.UUID, uuid.UUID, time.Time) error {
	q.enqueueCalls++
	return nil
}

// HasRunCapacity 返回预设容量并记录预检调用。
func (q *capacityQueue) HasRunCapacity(context.Context, uuid.UUID) (bool, error) {
	q.capacityCalls++
	return q.hasCapacity, nil
}

// newRunServiceWithQueue 构造带会话、消息与入队桩的 RunService。
func newRunServiceWithQueue(t *testing.T, ctrl *gomock.Controller, queue *capacityQueue) (*biz.RunService, *mocks.MockRunRepository, *mocks.MockSessionRepository) {
	t.Helper()
	runs := mocks.NewMockRunRepository(ctrl)
	sessions := mocks.NewMockSessionRepository(ctrl)
	service, err := biz.NewRunService(runs, sessions, biz.WithRunEnqueuer(queue))
	if err != nil {
		t.Fatal(err)
	}
	return service, runs, sessions
}

// expectActiveSessionWithUserMessage 预置一个可创建 Run 的活动会话与用户消息。
func expectActiveSessionWithUserMessage(sessions *mocks.MockSessionRepository, userID, sessionID uuid.UUID) {
	sessions.EXPECT().GetSession(gomock.Any(), userID, sessionID).Return(biz.Session{
		ID: sessionID, UserID: userID, Status: biz.SessionActive,
	}, nil)
	sessions.EXPECT().ListMessages(gomock.Any(), userID, sessionID, gomock.Any(), gomock.Any()).Return(biz.Page[biz.Message]{
		Items: []biz.Message{{
			ID: uuid.New(), SessionID: sessionID, UserID: userID,
			Role: biz.MessageUser, Content: "hello", CreatedAt: time.Now().UTC(),
		}},
	}, nil)
}

// TestRunCreateRejectsSaturatedQueueBeforePersisting 验证排队已满时在创建 Run 之前返回可重试错误。
func TestRunCreateRejectsSaturatedQueueBeforePersisting(t *testing.T) {
	ctrl := gomock.NewController(t)
	queue := &capacityQueue{hasCapacity: false}
	service, _, sessions := newRunServiceWithQueue(t, ctrl, queue)
	userID, sessionID := uuid.New(), uuid.New()
	principal := biz.Principal{UserID: userID, Role: biz.RoleUser}
	expectActiveSessionWithUserMessage(sessions, userID, sessionID)

	// 不设置 CreateRun 期望：一旦落库 gomock 会判定失败。
	_, err := service.Create(context.Background(), principal, biz.CreateRunInput{
		SessionID: sessionID, Mode: biz.AgentModeReact,
	})
	if !errors.Is(err, biz.ErrQueueSaturated) {
		t.Fatalf("Create() error = %v, want queue saturated", err)
	}
	if queue.capacityCalls != 1 || queue.enqueueCalls != 0 {
		t.Fatalf("capacity calls = %d, enqueue calls = %d", queue.capacityCalls, queue.enqueueCalls)
	}
}

// TestRunCreateEnqueuesWhenQueueHasCapacity 验证预检通过后仍按原路径落库并入队。
func TestRunCreateEnqueuesWhenQueueHasCapacity(t *testing.T) {
	ctrl := gomock.NewController(t)
	queue := &capacityQueue{hasCapacity: true}
	service, runs, sessions := newRunServiceWithQueue(t, ctrl, queue)
	userID, sessionID := uuid.New(), uuid.New()
	principal := biz.Principal{UserID: userID, Role: biz.RoleUser}
	expectActiveSessionWithUserMessage(sessions, userID, sessionID)
	runs.EXPECT().CreateRun(gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, run biz.Run) (biz.Run, error) {
		return run, nil
	})

	if _, err := service.Create(context.Background(), principal, biz.CreateRunInput{
		SessionID: sessionID, Mode: biz.AgentModeReact,
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if queue.capacityCalls != 1 || queue.enqueueCalls != 1 {
		t.Fatalf("capacity calls = %d, enqueue calls = %d", queue.capacityCalls, queue.enqueueCalls)
	}
}
