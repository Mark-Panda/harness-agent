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

// fakeProviderCredentials 提供固定供应商与加密凭证，避免测试依赖真实密钥环。
type fakeProviderCredentials struct {
	provider   biz.Provider
	credential biz.ProviderCredential
	err        error
}

// GetProviderCredential 实现凭证读取端口。
func (f fakeProviderCredentials) GetProviderCredential(_ context.Context, _ uuid.UUID) (biz.Provider, biz.ProviderCredential, error) {
	return f.provider, f.credential, f.err
}

// fakeOpener 返回固定明文密钥，模拟密钥环解密。
type fakeOpener struct {
	key string
	err error
}

// Open 实现凭证解密端口。
func (f fakeOpener) Open(_ string, _ []byte) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	return []byte(f.key), nil
}

// jobFixture 构造可被 Worker 认领的 Run 任务。
func jobFixture(userID, runID uuid.UUID) biz.Job {
	return biz.Job{
		ID: uuid.New(), UserID: &userID, Type: "agent.run",
		Payload:  map[string]any{"run_id": runID.String(), "user_id": userID.String()},
		Attempts: 1, MaxAttempts: 8, FencingToken: 7, LeaseOwner: "worker-1",
	}
}

// snapshotFixture 构造包含单个候选模型的 Run 策略快照。
func snapshotFixture(baseURL, providerID, modelID string) map[string]any {
	return map[string]any{
		"policy_id": uuid.NewString(), "policy_version": 1, "min_context_tokens": int64(1_000_000),
		"candidates": []any{map[string]any{
			"priority": 1, "model_id": modelID, "provider_id": providerID,
			"provider_base_url": baseURL, "remote_model_name": "generation",
			"context_window_tokens": int64(1_000_000), "max_output_tokens": 4096,
			"capabilities":            map[string]any{"tools": true, "streaming": false, "structured_output": true},
			"input_price_per_million": 10.0, "output_price_per_million": 20.0,
		}},
	}
}

// runFixture 构造排队中的 Run 聚合。
func runFixture(userID, runID, sessionID uuid.UUID, snapshot map[string]any) biz.Run {
	return biz.Run{
		ID: runID, UserID: userID, SessionID: sessionID, Mode: biz.AgentModeReact, Status: biz.RunQueued,
		ConfigSnapshot:      map[string]any{"agent": map[string]any{"name": "测试助手", "description": "回答简洁"}},
		ModelPolicySnapshot: snapshot,
	}
}

