package biz_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/service/mocks"
	"github.com/Mark-Panda/harness-agent/pkg/agent/model"
	"github.com/google/uuid"
	"go.uber.org/mock/gomock"
)

// newWorkerWithProviders 构造可注入供应商错误的 Worker。
func newWorkerWithProviders(t *testing.T, ctrl *gomock.Controller, cancelPoll time.Duration, providers biz.ProviderCredentialRepository) (*biz.ExecutionWorker, *mocks.MockJobQueue, *mocks.MockRunRepository, *mocks.MockSessionRepository, *mocks.MockModelCallRepository) {
	t.Helper()
	jobs := mocks.NewMockJobQueue(ctrl)
	runs := mocks.NewMockRunRepository(ctrl)
	sessions := mocks.NewMockSessionRepository(ctrl)
	attempts := mocks.NewMockModelCallRepository(ctrl)
	steps := mocks.NewMockExecutionStepRepository(ctrl)
	steps.EXPECT().StartStep(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, step biz.ExecutionStep) (biz.ExecutionStep, error) { return step, nil },
	).AnyTimes()
	steps.EXPECT().SavePayload(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, payload biz.ExecutionPayload) (biz.ExecutionPayload, error) {
			return payload, nil
		},
	).AnyTimes()
	steps.EXPECT().FinishStep(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(biz.ExecutionStep{}, nil).AnyTimes()
	worker, err := biz.NewExecutionWorker(biz.ExecutionWorkerDependencies{
		Jobs: jobs, Runs: runs, Sessions: sessions, Providers: providers,
		Opener: fakeOpener{key: "test-key"}, Attempts: attempts, Steps: steps,
	}, biz.WorkerConfig{
		WorkerID: "worker-1", Concurrency: 1, PollInterval: time.Millisecond,
		Lease: 60 * time.Second, Heartbeat: 20 * time.Second, CancelPoll: cancelPoll,
		Routing: model.RouterConfig{
			MinContextTokens: 1_000_000, MaxAttemptsPerModel: 2, MaxAttemptsPerRun: 6,
			MaxSwitchesPerRun: 2, RequestTimeout: 5 * time.Second,
			BackoffBase: time.Millisecond, MaxBackoff: time.Millisecond, MaxRetryAfter: time.Second,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return worker, jobs, runs, sessions, attempts
}

// TestWorkerSkipsTerminalAndInvalidJobs 验证终态 Run 与非法载荷不会触发执行。
func TestWorkerSkipsTerminalAndInvalidJobs(t *testing.T) {
	ctrl := gomock.NewController(t)
	userID, runID := uuid.New(), uuid.New()
	worker, jobs, runs, _, _ := newWorker(t, ctrl, time.Hour, uuid.NewString(), "https://models.example.com")

	terminal := jobFixture(userID, runID)
	jobs.EXPECT().Claim(gomock.Any(), "worker-1", "agent.run", gomock.Any(), gomock.Any()).Return(terminal, true, nil)
	runs.EXPECT().GetRun(gomock.Any(), userID, runID).Return(biz.Run{ID: runID, UserID: userID, Status: biz.RunCompleted}, nil)
	jobs.EXPECT().Complete(gomock.Any(), terminal.ID, "worker-1", terminal.FencingToken).Return(nil)
	if processed, err := worker.ProcessOne(context.Background()); err != nil || !processed {
		t.Fatalf("terminal ProcessOne() = %t, %v", processed, err)
	}

	invalid := terminal
	invalid.ID = uuid.New()
	invalid.Payload = map[string]any{"run_id": "not-a-uuid"}
	jobs.EXPECT().Claim(gomock.Any(), "worker-1", "agent.run", gomock.Any(), gomock.Any()).Return(invalid, true, nil)
	jobs.EXPECT().Complete(gomock.Any(), invalid.ID, "worker-1", invalid.FencingToken).Return(nil)
	if processed, err := worker.ProcessOne(context.Background()); err != nil || !processed {
		t.Fatalf("invalid ProcessOne() = %t, %v", processed, err)
	}

	reconciling := terminal
	reconciling.ID = uuid.New()
	jobs.EXPECT().Claim(gomock.Any(), "worker-1", "agent.run", gomock.Any(), gomock.Any()).Return(reconciling, true, nil)
	runs.EXPECT().GetRun(gomock.Any(), userID, runID).Return(biz.Run{ID: runID, UserID: userID, Status: biz.RunReconciliationRequired}, nil)
	jobs.EXPECT().Complete(gomock.Any(), reconciling.ID, "worker-1", reconciling.FencingToken).Return(nil)
	if processed, err := worker.ProcessOne(context.Background()); err != nil || !processed {
		t.Fatalf("reconciliation ProcessOne() = %t, %v", processed, err)
	}
}

// TestWorkerDefersWaitingApprovalAndLeaseConflict 验证审批等待与租约冲突都会退回队列等待，不消耗重试预算。
func TestWorkerDefersWaitingApprovalAndLeaseConflict(t *testing.T) {
	ctrl := gomock.NewController(t)
	userID, runID := uuid.New(), uuid.New()
	worker, jobs, runs, _, _ := newWorker(t, ctrl, time.Hour, uuid.NewString(), "https://models.example.com")

	waiting := jobFixture(userID, runID)
	jobs.EXPECT().Claim(gomock.Any(), "worker-1", "agent.run", gomock.Any(), gomock.Any()).Return(waiting, true, nil)
	runs.EXPECT().GetRun(gomock.Any(), userID, runID).Return(biz.Run{ID: runID, UserID: userID, Status: biz.RunWaitingApproval}, nil)
	jobs.EXPECT().Release(gomock.Any(), waiting.ID, "worker-1", waiting.FencingToken, gomock.Any(), gomock.Any()).Return(nil)
	if processed, err := worker.ProcessOne(context.Background()); err != nil || !processed {
		t.Fatalf("approval ProcessOne() = %t, %v", processed, err)
	}

	conflict := waiting
	conflict.ID = uuid.New()
	jobs.EXPECT().Claim(gomock.Any(), "worker-1", "agent.run", gomock.Any(), gomock.Any()).Return(conflict, true, nil)
	runs.EXPECT().GetRun(gomock.Any(), userID, runID).Return(biz.Run{ID: runID, UserID: userID, Status: biz.RunQueued}, nil)
	runs.EXPECT().ClaimRun(gomock.Any(), userID, runID, "worker-1", gomock.Any(), gomock.Any()).Return(biz.Run{}, biz.RunFence{}, biz.ErrResourceConflict)
	jobs.EXPECT().Release(gomock.Any(), conflict.ID, "worker-1", conflict.FencingToken, gomock.Any(), gomock.Any()).Return(nil)
	if processed, err := worker.ProcessOne(context.Background()); err != nil || !processed {
		t.Fatalf("conflict ProcessOne() = %t, %v", processed, err)
	}
}

// TestWorkerRequeuesRunsBlockedBySessionSerialization 验证同会话前序 Run 执行时后续任务退回队列而不是记失败。
func TestWorkerRequeuesRunsBlockedBySessionSerialization(t *testing.T) {
	ctrl := gomock.NewController(t)
	userID, runID := uuid.New(), uuid.New()
	worker, jobs, runs, _, _ := newWorker(t, ctrl, time.Hour, uuid.NewString(), "https://models.example.com")

	job := jobFixture(userID, runID)
	jobs.EXPECT().Claim(gomock.Any(), "worker-1", "agent.run", gomock.Any(), gomock.Any()).Return(job, true, nil)
	runs.EXPECT().GetRun(gomock.Any(), userID, runID).Return(biz.Run{ID: runID, UserID: userID, Status: biz.RunQueued}, nil)
	runs.EXPECT().ClaimRun(gomock.Any(), userID, runID, "worker-1", gomock.Any(), gomock.Any()).Return(biz.Run{}, biz.RunFence{}, biz.ErrSessionRunActive)
	jobs.EXPECT().Release(gomock.Any(), job.ID, "worker-1", job.FencingToken, gomock.Any(), gomock.Any()).Return(nil)
	if processed, err := worker.ProcessOne(context.Background()); err != nil || !processed {
		t.Fatalf("session serialization ProcessOne() = %t, %v", processed, err)
	}
}

// TestWorkerFailsRunPastDeadline 验证过期 Run 在认领后立即进入失败终态。
func TestWorkerFailsRunPastDeadline(t *testing.T) {
	ctrl := gomock.NewController(t)
	userID, runID, sessionID := uuid.New(), uuid.New(), uuid.New()
	worker, jobs, runs, _, _ := newWorker(t, ctrl, time.Hour, uuid.NewString(), "https://models.example.com")
	job := jobFixture(userID, runID)
	jobs.EXPECT().Claim(gomock.Any(), "worker-1", "agent.run", gomock.Any(), gomock.Any()).Return(job, true, nil)
	expired := time.Now().Add(-time.Minute)
	run := biz.Run{ID: runID, UserID: userID, SessionID: sessionID, Status: biz.RunQueued, Mode: biz.AgentModeReact, DeadlineAt: &expired}
	running := run
	running.Status = biz.RunRunning
	fence := biz.RunFence{Owner: "worker-1", Token: 9}
	runs.EXPECT().GetRun(gomock.Any(), userID, runID).Return(run, nil)
	runs.EXPECT().ClaimRun(gomock.Any(), userID, runID, "worker-1", gomock.Any(), gomock.Any()).Return(running, fence, nil)
	runs.EXPECT().FinishRunWithEvent(gomock.Any(), userID, runID, biz.RunRunning, biz.RunFailed, gomock.Any(), gomock.Any(), fence, gomock.Any()).DoAndReturn(
		func(_ context.Context, _ uuid.UUID, _ uuid.UUID, _, _ biz.RunStatus, fields map[string]any, _ biz.RunEvent, _ biz.RunFence, _ time.Time) (biz.Run, error) {
			if fields["error_code"] != "run_deadline_exceeded" {
				t.Fatalf("unexpected failure code: %#v", fields)
			}
			return biz.Run{ID: runID, Status: biz.RunFailed}, nil
		},
	)
	jobs.EXPECT().Complete(gomock.Any(), job.ID, "worker-1", job.FencingToken).Return(nil)
	if processed, err := worker.ProcessOne(context.Background()); err != nil || !processed {
		t.Fatalf("ProcessOne() = %t, %v", processed, err)
	}
}

// TestWorkerRetriesInfrastructureFailure 验证持久化故障在预算内退避重试而不是丢弃 Run。
func TestWorkerRetriesInfrastructureFailure(t *testing.T) {
	ctrl := gomock.NewController(t)
	userID, runID, sessionID := uuid.New(), uuid.New(), uuid.New()
	worker, jobs, runs, sessions, _ := newWorker(t, ctrl, time.Hour, uuid.NewString(), "https://models.example.com")
	job := jobFixture(userID, runID)
	jobs.EXPECT().Claim(gomock.Any(), "worker-1", "agent.run", gomock.Any(), gomock.Any()).Return(job, true, nil)
	run := biz.Run{ID: runID, UserID: userID, SessionID: sessionID, Status: biz.RunQueued, Mode: biz.AgentModeReact}
	running := run
	running.Status = biz.RunRunning
	fence := biz.RunFence{Owner: "worker-1", Token: 9}
	runs.EXPECT().GetRun(gomock.Any(), userID, runID).Return(run, nil)
	runs.EXPECT().ClaimRun(gomock.Any(), userID, runID, "worker-1", gomock.Any(), gomock.Any()).Return(running, fence, nil)
	sessions.EXPECT().ListMessages(gomock.Any(), userID, sessionID, 50, "").Return(biz.Page[biz.Message]{}, errors.New("database unavailable"))
	jobs.EXPECT().Fail(gomock.Any(), job.ID, "worker-1", job.FencingToken, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
	if processed, err := worker.ProcessOne(context.Background()); err != nil || !processed {
		t.Fatalf("ProcessOne() = %t, %v", processed, err)
	}
}

// TestWorkerAbortsAfterFinalInfrastructureAttempt 验证最后一次基础设施失败会终止 Run，避免永久活动状态。
func TestWorkerAbortsAfterFinalInfrastructureAttempt(t *testing.T) {
	ctrl := gomock.NewController(t)
	userID, runID, sessionID := uuid.New(), uuid.New(), uuid.New()
	worker, jobs, runs, sessions, _ := newWorker(t, ctrl, time.Hour, uuid.NewString(), "https://models.example.com")
	job := jobFixture(userID, runID)
	job.Attempts = job.MaxAttempts
	jobs.EXPECT().Claim(gomock.Any(), "worker-1", "agent.run", gomock.Any(), gomock.Any()).Return(job, true, nil)
	run := biz.Run{ID: runID, UserID: userID, SessionID: sessionID, Status: biz.RunQueued, Mode: biz.AgentModeReact}
	running := run
	running.Status = biz.RunRunning
	fence := biz.RunFence{Owner: "worker-1", Token: 9}
	runs.EXPECT().GetRun(gomock.Any(), userID, runID).Return(run, nil)
	runs.EXPECT().ClaimRun(gomock.Any(), userID, runID, "worker-1", gomock.Any(), gomock.Any()).Return(running, fence, nil)
	sessions.EXPECT().ListMessages(gomock.Any(), userID, sessionID, 50, "").Return(biz.Page[biz.Message]{}, errors.New("database unavailable"))
	runs.EXPECT().FinishRunWithEvent(gomock.Any(), userID, runID, biz.RunRunning, biz.RunFailed, gomock.Any(), gomock.Any(), fence, gomock.Any()).DoAndReturn(
		func(_ context.Context, _ uuid.UUID, _ uuid.UUID, _, _ biz.RunStatus, fields map[string]any, _ biz.RunEvent, _ biz.RunFence, _ time.Time) (biz.Run, error) {
			if fields["error_code"] != "execution_aborted" {
				t.Fatalf("unexpected failure code: %#v", fields)
			}
			return biz.Run{ID: runID, Status: biz.RunFailed}, nil
		},
	)
	jobs.EXPECT().Complete(gomock.Any(), job.ID, "worker-1", job.FencingToken).Return(nil)
	if processed, err := worker.ProcessOne(context.Background()); err != nil || !processed {
		t.Fatalf("ProcessOne() = %t, %v", processed, err)
	}
}

// TestWorkerFailsWhenProviderUnavailable 验证凭证不可读时快速失败且不发起模型调用。
func TestWorkerFailsWhenProviderUnavailable(t *testing.T) {
	ctrl := gomock.NewController(t)
	userID, runID, sessionID := uuid.New(), uuid.New(), uuid.New()
	providerID, modelID := uuid.NewString(), uuid.NewString()
	worker, jobs, runs, sessions, _ := newWorkerWithProviders(t, ctrl, time.Hour, fakeProviderCredentials{err: errors.New("provider missing")})
	job := jobFixture(userID, runID)
	jobs.EXPECT().Claim(gomock.Any(), "worker-1", "agent.run", gomock.Any(), gomock.Any()).Return(job, true, nil)
	run := runFixture(userID, runID, sessionID, snapshotFixture("https://models.example.com", providerID, modelID))
	running := run
	running.Status = biz.RunRunning
	fence := biz.RunFence{Owner: "worker-1", Token: 9}
	runs.EXPECT().GetRun(gomock.Any(), userID, runID).Return(run, nil)
	runs.EXPECT().ClaimRun(gomock.Any(), userID, runID, "worker-1", gomock.Any(), gomock.Any()).Return(running, fence, nil)
	sessions.EXPECT().ListMessages(gomock.Any(), userID, sessionID, 50, "").Return(biz.Page[biz.Message]{
		Items: []biz.Message{{Role: biz.MessageUser, Content: "你好"}},
	}, nil)
	runs.EXPECT().FinishRunWithEvent(gomock.Any(), userID, runID, biz.RunRunning, biz.RunFailed, gomock.Any(), gomock.Any(), fence, gomock.Any()).DoAndReturn(
		func(_ context.Context, _ uuid.UUID, _ uuid.UUID, _, _ biz.RunStatus, fields map[string]any, _ biz.RunEvent, _ biz.RunFence, _ time.Time) (biz.Run, error) {
			if fields["error_code"] != "invalid_run_snapshot" {
				t.Fatalf("unexpected failure code: %#v", fields)
			}
			return biz.Run{ID: runID, Status: biz.RunFailed}, nil
		},
	)
	jobs.EXPECT().Complete(gomock.Any(), job.ID, "worker-1", job.FencingToken).Return(nil)
	if processed, err := worker.ProcessOne(context.Background()); err != nil || !processed {
		t.Fatalf("ProcessOne() = %t, %v", processed, err)
	}
}

// TestWorkerEmitsModelSwitchEvent 验证候选故障后追加 model.switched 事件并成功完成。
func TestWorkerEmitsModelSwitchEvent(t *testing.T) {
	ctrl := gomock.NewController(t)
	userID, runID, sessionID := uuid.New(), uuid.New(), uuid.New()
	providerID := uuid.NewString()
	primaryID, secondaryID := uuid.NewString(), uuid.NewString()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Model == "primary" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"req-2","choices":[{"message":{"content":"切换成功"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	t.Cleanup(server.Close)
	snapshot := snapshotFixture(server.URL, providerID, primaryID)
	snapshot["candidates"] = []any{
		map[string]any{
			"priority": 1, "model_id": primaryID, "provider_id": providerID,
			"provider_base_url": server.URL, "remote_model_name": "primary",
			"context_window_tokens": int64(1_000_000), "max_output_tokens": 4096,
			"capabilities":            map[string]any{"tools": true},
			"input_price_per_million": 1.0, "output_price_per_million": 1.0,
		},
		map[string]any{
			"priority": 2, "model_id": secondaryID, "provider_id": providerID,
			"provider_base_url": server.URL, "remote_model_name": "secondary",
			"context_window_tokens": int64(1_000_000), "max_output_tokens": 4096,
			"capabilities":            map[string]any{"tools": true},
			"input_price_per_million": 1.0, "output_price_per_million": 1.0,
		},
	}
	worker, jobs, runs, sessions, attempts := newWorker(t, ctrl, time.Hour, providerID, server.URL)
	job := jobFixture(userID, runID)
	jobs.EXPECT().Claim(gomock.Any(), "worker-1", "agent.run", gomock.Any(), gomock.Any()).Return(job, true, nil)
	run := runFixture(userID, runID, sessionID, snapshot)
	running := run
	running.Status = biz.RunRunning
	fence := biz.RunFence{Owner: "worker-1", Token: 9}
	runs.EXPECT().GetRun(gomock.Any(), userID, runID).Return(run, nil)
	runs.EXPECT().ClaimRun(gomock.Any(), userID, runID, "worker-1", gomock.Any(), gomock.Any()).Return(running, fence, nil)
	sessions.EXPECT().ListMessages(gomock.Any(), userID, sessionID, 50, "").Return(biz.Page[biz.Message]{
		Items: []biz.Message{{Role: biz.MessageUser, Content: "你好"}},
	}, nil)
	runs.EXPECT().SetRunAttempt(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), fence).Return(nil).AnyTimes()
	attempts.EXPECT().CreateModelCall(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, call biz.ModelCall) (biz.ModelCall, error) { return call, nil },
	).AnyTimes()
	attempts.EXPECT().FinishModelCall(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(biz.ModelCall{}, nil).AnyTimes()
	var mu sync.Mutex
	var eventTypes []string
	runs.EXPECT().AppendEventFenced(gomock.Any(), gomock.Any(), fence).DoAndReturn(
		func(_ context.Context, event biz.RunEvent, _ biz.RunFence) (biz.RunEvent, error) {
			mu.Lock()
			eventTypes = append(eventTypes, event.EventType)
			mu.Unlock()
			return event, nil
		},
	).AnyTimes()
	sessions.EXPECT().AppendMessageIdempotent(gomock.Any(), gomock.Any(), gomock.Any()).Return(biz.Message{}, nil)
	runs.EXPECT().FinishRunWithEvent(gomock.Any(), userID, runID, biz.RunRunning, biz.RunCompleted, gomock.Any(), gomock.Any(), fence, gomock.Any()).Return(
		biz.Run{ID: runID, Status: biz.RunCompleted}, nil,
	)
	jobs.EXPECT().Complete(gomock.Any(), job.ID, "worker-1", job.FencingToken).Return(nil)
	if processed, err := worker.ProcessOne(context.Background()); err != nil || !processed {
		t.Fatalf("ProcessOne() = %t, %v", processed, err)
	}
	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, eventType := range eventTypes {
		if eventType == "model.switched" {
			found = true
		}
	}
	if !found {
		t.Fatalf("model.switched event was not persisted: %v", eventTypes)
	}
}
