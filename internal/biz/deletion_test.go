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

// fakeDeletionRepository 是删除请求仓储的内存实现。
type fakeDeletionRepository struct {
	request     biz.DeletionRequest
	claimed     bool
	created     []biz.DeletionRequest
	completed   int
	failed      int
	renewed     int
	getErr      error
	claimErr    error
	completeErr error
	active      biz.DeletionRequest
	dedupe      bool
}

// Create 记录创建或返回既有请求。
func (f *fakeDeletionRepository) Create(_ context.Context, request biz.DeletionRequest, _ biz.OutboxEvent) (biz.DeletionRequest, error) {
	if f.dedupe {
		return f.active, nil
	}
	f.created = append(f.created, request)
	return request, nil
}

// Get 返回预设请求。
func (f *fakeDeletionRepository) Get(context.Context, uuid.UUID, uuid.UUID) (biz.DeletionRequest, error) {
	return f.request, f.getErr
}

// List 返回单条分页。
func (f *fakeDeletionRepository) List(context.Context, uuid.UUID, int, string) (biz.Page[biz.DeletionRequest], error) {
	return biz.Page[biz.DeletionRequest]{Items: []biz.DeletionRequest{f.request}}, nil
}

// Claim 返回预设请求一次。
func (f *fakeDeletionRepository) Claim(context.Context, string, time.Duration, time.Time) (biz.DeletionRequest, bool, error) {
	if f.claimErr != nil {
		return biz.DeletionRequest{}, false, f.claimErr
	}
	if f.claimed {
		return biz.DeletionRequest{}, false, nil
	}
	f.claimed = true
	return f.request, true, nil
}

// Renew 记录续约。
func (f *fakeDeletionRepository) Renew(context.Context, uuid.UUID, string, int64, time.Duration, time.Time) error {
	f.renewed++
	return nil
}

// Complete 记录完成。
func (f *fakeDeletionRepository) Complete(context.Context, uuid.UUID, string, int64, time.Time, biz.OutboxEvent) error {
	f.completed++
	return f.completeErr
}

// Fail 记录失败。
func (f *fakeDeletionRepository) Fail(context.Context, uuid.UUID, string, int64, time.Time, string, time.Time) error {
	f.failed++
	return nil
}

// fakeDeletionExecutor 是删除执行器替身。
type fakeDeletionExecutor struct {
	sessionCalls int
	memoryCalls  int
	accountCalls int
	err          error
	userID       uuid.UUID
	sessionID    uuid.UUID
}

// DeleteSession 记录会话删除。
func (f *fakeDeletionExecutor) DeleteSession(_ context.Context, userID, sessionID uuid.UUID) error {
	f.sessionCalls++
	f.userID, f.sessionID = userID, sessionID
	return f.err
}

// DeleteMemory 记录记忆删除。
func (f *fakeDeletionExecutor) DeleteMemory(_ context.Context, userID uuid.UUID) error {
	f.memoryCalls++
	f.userID = userID
	return f.err
}

// DeleteAccount 记录账号删除。
func (f *fakeDeletionExecutor) DeleteAccount(_ context.Context, userID uuid.UUID) error {
	f.accountCalls++
	f.userID = userID
	return f.err
}

// TestDeletionServiceRequests 验证会话归属校验、记忆删除与重复请求幂等。
func TestDeletionServiceRequests(t *testing.T) {
	ctrl := gomock.NewController(t)
	sessions := mocks.NewMockSessionRepository(ctrl)
	repo := &fakeDeletionRepository{}
	service, err := biz.NewDeletionService(repo, sessions, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	userID, sessionID := uuid.New(), uuid.New()
	sessions.EXPECT().GetSession(gomock.Any(), userID, sessionID).Return(biz.Session{ID: sessionID, UserID: userID, Status: biz.SessionActive}, nil).AnyTimes()
	request, err := service.RequestSessionDeletion(context.Background(), biz.Principal{UserID: userID}, sessionID, "api")
	if err != nil || request.ResourceType != biz.DeletionResourceSession || request.RequestedSource != "api" {
		t.Fatalf("request = %#v err=%v", request, err)
	}
	if _, err := service.RequestSessionDeletion(context.Background(), biz.Principal{}, sessionID, ""); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("anonymous request error = %v", err)
	}

	repo.dedupe = true
	repo.active = request
	duplicate, err := service.RequestSessionDeletion(context.Background(), biz.Principal{UserID: userID}, sessionID, "web")
	if err != nil || duplicate.ID != request.ID {
		t.Fatalf("duplicate = %#v err=%v", duplicate, err)
	}

	repo.dedupe = false
	memory, err := service.RequestMemoryDeletion(context.Background(), biz.Principal{UserID: userID}, "")
	if err != nil || memory.ResourceType != biz.DeletionResourceMemory || memory.ResourceID != userID.String() {
		t.Fatalf("memory request = %#v err=%v", memory, err)
	}
	account, err := service.RequestAccountDeletion(context.Background(), biz.Principal{UserID: userID}, "api")
	if err != nil || account.ResourceType != biz.DeletionResourceAccount || account.ResourceID != userID.String() {
		t.Fatalf("account request = %#v err=%v", account, err)
	}
	if _, err := service.RequestAccountDeletion(context.Background(), biz.Principal{}, ""); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("anonymous account request error = %v", err)
	}
	if _, err := service.Get(context.Background(), biz.Principal{}, uuid.New()); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("get anonymous error = %v", err)
	}
	if _, err := service.List(context.Background(), biz.Principal{}, 10, ""); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("list anonymous error = %v", err)
	}
	if _, err := biz.NewDeletionService(nil, nil, nil, nil); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("constructor error = %v", err)
	}
}