// completionServer 返回固定 Chat Completions 响应。
func completionServer(t *testing.T, content string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body, _ := json.Marshal(map[string]any{
			"id": "req-1",
			"choices": []any{map[string]any{
				"message": map[string]any{"content": content}, "finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 20},
		})
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	return server
}

// newWorker 构造 Worker 与测试依赖；调用方负责设置 gomock 期望。
func newWorker(t *testing.T, ctrl *gomock.Controller, cancelPoll time.Duration, providerID, baseURL string) (*biz.ExecutionWorker, *mocks.MockJobQueue, *mocks.MockRunRepository, *mocks.MockSessionRepository, *mocks.MockModelCallRepository) {
	t.Helper()
	jobs := mocks.NewMockJobQueue(ctrl)
	runs := mocks.NewMockRunRepository(ctrl)
	sessions := mocks.NewMockSessionRepository(ctrl)
	attempts := mocks.NewMockModelCallRepository(ctrl)
	steps := mocks.NewMockExecutionStepRepository(ctrl)
	stepSeq := int64(0)
	steps.EXPECT().StartStep(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, step biz.ExecutionStep) (biz.ExecutionStep, error) {
			stepSeq++
			step.StepSeq = stepSeq
			return step, nil
		},
	).AnyTimes()
	steps.EXPECT().SavePayload(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, payload biz.ExecutionPayload) (biz.ExecutionPayload, error) {
			return payload, nil
		},
	).AnyTimes()
	steps.EXPECT().FinishStep(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(biz.ExecutionStep{}, nil).AnyTimes()
	worker, err := biz.NewExecutionWorker(biz.ExecutionWorkerDependencies{
		Jobs: jobs, Runs: runs, Sessions: sessions,
		Providers: fakeProviderCredentials{
			provider:   biz.Provider{ID: uuid.MustParse(providerID), Status: biz.ProviderEnabled, BaseURL: baseURL},
			credential: biz.ProviderCredential{KeyVersion: "v1", Encrypted: []byte("ciphertext")},
		},
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

// TestExecutionWorkerRunsReActSuccessfully 验证认领、ReAct 执行、事件/消息落库与终态提交闭环。
func TestExecutionWorkerRunsReActSuccessfully(t *testing.T) {
	ctrl := gomock.NewController(t)
	userID, runID, sessionID := uuid.New(), uuid.New(), uuid.New()
	providerID, modelID := uuid.NewString(), uuid.NewString()
	server := completionServer(t, "最终回答")
	worker, jobs, runs, sessions, attempts := newWorker(t, ctrl, time.Hour, providerID, server.URL)

	job := jobFixture(userID, runID)
	jobs.EXPECT().Claim(gomock.Any(), "worker-1", "agent.run", gomock.Any(), gomock.Any()).Return(job, true, nil)
	run := runFixture(userID, runID, sessionID, snapshotFixture(server.URL, providerID, modelID))
	running := run
	running.Status = biz.RunRunning
	fence := biz.RunFence{Owner: "worker-1", Token: 9}
	runs.EXPECT().GetRun(gomock.Any(), userID, runID).Return(run, nil)
	runs.EXPECT().ClaimRun(gomock.Any(), userID, runID, "worker-1", gomock.Any(), gomock.Any()).Return(running, fence, nil)
	sessions.EXPECT().ListMessages(gomock.Any(), userID, sessionID, 50, "").Return(biz.Page[biz.Message]{
		Items: []biz.Message{{Role: biz.MessageUser, Content: "你好"}},
	}, nil)
	runs.EXPECT().SetRunAttempt(gomock.Any(), userID, runID, 1, fence).Return(nil)
	attempts.EXPECT().CreateModelCall(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, call biz.ModelCall) (biz.ModelCall, error) { return call, nil },
	)
	attempts.EXPECT().FinishModelCall(gomock.Any(), userID, gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _ uuid.UUID, _ uuid.UUID, update biz.ModelCallUpdate) (biz.ModelCall, error) {
			if update.Status != biz.ModelCallSucceeded || update.InputTokens != 100 || update.OutputTokens != 20 {
				t.Fatalf("unexpected model call update: %#v", update)
			}
			return biz.ModelCall{}, nil
		},
	)
	runs.EXPECT().AppendEventFenced(gomock.Any(), gomock.Any(), fence).Return(biz.RunEvent{}, nil).AnyTimes()
	sessions.EXPECT().AppendMessageIdempotent(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, message biz.Message, _ biz.OutboxEvent) (biz.Message, error) {
			if message.Role != biz.MessageAssistant || message.Content != "最终回答" {
				t.Fatalf("unexpected assistant message: %#v", message)
			}
			return message, nil
		},
	)
	runs.EXPECT().FinishRunWithEvent(gomock.Any(), userID, runID, biz.RunRunning, biz.RunCompleted, gomock.Any(), gomock.Any(), fence, gomock.Any()).DoAndReturn(
		func(_ context.Context, _ uuid.UUID, _ uuid.UUID, _, _ biz.RunStatus, _ map[string]any, event biz.RunEvent, _ biz.RunFence, _ time.Time) (biz.Run, error) {
			if event.EventType != "run.completed" || event.Payload["message_id"] == "" {
				t.Fatalf("completion event is not atomic: %#v", event)
			}
			return biz.Run{ID: runID, Status: biz.RunCompleted}, nil
		},
	)
	jobs.EXPECT().Complete(gomock.Any(), job.ID, "worker-1", job.FencingToken).Return(nil)

	processed, err := worker.ProcessOne(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessOne() = %t, %v", processed, err)
	}
}

// TestExecutionWorkerFailsWhenCandidatesExhausted 验证候选耗尽后进入明确失败终态。
func TestExecutionWorkerFailsWhenCandidatesExhausted(t *testing.T) {
	ctrl := gomock.NewController(t)
	userID, runID, sessionID := uuid.New(), uuid.New(), uuid.New()
	providerID, modelID := uuid.NewString(), uuid.NewString()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(server.Close)
	worker, jobs, runs, sessions, attempts := newWorker(t, ctrl, time.Hour, providerID, server.URL)

	job := jobFixture(userID, runID)
	jobs.EXPECT().Claim(gomock.Any(), "worker-1", "agent.run", gomock.Any(), gomock.Any()).Return(job, true, nil)
	run := runFixture(userID, runID, sessionID, snapshotFixture(server.URL, providerID, modelID))
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
	runs.EXPECT().AppendEventFenced(gomock.Any(), gomock.Any(), fence).Return(biz.RunEvent{}, nil).AnyTimes()
	runs.EXPECT().FinishRunWithEvent(gomock.Any(), userID, runID, biz.RunRunning, biz.RunFailed, gomock.Any(), gomock.Any(), fence, gomock.Any()).DoAndReturn(
		func(_ context.Context, _ uuid.UUID, _ uuid.UUID, _, _ biz.RunStatus, fields map[string]any, _ biz.RunEvent, _ biz.RunFence, _ time.Time) (biz.Run, error) {
			if fields["error_code"] != "no_model_candidates" {
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

// TestExecutionWorkerFailsWithoutUserMessage 验证空会话快速失败并给出可诊断错误码。
func TestExecutionWorkerFailsWithoutUserMessage(t *testing.T) {
	ctrl := gomock.NewController(t)
	userID, runID, sessionID := uuid.New(), uuid.New(), uuid.New()
	providerID, modelID := uuid.NewString(), uuid.NewString()
	worker, jobs, runs, sessions, _ := newWorker(t, ctrl, time.Hour, providerID, "https://models.example.com")
	job := jobFixture(userID, runID)
	jobs.EXPECT().Claim(gomock.Any(), "worker-1", "agent.run", gomock.Any(), gomock.Any()).Return(job, true, nil)
	run := runFixture(userID, runID, sessionID, snapshotFixture("https://models.example.com", providerID, modelID))
	running := run
	running.Status = biz.RunRunning
	fence := biz.RunFence{Owner: "worker-1", Token: 9}
	runs.EXPECT().GetRun(gomock.Any(), userID, runID).Return(run, nil)
	runs.EXPECT().ClaimRun(gomock.Any(), userID, runID, "worker-1", gomock.Any(), gomock.Any()).Return(running, fence, nil)
	sessions.EXPECT().ListMessages(gomock.Any(), userID, sessionID, 50, "").Return(biz.Page[biz.Message]{}, nil)
	runs.EXPECT().FinishRunWithEvent(gomock.Any(), userID, runID, biz.RunRunning, biz.RunFailed, gomock.Any(), gomock.Any(), fence, gomock.Any()).DoAndReturn(
		func(_ context.Context, _ uuid.UUID, _ uuid.UUID, _, _ biz.RunStatus, fields map[string]any, _ biz.RunEvent, _ biz.RunFence, _ time.Time) (biz.Run, error) {
			if fields["error_code"] != "no_user_message" {
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

// TestExecutionWorkerStopsWhenRunCancelled 验证用户取消后中止模型调用且不写入终态。
func TestExecutionWorkerStopsWhenRunCancelled(t *testing.T) {
	ctrl := gomock.NewController(t)
	userID, runID, sessionID := uuid.New(), uuid.New(), uuid.New()
	providerID, modelID := uuid.NewString(), uuid.NewString()
	var calls int
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(300 * time.Millisecond):
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"req-1","choices":[{"message":{"content":"late"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	t.Cleanup(server.Close)
	worker, jobs, runs, sessions, attempts := newWorker(t, ctrl, 20*time.Millisecond, providerID, server.URL)
	job := jobFixture(userID, runID)
	jobs.EXPECT().Claim(gomock.Any(), "worker-1", "agent.run", gomock.Any(), gomock.Any()).Return(job, true, nil)
	run := runFixture(userID, runID, sessionID, snapshotFixture(server.URL, providerID, modelID))
	running := run
	running.Status = biz.RunRunning
	fence := biz.RunFence{Owner: "worker-1", Token: 9}
	runs.EXPECT().GetRun(gomock.Any(), userID, runID).DoAndReturn(
		func(_ context.Context, _, _ uuid.UUID) (biz.Run, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			if calls == 1 {
				return run, nil
			}
			return biz.Run{ID: runID, Status: biz.RunCancelled}, nil
		},
	).AnyTimes()
	runs.EXPECT().ClaimRun(gomock.Any(), userID, runID, "worker-1", gomock.Any(), gomock.Any()).Return(running, fence, nil)
	sessions.EXPECT().ListMessages(gomock.Any(), userID, sessionID, 50, "").Return(biz.Page[biz.Message]{
		Items: []biz.Message{{Role: biz.MessageUser, Content: "你好"}},
	}, nil)
	runs.EXPECT().SetRunAttempt(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), fence).Return(nil).AnyTimes()
	attempts.EXPECT().CreateModelCall(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, call biz.ModelCall) (biz.ModelCall, error) { return call, nil },
	)
	attempts.EXPECT().FinishModelCall(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(biz.ModelCall{}, nil).AnyTimes()
	runs.EXPECT().AppendEventFenced(gomock.Any(), gomock.Any(), fence).Return(biz.RunEvent{}, nil).AnyTimes()
	jobs.EXPECT().Complete(gomock.Any(), job.ID, "worker-1", job.FencingToken).Return(nil)

	if processed, err := worker.ProcessOne(context.Background()); err != nil || !processed {
		t.Fatalf("ProcessOne() = %t, %v", processed, err)
	}
}

// TestExecutionWorkerRequiresDependencies 验证缺依赖与非法配置在构造期被拒绝。
func TestExecutionWorkerRequiresDependencies(t *testing.T) {
	if _, err := biz.NewExecutionWorker(biz.ExecutionWorkerDependencies{}, biz.WorkerConfig{}); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("missing dependencies error = %v", err)
	}
}
