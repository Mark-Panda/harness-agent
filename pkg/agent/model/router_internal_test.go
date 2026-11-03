package model

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
)

// recordingLimiter 记录供应商舱壁的获取、释放与故障。
type recordingLimiter struct {
	err      error
	acquires int
	releases int
	keys     []string
}

// Acquire 模拟获取供应商槽位。
func (l *recordingLimiter) Acquire(_ context.Context, key string) (func(), error) {
	if l.err != nil {
		return nil, l.err
	}
	l.acquires++
	l.keys = append(l.keys, key)
	return func() { l.releases++ }, nil
}

// TryAcquire 复用 Acquire，测试不涉及非阻塞路径。
func (l *recordingLimiter) TryAcquire(ctx context.Context, key string) (func(), bool, error) {
	release, err := l.Acquire(ctx, key)
	if err != nil {
		return nil, false, err
	}
	return release, true, nil
}

// errorObserver 模拟 Attempt 持久化失败，验证路由层会中止执行。
type errorObserver struct{}

// AttemptStarted 始终返回错误。
func (errorObserver) AttemptStarted(context.Context, AttemptRecord) error {
	return errors.New("persist failed")
}

// AttemptFinished 始终返回错误。
func (errorObserver) AttemptFinished(context.Context, AttemptRecord) error {
	return errors.New("persist failed")
}

// ModelSwitched 始终返回错误。
func (errorObserver) ModelSwitched(context.Context, SwitchRecord) error {
	return errors.New("persist failed")
}

// TestRouterConfigNormalizeAndBudget 验证零配置默认值与输出预算上限。
func TestRouterConfigNormalizeAndBudget(t *testing.T) {
	config := RouterConfig{}.normalize()
	if config.MinContextTokens != 1_000_000 || config.MaxAttemptsPerModel != 2 || config.MaxAttemptsPerRun != 6 {
		t.Fatalf("unexpected defaults: %#v", config)
	}
	if config.RequestTimeout <= 0 || config.BackoffBase <= 0 || config.MaxBackoff < config.BackoffBase || config.MaxRetryAfter <= 0 {
		t.Fatalf("durations were not normalized: %#v", config)
	}
	candidate := Candidate{Ref: ModelRef{MaxOutputTokens: 16_384}}
	router := &Router{config: config}
	if budget := router.outputBudget(candidate); budget != 16_384 {
		t.Fatalf("output budget = %d", budget)
	}
	router.config.MaxOutputTokens = 4096
	if budget := router.outputBudget(candidate); budget != 4096 {
		t.Fatalf("policy output budget was not applied: %d", budget)
	}
	if budget := router.outputBudget(Candidate{}); budget != 4096 {
		t.Fatalf("zero model output budget = %d", budget)
	}
}

