package biz_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/service/mocks"
	"github.com/Mark-Panda/harness-agent/pkg/agent/model"
	"github.com/google/uuid"
	"go.uber.org/mock/gomock"
)

// fakeLimiter 记录槽位获取与释放，并按预设结果返回是否获得槽位。
type fakeLimiter struct {
	mu       sync.Mutex
	allowed  bool
	err      error
	acquired int
	released int
	keys     []string
}

// TryAcquire 返回预设结果并统计调用。
func (f *fakeLimiter) TryAcquire(_ context.Context, key string) (func(), bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = append(f.keys, key)
	if f.err != nil {
		return nil, false, f.err
	}
	if !f.allowed {
		return nil, false, nil
	}
	f.acquired++
	return func() {
		f.mu.Lock()
		f.released++
		f.mu.Unlock()
	}, true, nil
}

// Acquire 直接复用 TryAcquire，测试不涉及阻塞路径。
func (f *fakeLimiter) Acquire(ctx context.Context, key string) (func(), error) {
	release, acquired, err := f.TryAcquire(ctx, key)
	if err != nil {
		return nil, err
	}
	if !acquired {
		return nil, errors.New("limit reached")
	}
	return release, nil
}

// newWorkerWithLimiters 构造带舱壁的 Worker。
func newWorkerWithLimiters(t *testing.T, ctrl *gomock.Controller, global, user model.ConcurrencyLimiter) (*biz.ExecutionWorker, *mocks.MockJobQueue, *mocks.MockRunRepository) {
	t.Helper()
	jobs := mocks.NewMockJobQueue(ctrl)
	runs := mocks.NewMockRunRepository(ctrl)
	worker, err := biz.NewExecutionWorker(biz.ExecutionWorkerDependencies{
		Jobs: jobs, Runs: runs, Sessions: mocks.NewMockSessionRepository(ctrl),
		Providers: fakeProviderCredentials{
			provider:   biz.Provider{ID: uuid.New(), Status: biz.ProviderEnabled, BaseURL: "https://models.example.com"},
			credential: biz.ProviderCredential{KeyVersion: "v1", Encrypted: []byte("x")},
		},
		Opener: fakeOpener{key: "test-key"}, Attempts: mocks.NewMockModelCallRepository(ctrl),
		GlobalLimiter: global, UserLimiter: user,
	}, biz.WorkerConfig{
		WorkerID: "worker-1", PollInterval: time.Millisecond, Lease: time.Minute,
		Heartbeat: time.Second, CancelPoll: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	return worker, jobs, runs
}

// TestWorkerDefersJobWhenGlobalLimitReached 验证全局舱壁满时退回任务且不执行。
func TestWorkerDefersJobWhenGlobalLimitReached(t *testing.T) {
	ctrl := gomock.NewController(t)
	userID, runID := uuid.New(), uuid.New()
	global := &fakeLimiter{allowed: false}
	worker, jobs, _ := newWorkerWithLimiters(t, ctrl, global, &fakeLimiter{allowed: true})
	job := jobFixture(userID, runID)
	jobs.EXPECT().Claim(gomock.Any(), "worker-1", "agent.run", gomock.Any(), gomock.Any()).Return(job, true, nil)
	jobs.EXPECT().Release(gomock.Any(), job.ID, "worker-1", job.FencingToken, gomock.Any(), gomock.Any()).Return(nil)
	if processed, err := worker.ProcessOne(context.Background()); err != nil || processed {
		t.Fatalf("ProcessOne() = %t, %v", processed, err)
	}
}

// TestWorkerDefersJobWhenUserLimitReached 验证用户舱壁满时释放全局槽位并退回任务。
func TestWorkerDefersJobWhenUserLimitReached(t *testing.T) {
	ctrl := gomock.NewController(t)
	userID, runID := uuid.New(), uuid.New()
	global := &fakeLimiter{allowed: true}
	user := &fakeLimiter{allowed: false}
	worker, jobs, _ := newWorkerWithLimiters(t, ctrl, global, user)
	job := jobFixture(userID, runID)
	jobs.EXPECT().Claim(gomock.Any(), "worker-1", "agent.run", gomock.Any(), gomock.Any()).Return(job, true, nil)
	jobs.EXPECT().Release(gomock.Any(), job.ID, "worker-1", job.FencingToken, gomock.Any(), gomock.Any()).Return(nil)
	if processed, err := worker.ProcessOne(context.Background()); err != nil || processed {
		t.Fatalf("ProcessOne() = %t, %v", processed, err)
	}
	global.mu.Lock()
	defer global.mu.Unlock()
	if global.released != 1 {
		t.Fatalf("global slots released = %d", global.released)
	}
	user.mu.Lock()
	defer user.mu.Unlock()
	if len(user.keys) != 1 || user.keys[0] != "user:"+userID.String() {
		t.Fatalf("user limiter keys = %v", user.keys)
	}
}

// TestWorkerReleasesSlotsAfterExecution 验证正常执行后全局与用户槽位都会释放。
func TestWorkerReleasesSlotsAfterExecution(t *testing.T) {
	ctrl := gomock.NewController(t)
	userID, runID := uuid.New(), uuid.New()
	global := &fakeLimiter{allowed: true}
	user := &fakeLimiter{allowed: true}
	worker, jobs, runs := newWorkerWithLimiters(t, ctrl, global, user)
	job := jobFixture(userID, runID)
	jobs.EXPECT().Claim(gomock.Any(), "worker-1", "agent.run", gomock.Any(), gomock.Any()).Return(job, true, nil)
	runs.EXPECT().GetRun(gomock.Any(), userID, runID).Return(biz.Run{ID: runID, UserID: userID, Status: biz.RunCompleted}, nil)
	jobs.EXPECT().Complete(gomock.Any(), job.ID, "worker-1", job.FencingToken).Return(nil)
	if processed, err := worker.ProcessOne(context.Background()); err != nil || !processed {
		t.Fatalf("ProcessOne() = %t, %v", processed, err)
	}
	global.mu.Lock()
	globalReleased := global.released
	global.mu.Unlock()
	user.mu.Lock()
	userReleased := user.released
	user.mu.Unlock()
	if globalReleased != 1 || userReleased != 1 {
		t.Fatalf("released global=%d user=%d", globalReleased, userReleased)
	}
}

// TestWorkerFailsOpenWhenLimiterUnavailable 验证 Redis 故障时放行执行而不是阻断。
func TestWorkerFailsOpenWhenLimiterUnavailable(t *testing.T) {
	ctrl := gomock.NewController(t)
	userID, runID := uuid.New(), uuid.New()
	worker, jobs, runs := newWorkerWithLimiters(t, ctrl, &fakeLimiter{err: errors.New("redis down")}, &fakeLimiter{err: errors.New("redis down")})
	job := jobFixture(userID, runID)
	jobs.EXPECT().Claim(gomock.Any(), "worker-1", "agent.run", gomock.Any(), gomock.Any()).Return(job, true, nil)
	runs.EXPECT().GetRun(gomock.Any(), userID, runID).Return(biz.Run{ID: runID, UserID: userID, Status: biz.RunFailed}, nil)
	jobs.EXPECT().Complete(gomock.Any(), job.ID, "worker-1", job.FencingToken).Return(nil)
	if processed, err := worker.ProcessOne(context.Background()); err != nil || !processed {
		t.Fatalf("ProcessOne() = %t, %v", processed, err)
	}
}
