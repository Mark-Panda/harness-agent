package model

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
)

// fakeHealth 记录熔断调用次数，并可模拟 Redis 故障或指定模型不可用。
type fakeHealth struct {
	mu            sync.Mutex
	allowed       map[string]bool
	allowErr      error
	failures      map[string]int
	successes     map[string]int
	allowCalls    int
	failureRecord int
}

// newFakeHealth 构造默认全部可用的健康存储。
func newFakeHealth() *fakeHealth {
	return &fakeHealth{allowed: map[string]bool{}, failures: map[string]int{}, successes: map[string]int{}}
}

// Allow 返回预设可用性；allowErr 非空时模拟缓存故障。
func (f *fakeHealth) Allow(_ context.Context, providerID, modelID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.allowCalls++
	if f.allowErr != nil {
		return false, f.allowErr
	}
	allowed, exists := f.allowed[providerID+"/"+modelID]
	if !exists {
		// 未显式设置的模型默认健康，便于聚焦测试故障切换路径。
		return true, nil
	}
	return allowed, nil
}

// RecordSuccess 记录成功次数。
func (f *fakeHealth) RecordSuccess(_ context.Context, providerID, modelID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.successes[providerID+"/"+modelID]++
	return nil
}

// RecordFailure 记录失败次数。
func (f *fakeHealth) RecordFailure(_ context.Context, providerID, modelID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures[providerID+"/"+modelID]++
	f.failureRecord++
	return nil
}

// attemptRecorder 记录 Attempt 生命周期与切换事实，供断言路由行为。
type attemptRecorder struct {
	mu        sync.Mutex
	started   []AttemptRecord
	finished  []AttemptRecord
	switches  []SwitchRecord
	startedAt []time.Time
}

// AttemptStarted 记录调用前快照。
func (r *attemptRecorder) AttemptStarted(_ context.Context, record AttemptRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.started = append(r.started, record)
	r.startedAt = append(r.startedAt, time.Now())
	return nil
}

// AttemptFinished 记录调用结果。
func (r *attemptRecorder) AttemptFinished(_ context.Context, record AttemptRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finished = append(r.finished, record)
	return nil
}

// ModelSwitched 记录切换原因。
func (r *attemptRecorder) ModelSwitched(_ context.Context, record SwitchRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.switches = append(r.switches, record)
	return nil
}

// newChatServer 构造按调用序号返回响应的 OpenAI-compatible 测试服务。
func newChatServer(t *testing.T, handler func(call int, w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	calls := 0
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		current := calls
		mu.Unlock()
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		handler(current, w, r)
	}))
	t.Cleanup(server.Close)
	return server
}