// TestNewRouterValidationAndOptions 验证候选校验与测试选项生效。
func TestNewRouterValidationAndOptions(t *testing.T) {
	if _, err := NewRouter(nil, nil, RouterConfig{}, nil); !errors.Is(err, ErrNoCandidates) {
		t.Fatalf("empty candidates error = %v", err)
	}
	if _, err := NewRouter([]Candidate{{Ref: ModelRef{ModelID: "m"}}}, nil, RouterConfig{}, nil); err == nil {
		t.Fatal("candidate without connection fields was accepted")
	}
	server := newChatServer(t, func(_ int, w http.ResponseWriter, _ *http.Request) { writeCompletion(w, "ok") })
	router, err := NewRouter([]Candidate{candidateFor(server, "p", "m", 1_000_000)}, newFakeHealth(), RouterConfig{},
		&attemptRecorder{}, WithSleep(func(context.Context, time.Duration) error { return nil }),
		WithJitter(func(duration time.Duration) time.Duration { return duration }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if router.config.MaxAttemptsPerRun != 6 || router.jitter(time.Second) != time.Second {
		t.Fatalf("router options were not applied")
	}
}

// TestRouterDecideRules 验证错误分类到重试、切换与终止的确定性映射。
func TestRouterDecideRules(t *testing.T) {
	candidate := Candidate{Ref: ModelRef{ModelID: "m", ContextWindowTokens: 1_000_000, MaxOutputTokens: 4096}}
	config := testConfig()
	router := &Router{candidates: []Candidate{candidate}, config: config, current: 0, attempts: 1, excluded: map[int]string{}}
	decision := func(err error) (routeAction, time.Duration, string) {
		return router.decide(err, candidate, 0, false)
	}
	if action, _, _ := decision(fmt.Errorf("%w", ErrContextExceeded)); action != routeFail {
		t.Fatalf("context without larger candidate must fail, got %v", action)
	}
	router.candidates = append(router.candidates, Candidate{Ref: ModelRef{ModelID: "large", ContextWindowTokens: 2_000_000, MaxOutputTokens: 4096}})
	if action, _, reason := decision(fmt.Errorf("%w", ErrContextExceeded)); action != routeSwitch || reason != "context_window" {
		t.Fatalf("context with larger candidate = %v, %s", action, reason)
	}
	if action, wait, reason := decision(&ResponseError{StatusCode: 429, RetryAfter: 30 * time.Second, Class: ErrRateLimited}); action != routeSwitch || reason != "rate_limited" || wait != 0 {
		t.Fatalf("excessive Retry-After = %v, %v, %s", action, wait, reason)
	}
	if action, wait, reason := decision(&ResponseError{StatusCode: 429, RetryAfter: time.Second, Class: ErrRateLimited}); action != routeRetry || wait != time.Second {
		t.Fatalf("acceptable Retry-After = %v, %v, %s", action, wait, reason)
	}
	router.attempts = 2
	if action, _, reason := decision(&ResponseError{StatusCode: 429, Class: ErrRateLimited}); action != routeSwitch || reason != "rate_limited" {
		t.Fatalf("rate limited after max attempts = %v, %s", action, reason)
	}
	router.attempts = 1
	if action, wait, _ := decision(fmt.Errorf("%w: timeout", ErrRetryable)); action != routeRetry || wait <= 0 {
		t.Fatalf("retryable = %v, %v", action, wait)
	}
	router.attempts = 2
	if action, _, reason := decision(fmt.Errorf("%w: exhausted", ErrRetryable)); action != routeSwitch || reason != "retryable_failure" {
		t.Fatalf("retryable exhausted = %v, %s", action, reason)
	}
	if action, _, reason := decision(fmt.Errorf("%w", ErrAuthentication)); action != routeSwitch || reason != "auth_failed" {
		t.Fatalf("auth = %v, %s", action, reason)
	}
	if action, _, reason := decision(fmt.Errorf("%w", ErrCapability)); action != routeSwitch || reason != "capability_mismatch" {
		t.Fatalf("capability = %v, %s", action, reason)
	}
	if action, _, _ := decision(fmt.Errorf("%w: 400", ErrPermanent)); action != routeFail {
		t.Fatalf("permanent must fail, got %v", action)
	}
	if action, _, _ := decision(errors.New("unknown")); action != routeFail {
		t.Fatalf("unknown must fail, got %v", action)
	}
}

// TestRouterHasLargerCandidateAndPick 验证上下文适配与熔断过滤。
func TestRouterHasLargerCandidateAndPick(t *testing.T) {
	router := &Router{
		candidates: []Candidate{
			{Ref: ModelRef{ProviderID: "p1", ModelID: "m1", ContextWindowTokens: 1_000_000, MaxOutputTokens: 4096}},
			{Ref: ModelRef{ProviderID: "p2", ModelID: "m2", ContextWindowTokens: 2_000_000, MaxOutputTokens: 4096}},
		},
		config:   testConfig(),
		current:  0,
		excluded: map[int]string{},
	}
	if !router.hasLargerCandidate(1_500_000, false) {
		t.Fatal("larger candidate was not detected")
	}
	router.excluded[1] = "circuit_open"
	if router.hasLargerCandidate(1_500_000, false) {
		t.Fatal("excluded candidate must not be selected")
	}
	router.excluded = map[int]string{}
	health := newFakeHealth()
	health.allowed["p1/m1"] = false
	router.health = health
	if index := router.pick(context.Background(), 10, false, -1); index != 1 {
		t.Fatalf("pick() = %d, want second healthy candidate", index)
	}
}

// TestRouterObserverErrorsPropagate 验证 Attempt 与切换持久化失败会阻止静默执行。
func TestRouterObserverErrorsPropagate(t *testing.T) {
	router := &Router{observer: errorObserver{}, config: testConfig(), excluded: map[int]string{}}
	if err := router.started(context.Background(), AttemptRecord{AttemptNo: 1}); err == nil {
		t.Fatal("attempt start persistence failure was ignored")
	}
	if err := router.finished(context.Background(), AttemptRecord{AttemptNo: 1}); err == nil {
		t.Fatal("attempt finish persistence failure was ignored")
	}
	if err := router.notifySwitch(context.Background(), Candidate{Ref: ModelRef{ModelID: "a"}}, Candidate{Ref: ModelRef{ModelID: "b"}}, "reason"); err == nil {
		t.Fatal("switch persistence failure was ignored")
	}
}

// TestRouterLimitErrorsExposeLastFailure 验证预算错误保留底层分类信息。
func TestRouterLimitErrorsExposeLastFailure(t *testing.T) {
	if err := newAttemptLimitError(nil); err.Error() != ErrAttemptLimit.Error() {
		t.Fatalf("attempt limit error = %v", err)
	}
	last := fmt.Errorf("%w", ErrRateLimited)
	attemptErr := newAttemptLimitError(last)
	if !errors.Is(attemptErr, ErrRateLimited) || !errors.Is(attemptErr, ErrAttemptLimit) {
		t.Fatalf("attempt limit error lost classification: %v", attemptErr)
	}
	if attemptErr.(*attemptLimitError).Unwrap() == nil {
		t.Fatal("attempt limit error must unwrap the last failure")
	}
	if err := newSwitchLimitError(nil); err.Error() != ErrSwitchLimit.Error() {
		t.Fatalf("switch limit error = %v", err)
	}
	switchErr := newSwitchLimitError(last)
	if !errors.Is(switchErr, ErrRateLimited) || !errors.Is(switchErr, ErrSwitchLimit) {
		t.Fatalf("switch limit error lost classification: %v", switchErr)
	}
	if switchErr.(*switchLimitError).Unwrap() == nil {
		t.Fatal("switch limit error must unwrap the last failure")
	}
}

// TestRouterProviderLimiter 验证供应商舱壁被获取与释放，Redis 故障时降级放行。
func TestRouterProviderLimiter(t *testing.T) {
	server := newChatServer(t, func(_ int, w http.ResponseWriter, _ *http.Request) { writeCompletion(w, "ok") })
	limiter := &recordingLimiter{}
	config := testConfig()
	config.Limiter = limiter
	router, err := NewRouter([]Candidate{candidateFor(server, "p1", "m1", 1_000_000)}, newFakeHealth(), config, &attemptRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := router.Complete(context.Background(), []react.Message{{Role: react.RoleUser, Content: "hi"}}, nil); err != nil {
		t.Fatal(err)
	}
	if limiter.acquires != 1 || limiter.releases != 1 || limiter.keys[0] != "provider:p1" {
		t.Fatalf("limiter state = %#v", limiter)
	}

	// 缓存故障时放行请求，避免舱壁故障阻断全部模型调用。
	limiter = &recordingLimiter{err: errors.New("redis down")}
	config.Limiter = limiter
	router, _ = NewRouter([]Candidate{candidateFor(server, "p1", "m1", 1_000_000)}, newFakeHealth(), config, &attemptRecorder{})
	result, err := router.Complete(context.Background(), []react.Message{{Role: react.RoleUser, Content: "hi"}}, nil)
	if err != nil || result.Content != "ok" {
		t.Fatalf("fail-open Complete() = %#v, %v", result, err)
	}
}

// TestRouterRespectsSwitchLimit 验证切换预算耗尽时返回稳定错误。
func TestRouterRespectsSwitchLimit(t *testing.T) {
	first := newChatServer(t, func(_ int, w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	second := newChatServer(t, func(_ int, w http.ResponseWriter, _ *http.Request) { writeCompletion(w, "ok") })
	config := testConfig()
	config.MaxSwitchesPerRun = 0
	router, err := NewRouter([]Candidate{
		candidateFor(first, "p1", "m1", 1_000_000),
		candidateFor(second, "p2", "m2", 1_000_000),
	}, newFakeHealth(), config, &attemptRecorder{}, WithSleep(func(context.Context, time.Duration) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	_, err = router.Complete(context.Background(), []react.Message{{Role: react.RoleUser, Content: "hi"}}, nil)
	if !errors.Is(err, ErrSwitchLimit) {
		t.Fatalf("switch limit error = %v", err)
	}
}

// TestRouterRateLimitRetryWithinBudget 验证可接受 Retry-After 先等待同模型重试。
func TestRouterRateLimitRetryWithinBudget(t *testing.T) {
	server := newChatServer(t, func(call int, w http.ResponseWriter, _ *http.Request) {
		if call == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writeCompletion(w, "recovered")
	})
	router, err := NewRouter([]Candidate{candidateFor(server, "p1", "m1", 1_000_000)}, newFakeHealth(), testConfig(),
		&attemptRecorder{}, WithSleep(func(_ context.Context, duration time.Duration) error {
			if duration != time.Second {
				t.Fatalf("unexpected Retry-After wait: %v", duration)
			}
			return nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	result, err := router.Complete(context.Background(), []react.Message{{Role: react.RoleUser, Content: "hi"}}, nil)
	if err != nil || result.Content != "recovered" {
		t.Fatalf("Complete() = %#v, %v", result, err)
	}
}