// TestDeletionWorkerExecutes 验证会话/记忆删除成功、非法资源与执行失败。
func TestDeletionWorkerExecutes(t *testing.T) {
	request := biz.DeletionRequest{
		ID: uuid.New(), UserID: uuid.New(), ResourceType: biz.DeletionResourceSession,
		ResourceID: uuid.New().String(), Status: biz.DeletionRunning, Attempts: 1, FencingToken: 1,
	}
	repo := &fakeDeletionRepository{request: request}
	executor := &fakeDeletionExecutor{}
	worker, err := biz.NewDeletionWorker(biz.DeletionWorkerDependencies{Requests: repo, Executor: executor},
		biz.DeletionWorkerConfig{WorkerID: "deletion-test", Heartbeat: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if repo.completed != 1 || executor.sessionCalls != 1 || executor.userID != request.UserID {
		t.Fatalf("completed=%d sessionCalls=%d", repo.completed, executor.sessionCalls)
	}

	// 非法资源 ID 失败并进入重试。
	repo.claimed = false
	repo.request.ResourceID = "not-a-uuid"
	executor = &fakeDeletionExecutor{}
	worker, err = biz.NewDeletionWorker(biz.DeletionWorkerDependencies{Requests: repo, Executor: executor},
		biz.DeletionWorkerConfig{WorkerID: "deletion-test", Heartbeat: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if repo.failed != 1 || executor.sessionCalls != 0 {
		t.Fatalf("failed=%d calls=%d", repo.failed, executor.sessionCalls)
	}

	// 执行器错误进入重试。
	repo.claimed = false
	repo.request.ResourceType = biz.DeletionResourceMemory
	repo.request.ResourceID = request.UserID.String()
	executor = &fakeDeletionExecutor{err: errors.New("db busy")}
	worker, err = biz.NewDeletionWorker(biz.DeletionWorkerDependencies{Requests: repo, Executor: executor},
		biz.DeletionWorkerConfig{WorkerID: "deletion-test", Heartbeat: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if repo.failed != 2 || executor.memoryCalls != 1 {
		t.Fatalf("failed=%d memoryCalls=%d", repo.failed, executor.memoryCalls)
	}

	// 账号删除走独立执行分支并成功完成。
	repo.claimed = false
	repo.request.ResourceType = biz.DeletionResourceAccount
	repo.request.ResourceID = request.UserID.String()
	executor = &fakeDeletionExecutor{}
	worker, err = biz.NewDeletionWorker(biz.DeletionWorkerDependencies{Requests: repo, Executor: executor},
		biz.DeletionWorkerConfig{WorkerID: "deletion-test", Heartbeat: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if executor.accountCalls != 1 || repo.completed != 2 {
		t.Fatalf("accountCalls=%d completed=%d", executor.accountCalls, repo.completed)
	}
}

// TestDeletionWorkerStartAndClaimError 验证循环取消与领取错误。
func TestDeletionWorkerStartAndClaimError(t *testing.T) {
	repo := &fakeDeletionRepository{claimed: true, claimErr: errors.New("db down")}
	worker, err := biz.NewDeletionWorker(biz.DeletionWorkerDependencies{Requests: repo, Executor: &fakeDeletionExecutor{}},
		biz.DeletionWorkerConfig{WorkerID: "deletion-test"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	worker.Start(ctx)
	if claimed, err := worker.ProcessOne(context.Background()); err == nil || claimed {
		t.Fatalf("claim error: claimed=%v err=%v", claimed, err)
	}
	if _, err := biz.NewDeletionWorker(biz.DeletionWorkerDependencies{}, biz.DeletionWorkerConfig{}); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("constructor error = %v", err)
	}
}