// writeCompletion 写出合法的 Chat Completions 响应。
func writeCompletion(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"id":"req-1","choices":[{"message":{"content":"` + content + `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":50}}`))
}

// candidateFor 构造指向测试服务的候选。
func candidateFor(server *httptest.Server, providerID, modelID string, window int64) Candidate {
	return Candidate{
		Priority: 1,
		Ref: ModelRef{
			ProviderID: providerID, ModelID: modelID, RemoteModelName: modelID,
			ContextWindowTokens: window, MaxOutputTokens: 4096, Tools: true,
		},
		BaseURL: server.URL, APIKey: "test-key", InputPricePerMillion: 1, OutputPricePerMillion: 2,
	}
}

// testConfig 构造不实际等待的路由配置。
func testConfig() RouterConfig {
	return RouterConfig{
		MinContextTokens: 1_000_000, MaxAttemptsPerModel: 2, MaxAttemptsPerRun: 6, MaxSwitchesPerRun: 2,
		BackoffBase: time.Millisecond, MaxBackoff: time.Millisecond, MaxRetryAfter: 10 * time.Second,
		RequestTimeout: 5 * time.Second,
	}
}

// TestRouterPriorityStickiness 验证按优先级选择并在多次调用间保持模型粘性。
func TestRouterPriorityStickiness(t *testing.T) {
	server := newChatServer(t, func(_ int, w http.ResponseWriter, _ *http.Request) { writeCompletion(w, "hello") })
	candidates := []Candidate{
		candidateFor(server, "p1", "m1", 1_000_000),
		candidateFor(server, "p2", "m2", 1_000_000),
	}
	health := newFakeHealth()
	recorder := &attemptRecorder{}
	router, err := NewRouter(candidates, health, testConfig(), recorder, WithSleep(func(context.Context, time.Duration) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		result, err := router.Complete(context.Background(), []react.Message{{Role: react.RoleUser, Content: "hi"}}, nil)
		if err != nil || result.Content != "hello" {
			t.Fatalf("Complete() = %#v, %v", result, err)
		}
	}
	if len(recorder.started) != 2 || recorder.started[0].ModelID != "m1" || recorder.started[1].ModelID != "m1" {
		t.Fatalf("model stickiness violated: %#v", recorder.started)
	}
	if recorder.started[0].AttemptNo != 1 || recorder.started[1].AttemptNo != 2 {
		t.Fatalf("attempt numbers are not monotonic: %#v", recorder.started)
	}
	if len(recorder.started[0].RequestHash) != 32 {
		t.Fatalf("request hash must be a SHA-256 digest")
	}
}

// TestRouterRetryThenSwitch 验证可重试 5xx 先同模型退避重试，耗尽后切换下一候选。
func TestRouterRetryThenSwitch(t *testing.T) {
	first := newChatServer(t, func(_ int, w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	second := newChatServer(t, func(_ int, w http.ResponseWriter, _ *http.Request) { writeCompletion(w, "recovered") })
	candidates := []Candidate{
		candidateFor(first, "p1", "m1", 1_000_000),
		candidateFor(second, "p2", "m2", 1_000_000),
	}
	health := newFakeHealth()
	recorder := &attemptRecorder{}
	router, err := NewRouter(candidates, health, testConfig(), recorder, WithSleep(func(context.Context, time.Duration) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	result, err := router.Complete(context.Background(), []react.Message{{Role: react.RoleUser, Content: "hi"}}, nil)
	if err != nil || result.Content != "recovered" {
		t.Fatalf("Complete() = %#v, %v", result, err)
	}
	if len(recorder.finished) != 3 {
		t.Fatalf("expected two failures and one success, got %d", len(recorder.finished))
	}
	if len(recorder.switches) != 1 || recorder.switches[0].FromModelID != "m1" || recorder.switches[0].ToModelID != "m2" || recorder.switches[0].Reason != "retryable_failure" {
		t.Fatalf("unexpected switch record: %#v", recorder.switches)
	}
	if health.failures["p1/m1"] != 2 || health.successes["p2/m2"] != 1 {
		t.Fatalf("health accounting mismatch: failures=%v successes=%v", health.failures, health.successes)
	}
}

// TestRouterRateLimitRetryAfterSwitchesImmediately 验证超出预算的 Retry-After 直接切换且不等待。
func TestRouterRateLimitRetryAfterSwitchesImmediately(t *testing.T) {
	first := newChatServer(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	second := newChatServer(t, func(_ int, w http.ResponseWriter, _ *http.Request) { writeCompletion(w, "ok") })
	candidates := []Candidate{
		candidateFor(first, "p1", "m1", 1_000_000),
		candidateFor(second, "p2", "m2", 1_000_000),
	}
	health := newFakeHealth()
	recorder := &attemptRecorder{}
	sleeps := 0
	router, err := NewRouter(candidates, health, testConfig(), recorder, WithSleep(func(context.Context, time.Duration) error {
		sleeps++
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	result, err := router.Complete(context.Background(), []react.Message{{Role: react.RoleUser, Content: "hi"}}, nil)
	if err != nil || result.Content != "ok" {
		t.Fatalf("Complete() = %#v, %v", result, err)
	}
	if sleeps != 0 {
		t.Fatalf("rate limit with excessive Retry-After must switch without sleeping")
	}
	if len(recorder.switches) != 1 || recorder.switches[0].Reason != "rate_limited" {
		t.Fatalf("unexpected switch: %#v", recorder.switches)
	}
}

// TestRouterSlowProviderTimesOutAndSwitches 验证模型慢调用在请求超时后切换健康候选。
func TestRouterSlowProviderTimesOutAndSwitches(t *testing.T) {
	slow := newChatServer(t, func(_ int, _ http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(300 * time.Millisecond):
		case <-r.Context().Done():
		}
	})
	healthy := newChatServer(t, func(_ int, w http.ResponseWriter, _ *http.Request) { writeCompletion(w, "ok") })
	config := testConfig()
	config.RequestTimeout = 30 * time.Millisecond
	config.MaxAttemptsPerModel = 1
	health := newFakeHealth()
	recorder := &attemptRecorder{}
	router, err := NewRouter([]Candidate{
		candidateFor(slow, "p1", "m1", 1_000_000),
		candidateFor(healthy, "p2", "m2", 1_000_000),
	}, health, config, recorder, WithSleep(func(context.Context, time.Duration) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	result, err := router.Complete(context.Background(), []react.Message{{Role: react.RoleUser, Content: "hi"}}, nil)
	if err != nil || result.Content != "ok" {
		t.Fatalf("Complete() = %#v, %v", result, err)
	}
	// 必须按请求超时而不是等待上游自然返回，避免慢调用占满 Worker 并发。
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("slow provider was not abandoned in time: %s", elapsed)
	}
	if len(recorder.finished) != 2 || recorder.finished[0].ErrorClass != "timeout" {
		t.Fatalf("timeout attempt not recorded: %#v", recorder.finished)
	}
	if len(recorder.switches) != 1 || recorder.switches[0].Reason != "retryable_failure" {
		t.Fatalf("unexpected switch: %#v", recorder.switches)
	}
	if health.failures["p1/m1"] == 0 {
		t.Fatal("slow provider must count as a health failure for the circuit breaker")
	}
}

// TestRouterPermanentFailureDoesNotSwitch 验证 400 等永久错误禁止通过切换绕过校验。
func TestRouterPermanentFailureDoesNotSwitch(t *testing.T) {
	server := newChatServer(t, func(_ int, w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadRequest) })
	second := newChatServer(t, func(_ int, w http.ResponseWriter, _ *http.Request) { writeCompletion(w, "should-not-run") })
	candidates := []Candidate{
		candidateFor(server, "p1", "m1", 1_000_000),
		candidateFor(second, "p2", "m2", 1_000_000),
	}
	health := newFakeHealth()
	recorder := &attemptRecorder{}
	router, err := NewRouter(candidates, health, testConfig(), recorder, WithSleep(func(context.Context, time.Duration) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := router.Complete(context.Background(), []react.Message{{Role: react.RoleUser, Content: "hi"}}, nil); !errors.Is(err, ErrPermanent) {
		t.Fatalf("permanent error = %v", err)
	}
	if len(recorder.finished) != 1 || len(recorder.switches) != 0 {
		t.Fatalf("permanent failure must not retry or switch: %#v", recorder)
	}
	if health.failureRecord != 0 {
		t.Fatalf("permanent request errors must not trip the circuit breaker")
	}
}

// TestRouterAuthenticationSwitches 验证 401 立即切换另一个合规供应商。
func TestRouterAuthenticationSwitches(t *testing.T) {
	first := newChatServer(t, func(_ int, w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	second := newChatServer(t, func(_ int, w http.ResponseWriter, _ *http.Request) { writeCompletion(w, "ok") })
	recorder := &attemptRecorder{}
	router, err := NewRouter([]Candidate{
		candidateFor(first, "p1", "m1", 1_000_000),
		candidateFor(second, "p2", "m2", 1_000_000),
	}, newFakeHealth(), testConfig(), recorder, WithSleep(func(context.Context, time.Duration) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := router.Complete(context.Background(), []react.Message{{Role: react.RoleUser, Content: "hi"}}, nil); err != nil {
		t.Fatal(err)
	}
	if len(recorder.switches) != 1 || recorder.switches[0].Reason != "auth_failed" {
		t.Fatalf("unexpected switch: %#v", recorder.switches)
	}
}

// TestRouterContextFitSkipsSmallWindow 验证输入放不下时跳过小窗口候选，选择更大窗口模型。
func TestRouterContextFitSkipsSmallWindow(t *testing.T) {
	server := newChatServer(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		writeCompletion(w, "fit")
	})
	small := candidateFor(server, "p1", "m-small", 1_000_000)
	large := candidateFor(server, "p2", "m-large", 2_000_000)
	recorder := &attemptRecorder{}
	router, err := NewRouter([]Candidate{small, large}, newFakeHealth(), testConfig(), recorder, WithSleep(func(context.Context, time.Duration) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	// 约 1M 输入 Token，叠加输出预算后无法放入 1M 窗口。
	prompt := strings.Repeat("a", 4_000_000)
	result, err := router.Complete(context.Background(), []react.Message{{Role: react.RoleUser, Content: prompt}}, nil)
	if err != nil || result.Content != "fit" {
		t.Fatalf("Complete() = %#v, %v", result, err)
	}
	used := recorder.started[0].ModelID
	if used != "m-large" {
		t.Fatalf("expected larger window candidate, got %s", used)
	}
}

// TestRouterAttemptLimit 验证单 Run 总尝试预算耗尽后停止重试。
func TestRouterAttemptLimit(t *testing.T) {
	server := newChatServer(t, func(_ int, w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	config := testConfig()
	config.MaxAttemptsPerRun = 2
	router, err := NewRouter([]Candidate{candidateFor(server, "p1", "m1", 1_000_000)}, newFakeHealth(), config, &attemptRecorder{}, WithSleep(func(context.Context, time.Duration) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	_, err = router.Complete(context.Background(), []react.Message{{Role: react.RoleUser, Content: "hi"}}, nil)
	if !errors.Is(err, ErrAttemptLimit) {
		t.Fatalf("attempt limit error = %v", err)
	}
}

// TestRouterRejectsNon1MAndNoCandidates 验证 1M+ 是硬准入，无候选时快速失败。
func TestRouterRejectsNon1MAndNoCandidates(t *testing.T) {
	server := newChatServer(t, func(_ int, w http.ResponseWriter, _ *http.Request) { writeCompletion(w, "wrong") })
	router, err := NewRouter([]Candidate{candidateFor(server, "p1", "small", 100_000)}, newFakeHealth(), testConfig(), &attemptRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = router.Complete(context.Background(), []react.Message{{Role: react.RoleUser, Content: "hi"}}, nil)
	if !errors.Is(err, ErrNoCandidates) {
		t.Fatalf("no candidate error = %v", err)
	}
}

// TestRouterHealthFailureFallsBackToStaticOrder 验证健康缓存不可用时仍按静态顺序调用。
func TestRouterHealthFailureFallsBackToStaticOrder(t *testing.T) {
	server := newChatServer(t, func(_ int, w http.ResponseWriter, _ *http.Request) { writeCompletion(w, "ok") })
	health := newFakeHealth()
	health.allowErr = errors.New("redis unavailable")
	recorder := &attemptRecorder{}
	router, err := NewRouter([]Candidate{candidateFor(server, "p1", "m1", 1_000_000)}, health, testConfig(), recorder)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := router.Complete(context.Background(), []react.Message{{Role: react.RoleUser, Content: "hi"}}, nil); err != nil {
		t.Fatal(err)
	}
	if len(recorder.started) != 1 {
		t.Fatalf("static fallback did not call the candidate")
	}
}
