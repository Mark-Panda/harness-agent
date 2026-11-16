//go:build integration

package data

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type runCreationNotifierFunc func(context.Context, uuid.UUID)

func (notify runCreationNotifierFunc) Publish(ctx context.Context, runID uuid.UUID) {
	notify(ctx, runID)
}

func TestRunCreationRollsBackWhenEnqueueFails(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	user, err := NewUserRepository(appDB, authDB).CreateUser(ctx, biz.User{
		Username: "atomic_run_" + uuid.NewString(), PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := adminDB.Exec("DELETE FROM jobs WHERE user_id = ?", user.ID).Error; err != nil {
			t.Error(err)
		}
		if err := adminDB.Exec("DELETE FROM users WHERE id = ?", user.ID).Error; err != nil {
			t.Error(err)
		}
	})
	sessions := NewSessionRepository(appDB)
	session, err := sessions.CreateSession(ctx, biz.Session{
		ID: uuid.New(), UserID: user.ID, Title: "atomic run creation", Source: biz.SessionSourceWeb, Status: biz.SessionActive, Version: 1,
	}, testOutboxEvent(user.ID, "test.session"))
	if err != nil {
		t.Fatal(err)
	}
	queue, plans := NewJobRepository(appDB), NewPlanRepository(appDB)
	var notified []uuid.UUID
	runs := NewRunRepository(appDB, WithRunEventNotifier(runCreationNotifierFunc(func(ctx context.Context, runID uuid.UUID) {
		notified = append(notified, runID)
		exists, err := queue.HasRunJob(ctx, user.ID, runID)
		if err != nil || !exists {
			t.Errorf("notified before job commit: exists=%v err=%v", exists, err)
		}
	})))
	previous, err := runs.CreateRun(ctx, biz.Run{
		ID: uuid.New(), SessionID: session.ID, UserID: user.ID, Mode: biz.AgentModePlan, Status: biz.RunRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := plans.CreatePlan(ctx, user.ID, biz.Plan{
		ID: uuid.New(), SessionID: session.ID, RunID: previous.ID, UserID: user.ID, Version: 1,
		Status: biz.PlanPaused, Goal: "resume this plan", MaxSteps: 5,
	}, testOutboxEvent(user.ID, "test.plan"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.AppendMessage(ctx, biz.Message{
		ID: uuid.New(), SessionID: session.ID, UserID: user.ID, Role: biz.MessageUser, Content: "继续",
	}, testOutboxEvent(user.ID, "test.message")); err != nil {
		t.Fatal(err)
	}
	service, err := biz.NewRunService(runs, sessions, biz.WithRunEnqueuer(queue),
		biz.WithPlanResume(plans),
		biz.WithRunCreationTransaction(NewRunCreationTransaction(appDB)))
	if err != nil {
		t.Fatal(err)
	}
	principal := biz.Principal{UserID: user.ID}
	input := biz.CreateRunInput{SessionID: session.ID, Mode: biz.AgentModePlan, IdempotencyKey: "enqueue-retry"}
	enqueueErr := errors.New("injected enqueue failure")
	if err := appDB.Callback().Create().After("gorm:create").Register("test:fail-job-insert", func(tx *gorm.DB) {
		if tx.Statement.Table == "jobs" {
			_ = tx.AddError(enqueueErr)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Create(ctx, principal, input); !errors.Is(err, enqueueErr) {
		t.Fatalf("Create error = %v, want %v", err, enqueueErr)
	}
	page, err := runs.ListRuns(ctx, user.ID, session.ID, 20, "")
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != previous.ID || page.Items[0].Status != biz.RunRunning {
		t.Fatalf("runs after enqueue rollback = %v, err=%v", page.Items, err)
	}
	paused, err := plans.GetPlan(ctx, user.ID, plan.ID)
	if err != nil || paused.RunID != previous.ID || paused.Status != biz.PlanPaused || len(notified) != 0 {
		t.Fatalf("plan after enqueue rollback = %#v, err=%v, notifications=%v", paused, err, notified)
	}
	if err := appDB.Callback().Create().Remove("test:fail-job-insert"); err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(ctx, principal, input)
	if err != nil || created.Status != biz.RunQueued {
		t.Fatalf("retry after rollback = %#v, err=%v", created, err)
	}
	rebound, err := plans.GetPlanByRun(ctx, user.ID, created.ID)
	if err != nil || rebound.ID != plan.ID || rebound.Status != biz.PlanExecuting || len(notified) != 1 || notified[0] != created.ID {
		t.Fatalf("plan after enqueue commit = %#v, err=%v, notifications=%v", rebound, err, notified)
	}
	replayed, err := service.Create(ctx, principal, input)
	if err != nil || replayed.ID != created.ID {
		t.Fatalf("idempotent retry = %#v, err=%v", replayed, err)
	}
	// Reproduce a queued Run left without a job by an older server.
	if err := adminDB.Exec("DELETE FROM jobs WHERE user_id = ? AND dedupe_key = ?", user.ID, "run:"+created.ID.String()).Error; err != nil {
		t.Fatal(err)
	}
	repaired, err := service.Create(ctx, principal, input)
	if err != nil || repaired.ID != created.ID {
		t.Fatalf("repair of queued Run = %#v, err=%v", repaired, err)
	}
	var jobs int64
	err = WithScope(ctx, appDB, Scope{UserID: user.ID}, func(tx *gorm.DB) error {
		return tx.Model(&jobModel{}).Where("user_id = ? AND dedupe_key = ?", user.ID, "run:"+created.ID.String()).Count(&jobs).Error
	})
	if err != nil || jobs != 1 {
		t.Fatalf("jobs for committed run = %d, err=%v", jobs, err)
	}
}

// TestRunRepositoryLifecycleAndEventCursor 验证 Run 创建、事件单调序号、游标与跨用户隔离。
func TestRunRepositoryLifecycleAndEventCursor(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	users := NewUserRepository(appDB, authDB)
	userA, err := users.CreateUser(ctx, biz.User{Username: "run_a_" + suffix, PasswordHash: "hash", Role: biz.RoleUser, Status: biz.UserStatusActive, CredentialVersion: 1}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	userB, err := users.CreateUser(ctx, biz.User{Username: "run_b_" + suffix, PasswordHash: "hash", Role: biz.RoleUser, Status: biz.UserStatusActive, CredentialVersion: 1}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	defer adminDB.Exec("DELETE FROM users WHERE id IN (?, ?)", userA.ID, userB.ID)

	sessions := NewSessionRepository(appDB)
	session, err := sessions.CreateSession(ctx, biz.Session{
		ID: uuid.New(), UserID: userA.ID, Title: "run-session", Source: biz.SessionSourceWeb, Status: biz.SessionActive, Version: 1,
	}, testOutboxEvent(userA.ID, "test.session"))
	if err != nil {
		t.Fatal(err)
	}
	runs := NewRunRepository(appDB)
	deadline := time.Now().UTC().Add(5 * time.Minute)
	key := "run-key-" + suffix
	created, err := runs.CreateRun(ctx, biz.Run{
		ID: uuid.New(), SessionID: session.ID, UserID: userA.ID, Mode: biz.AgentModeReact,
		Status: biz.RunQueued, IdempotencyKey: key, DeadlineAt: &deadline,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 相同幂等键必须返回同一条 Run。
	again, err := runs.CreateRun(ctx, biz.Run{
		ID: uuid.New(), SessionID: session.ID, UserID: userA.ID, Mode: biz.AgentModeReact,
		Status: biz.RunQueued, IdempotencyKey: key, DeadlineAt: &deadline,
	})
	if err != nil || again.ID != created.ID {
		t.Fatalf("idempotent create = %#v, %v", again, err)
	}
	if _, err := runs.GetRun(ctx, userB.ID, created.ID); !errors.Is(err, biz.ErrResourceNotFound) {
		t.Fatalf("cross-user run lookup error = %v", err)
	}
	// 轻量状态查询必须同样按 user_id 隔离。
	if status, err := runs.GetRunStatus(ctx, userA.ID, created.ID); err != nil || status != biz.RunQueued {
		t.Fatalf("GetRunStatus() = %q, %v", status, err)
	}
	if _, err := runs.GetRunStatus(ctx, userB.ID, created.ID); !errors.Is(err, biz.ErrResourceNotFound) {
		t.Fatalf("cross-user status lookup error = %v", err)
	}

	for index := 0; index < 3; index++ {
		event, err := runs.AppendEvent(ctx, biz.RunEvent{
			RunID: created.ID, UserID: userA.ID, EventType: "message.delta", Payload: map[string]any{"index": index},
		})
		if err != nil {
			t.Fatal(err)
		}
		if event.Seq != int64(index+1) {
			t.Fatalf("event seq = %d, want %d", event.Seq, index+1)
		}
	}
	events, err := runs.ListEvents(ctx, userA.ID, created.ID, 1, 10)
	if err != nil || len(events) != 2 || events[0].Seq != 2 {
		t.Fatalf("ListEvents(after=1) = %#v, %v", events, err)
	}
	if _, err := runs.ListEvents(ctx, userB.ID, created.ID, 0, 10); err != nil {
		t.Fatal(err)
	}
	page, err := runs.ListRuns(ctx, userA.ID, session.ID, 20, "")
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("ListRuns() = %#v, %v", page, err)
	}

	running, err := runs.TransitionRun(ctx, userA.ID, created.ID, biz.RunQueued, biz.RunRunning, nil)
	if err != nil || running.Status != biz.RunRunning {
		t.Fatalf("TransitionRun(running) = %#v, %v", running, err)
	}
	completed, err := runs.TransitionRun(ctx, userA.ID, created.ID, biz.RunRunning, biz.RunCompleted, map[string]any{"finished_at": time.Now().UTC()})
	if err != nil || completed.Status != biz.RunCompleted {
		t.Fatalf("TransitionRun(completed) = %#v, %v", completed, err)
	}
	if _, err := runs.TransitionRun(ctx, userA.ID, created.ID, biz.RunQueued, biz.RunRunning, nil); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("illegal transition error = %v", err)
	}
}

// TestRunRepositorySerializesSessionRuns 验证同会话 Run 允许排队但必须串行认领。
func TestRunRepositorySerializesSessionRuns(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	users := NewUserRepository(appDB, authDB)
	user, err := users.CreateUser(ctx, biz.User{Username: "serial_" + suffix, PasswordHash: "hash", Role: biz.RoleUser, Status: biz.UserStatusActive, CredentialVersion: 1}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	defer adminDB.Exec("DELETE FROM users WHERE id = ?", user.ID)
	sessions := NewSessionRepository(appDB)
	session, err := sessions.CreateSession(ctx, biz.Session{
		ID: uuid.New(), UserID: user.ID, Title: "serial", Source: biz.SessionSourceWeb, Status: biz.SessionActive, Version: 1,
	}, testOutboxEvent(user.ID, "test.session"))
	if err != nil {
		t.Fatal(err)
	}
	runs := NewRunRepository(appDB)
	// 触发消息边界与执行截止时间必须随 Run 一起持久化，供 Worker 组装上下文与超时判定。
	deadline := time.Now().UTC().Add(5 * time.Minute)
	triggerID := uuid.New()
	triggerAt := time.Now().UTC()
	first, err := runs.CreateRun(ctx, biz.Run{
		ID: uuid.New(), SessionID: session.ID, UserID: user.ID, Mode: biz.AgentModeReact, Status: biz.RunQueued,
		DeadlineAt: &deadline, TriggerMessageID: &triggerID, TriggerMessageAt: &triggerAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.TriggerMessageID == nil || *first.TriggerMessageID != triggerID || first.TriggerMessageAt == nil {
		t.Fatalf("trigger boundary was not persisted: %#v", first)
	}
	// 同会话第二个 Run 允许排队，不再直接返回 409。
	secondDeadline := time.Now().UTC().Add(5 * time.Minute)
	second, err := runs.CreateRun(ctx, biz.Run{
		ID: uuid.New(), SessionID: session.ID, UserID: user.ID, Mode: biz.AgentModeReact, Status: biz.RunQueued,
		DeadlineAt: &secondDeadline,
	})
	if err != nil {
		t.Fatalf("queued creation must not conflict: %v", err)
	}
	// 模拟排队两分钟：截止时间固定于创建时刻，若不补偿，后续 Run 认领后执行窗口会被排队耗尽。
	if err := adminDB.Exec("UPDATE agent_runs SET created_at = ? WHERE id = ?", time.Now().UTC().Add(-2*time.Minute), second.ID).Error; err != nil {
		t.Fatalf("backdate queued run: %v", err)
	}
	now := time.Now().UTC()
	claimed, fence, err := runs.ClaimRun(ctx, user.ID, first.ID, "worker-a", time.Minute, now)
	if err != nil || claimed.Status != biz.RunRunning {
		t.Fatalf("ClaimRun(first) = %#v, %#v, %v", claimed, fence, err)
	}
	// 并发 Attempt 乱序写入时，Run 只能保留最大序号，不能回退。
	if err := runs.SetRunAttempt(ctx, user.ID, first.ID, 7, fence); err != nil {
		t.Fatal(err)
	}
	if err := runs.SetRunAttempt(ctx, user.ID, first.ID, 3, fence); err != nil {
		t.Fatal(err)
	}
	current, err := runs.GetRun(ctx, user.ID, first.ID)
	if err != nil || current.AttemptNo != 7 {
		t.Fatalf("attempt number regressed: %#v, %v", current, err)
	}
	// 前序 Run 运行中时，后续 Run 只能继续排队。
	if _, _, err := runs.ClaimRun(ctx, user.ID, second.ID, "worker-b", time.Minute, time.Now().UTC()); !errors.Is(err, biz.ErrSessionRunActive) {
		t.Fatalf("second ClaimRun() error = %v", err)
	}
	if _, err := runs.FinishRun(ctx, user.ID, first.ID, biz.RunRunning, biz.RunCompleted, nil, fence, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	claimedSecond, _, err := runs.ClaimRun(ctx, user.ID, second.ID, "worker-b", time.Minute, time.Now().UTC())
	if err != nil || claimedSecond.Status != biz.RunRunning {
		t.Fatalf("ClaimRun(second) = %#v, %v", claimedSecond, err)
	}
	// 排队时长要补回截止时间，避免后续 Run 一被认领就超时。
	if claimedSecond.DeadlineAt == nil || claimedSecond.DeadlineAt.Before(secondDeadline.Add(time.Minute)) {
		t.Fatalf("queued wait was not compensated: %#v", claimedSecond.DeadlineAt)
	}
}

// TestRunRepositoryCompensatesQueuedDeadlineOnce 验证首次排队、重复暂停和并发认领均不会重复补偿执行预算。
func TestRunRepositoryCompensatesQueuedDeadlineOnce(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	user, err := NewUserRepository(appDB, authDB).CreateUser(ctx, biz.User{
		Username: "run_deadline_" + strings.ReplaceAll(uuid.NewString(), "-", ""), PasswordHash: "hash",
		Role: biz.RoleUser, Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adminDB.Exec("DELETE FROM users WHERE id = ?", user.ID).Error })
	session, err := NewSessionRepository(appDB).CreateSession(ctx, biz.Session{
		ID: uuid.New(), UserID: user.ID, Title: "deadline compensation", Source: biz.SessionSourceWeb,
		Status: biz.SessionActive, Version: 1,
	}, testOutboxEvent(user.ID, "test.session"))
	if err != nil {
		t.Fatal(err)
	}
	runs := NewRunRepository(appDB)
	// 租约校验同时参考数据库 clock_timestamp，测试时间线从当前时刻开始，避免使用已过期租约。
	base := time.Now().UTC().Truncate(time.Millisecond)
	initialDeadline := base.Add(30 * time.Minute)
	run, err := runs.CreateRun(ctx, biz.Run{
		ID: uuid.New(), SessionID: session.ID, UserID: user.ID, Mode: biz.AgentModePlan,
		Status: biz.RunQueued, DeadlineAt: &initialDeadline,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := adminDB.Exec("UPDATE agent_runs SET created_at = ?, updated_at = ? WHERE id = ?", base, base, run.ID).Error; err != nil {
		t.Fatal(err)
	}

	firstClaimAt := base.Add(2 * time.Minute)
	claimed, fence, err := runs.ClaimRun(ctx, user.ID, run.ID, "worker-a", time.Minute, firstClaimAt)
	if err != nil {
		t.Fatal(err)
	}
	wantDeadline := initialDeadline.Add(2 * time.Minute)
	if claimed.DeadlineAt == nil || !claimed.DeadlineAt.Equal(wantDeadline) {
		t.Fatalf("first queued deadline = %v, want %v", claimed.DeadlineAt, wantDeadline)
	}

	requeueAt := firstClaimAt.Add(30 * time.Second)
	if err := runs.RequeuePausedRun(ctx, user.ID, run.ID, fence, requeueAt); err != nil {
		t.Fatal(err)
	}
	requeued, err := runs.GetRun(ctx, user.ID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if requeued.Status != biz.RunQueued || requeued.DeadlineAt == nil || !requeued.DeadlineAt.Equal(wantDeadline) {
		t.Fatalf("requeue changed deadline: %#v", requeued)
	}
	if !requeued.UpdatedAt.Equal(requeueAt) {
		t.Fatalf("requeue updated_at = %v, want %v", requeued.UpdatedAt, requeueAt)
	}

	secondClaimAt := requeueAt.Add(3 * time.Minute)
	claimed, fence, err = runs.ClaimRun(ctx, user.ID, run.ID, "worker-b", time.Minute, secondClaimAt)
	if err != nil {
		t.Fatal(err)
	}
	wantDeadline = wantDeadline.Add(3 * time.Minute)
	if claimed.DeadlineAt == nil || !claimed.DeadlineAt.Equal(wantDeadline) {
		t.Fatalf("second queued deadline = %v, want %v", claimed.DeadlineAt, wantDeadline)
	}

	secondRequeueAt := secondClaimAt.Add(30 * time.Second)
	if err := runs.RequeuePausedRun(ctx, user.ID, run.ID, fence, secondRequeueAt); err != nil {
		t.Fatal(err)
	}
	thirdClaimAt := secondRequeueAt.Add(4 * time.Minute)
	claimed, fence, err = runs.ClaimRun(ctx, user.ID, run.ID, "worker-c", time.Minute, thirdClaimAt)
	if err != nil {
		t.Fatal(err)
	}
	wantDeadline = wantDeadline.Add(4 * time.Minute)
	if claimed.DeadlineAt == nil || !claimed.DeadlineAt.Equal(wantDeadline) {
		t.Fatalf("repeated pause deadline = %v, want %v", claimed.DeadlineAt, wantDeadline)
	}
	if _, err := runs.FinishRun(ctx, user.ID, run.ID, biz.RunRunning, biz.RunCompleted, nil, fence, thirdClaimAt.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}

	concurrentBase := base.Add(10 * time.Minute)
	concurrentDeadline := concurrentBase.Add(30 * time.Minute)
	concurrentRun, err := runs.CreateRun(ctx, biz.Run{
		ID: uuid.New(), SessionID: session.ID, UserID: user.ID, Mode: biz.AgentModeReact,
		Status: biz.RunQueued, DeadlineAt: &concurrentDeadline,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := adminDB.Exec("UPDATE agent_runs SET created_at = ?, updated_at = ? WHERE id = ?", concurrentBase, concurrentBase, concurrentRun.ID).Error; err != nil {
		t.Fatal(err)
	}
	claimAt := concurrentBase.Add(5 * time.Minute)
	type claimResult struct {
		run   biz.Run
		fence biz.RunFence
		err   error
	}
	results := make(chan claimResult, 2)
	var wg sync.WaitGroup
	for _, owner := range []string{"worker-d", "worker-e"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claimed, fence, err := runs.ClaimRun(ctx, user.ID, concurrentRun.ID, owner, time.Minute, claimAt)
			results <- claimResult{run: claimed, fence: fence, err: err}
		}()
	}
	wg.Wait()
	close(results)
	successes := 0
	for result := range results {
		if result.err == nil {
			successes++
			continue
		}
		if !errors.Is(result.err, biz.ErrResourceConflict) {
			t.Fatalf("concurrent ClaimRun error = %v", result.err)
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent ClaimRun successes = %d, want 1", successes)
	}
	finalRun, err := runs.GetRun(ctx, user.ID, concurrentRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantConcurrentDeadline := concurrentDeadline.Add(5 * time.Minute)
	if finalRun.DeadlineAt == nil || !finalRun.DeadlineAt.Equal(wantConcurrentDeadline) {
		t.Fatalf("concurrent claim deadline = %v, want %v", finalRun.DeadlineAt, wantConcurrentDeadline)
	}
}

// TestRunRepositoryListAllRuns 验证管理员全局运行列表的系统作用域、联表字段与筛选。
func TestRunRepositoryListAllRuns(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	repository := NewRunRepository(appDB)
	ctx := context.Background()
	page, err := repository.ListAllRuns(ctx, biz.AdminRunFilter{}, 5, "")
	if err != nil {
		t.Fatalf("list all runs: %v", err)
	}
	if len(page.Items) == 0 {
		return
	}
	first := page.Items[0]
	if first.Source == "" || first.Username == "" || first.Run.UserID == uuid.Nil {
		t.Fatalf("missing joined fields: %#v", first)
	}
	userPage, err := repository.ListAllRuns(ctx, biz.AdminRunFilter{UserID: &first.Run.UserID}, 5, "")
	if err != nil || len(userPage.Items) == 0 {
		t.Fatalf("filter by user returned %#v, %v", userPage, err)
	}
	for _, item := range userPage.Items {
		if item.Run.UserID != first.Run.UserID {
			t.Fatalf("user filter leaked another user: %#v", item)
		}
	}
	sourcePage, err := repository.ListAllRuns(ctx, biz.AdminRunFilter{Source: first.Source}, 5, "")
	if err != nil || len(sourcePage.Items) == 0 {
		t.Fatalf("filter by source returned %#v, %v", sourcePage, err)
	}
	for _, item := range sourcePage.Items {
		if item.Source != first.Source {
			t.Fatalf("source filter leaked another source: %#v", item)
		}
	}
}
