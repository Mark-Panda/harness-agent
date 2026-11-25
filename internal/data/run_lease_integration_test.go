//go:build integration

package data

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
)

// TestRunRepositoryReleaseRunLease 验证停机释放 Run 租约后可被其他 Worker 立即接管，且旧栅栏不能继续操作。
func TestRunRepositoryReleaseRunLease(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	users := NewUserRepository(appDB, authDB)
	user, err := users.CreateUser(ctx, biz.User{
		Username: "lease_user_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adminDB.Exec("DELETE FROM users WHERE id = ?", user.ID).Error }()

	sessions := NewSessionRepository(appDB)
	session, err := sessions.CreateSession(ctx, biz.Session{
		ID: uuid.New(), UserID: user.ID, Title: "lease", Source: biz.SessionSourceWeb,
		Status: biz.SessionActive, Version: 1,
	}, testOutboxEvent(user.ID, "session.created"))
	if err != nil {
		t.Fatal(err)
	}
	runs := NewRunRepository(appDB)
	run, err := runs.CreateRun(ctx, biz.Run{
		ID: uuid.New(), SessionID: session.ID, UserID: user.ID, Mode: biz.AgentModeReact, Status: biz.RunQueued,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adminDB.Exec("DELETE FROM agent_runs WHERE id = ?", run.ID).Error }()

	now := time.Now().UTC()
	if _, fence, err := runs.ClaimRun(ctx, user.ID, run.ID, "worker-a", time.Minute, now); err != nil {
		t.Fatalf("claim run: %v", err)
	} else if err := runs.ReleaseRunLease(ctx, user.ID, run.ID, fence, now.Add(time.Second)); err != nil {
		t.Fatalf("release run lease: %v", err)
	}
	var state struct {
		LeaseOwner     string
		LeaseExpiresAt *time.Time
		Status         string
	}
	if err := adminDB.Raw("SELECT lease_owner, lease_expires_at, status FROM agent_runs WHERE id = ?", run.ID).Scan(&state).Error; err != nil {
		t.Fatal(err)
	}
	if state.LeaseOwner != "" || state.LeaseExpiresAt != nil || state.Status != string(biz.RunRunning) {
		t.Fatalf("unexpected released run state: %+v", state)
	}

	// 新 Worker 必须能立即接管，而不是等待原租约到期。
	claimed, secondFence, err := runs.ClaimRun(ctx, user.ID, run.ID, "worker-b", time.Minute, now.Add(2*time.Second))
	if err != nil || claimed.Status != biz.RunRunning {
		t.Fatalf("second claim = %v, %v", claimed.Status, err)
	}
	if secondFence.Token <= 1 {
		t.Fatalf("fencing token did not advance: %#v", secondFence)
	}
	// 旧栅栏不得再释放新租约。
	if err := runs.ReleaseRunLease(ctx, user.ID, run.ID, biz.RunFence{Owner: "worker-a", Token: 1}, now.Add(3*time.Second)); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("stale fence release error = %v", err)
	}
}

// TestJobRepositoryPendingRunCap 验证单用户排队上限，且重复入队不会被自己的任务挡住。
func TestJobRepositoryPendingRunCap(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	users := NewUserRepository(appDB, authDB)
	user, err := users.CreateUser(ctx, biz.User{
		Username: "cap_user_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	first, second, third := uuid.New(), uuid.New(), uuid.New()
	defer func() {
		_ = adminDB.Exec("DELETE FROM jobs WHERE dedupe_key IN (?, ?, ?)",
			"run:"+first.String(), "run:"+second.String(), "run:"+third.String()).Error
		_ = adminDB.Exec("DELETE FROM users WHERE id = ?", user.ID).Error
	}()

	// 使用将来时间避免在线 Worker 在测试窗口内认领这些任务；上限计数不关心 available_at。
	future := time.Now().UTC().Add(time.Hour)
	jobs := NewJobRepository(appDB, WithMaxPendingRunsPerUser(2))
	if err := jobs.EnqueueRunLane(ctx, user.ID, first, biz.AgentModeReact, future); err != nil {
		t.Fatal(err)
	}
	if err := jobs.EnqueueRunLane(ctx, user.ID, second, biz.AgentModeReact, future); err != nil {
		t.Fatal(err)
	}
	if err := jobs.EnqueueRunLane(ctx, user.ID, third, biz.AgentModeReact, future); !errors.Is(err, biz.ErrQueueSaturated) {
		t.Fatalf("expected queue saturated error, got %v", err)
	}
	// 同一 Run 的重复入队（审批恢复等）不应被自己的任务挡住。
	if err := jobs.EnqueueRunLane(ctx, user.ID, first, biz.AgentModeReact, future); err != nil {
		t.Fatalf("re-enqueue existing run: %v", err)
	}
	if capacity, err := jobs.HasRunCapacity(ctx, user.ID); err != nil || capacity {
		t.Fatalf("capacity = %v, %v; want full", capacity, err)
	}
}

// TestJobRepositoryRequeueRespectsPendingCap 验证人工重放不能绕过单用户排队上限。
func TestJobRepositoryRequeueRespectsPendingCap(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	users := NewUserRepository(appDB, authDB)
	user, err := users.CreateUser(ctx, biz.User{
		Username: "replay_user_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	first, second := uuid.New(), uuid.New()
	defer func() {
		_ = adminDB.Exec("DELETE FROM jobs WHERE dedupe_key IN (?, ?)", "run:"+first.String(), "run:"+second.String()).Error
		_ = adminDB.Exec("DELETE FROM users WHERE id = ?", user.ID).Error
	}()

	future := time.Now().UTC().Add(time.Hour)
	jobs := NewJobRepository(appDB, WithMaxPendingRunsPerUser(1))
	if err := jobs.EnqueueRunLane(ctx, user.ID, first, biz.AgentModeReact, future); err != nil {
		t.Fatal(err)
	}
	if err := adminDB.Exec("UPDATE jobs SET status = 'failed' WHERE dedupe_key = ?", "run:"+first.String()).Error; err != nil {
		t.Fatal(err)
	}
	if err := jobs.EnqueueRunLane(ctx, user.ID, second, biz.AgentModeReact, future); err != nil {
		t.Fatal(err)
	}
	if err := jobs.RequeueRun(ctx, first, time.Now().UTC()); !errors.Is(err, biz.ErrQueueSaturated) {
		t.Fatalf("expected requeue saturated error, got %v", err)
	}
}

// TestJobRepositoryStatsIncludeReclaimableDead 验证 dead 但 Run 仍活跃的任务计入队列深度。
func TestJobRepositoryStatsIncludeReclaimableDead(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	users := NewUserRepository(appDB, authDB)
	user, err := users.CreateUser(ctx, biz.User{
		Username: "deadstat_user_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adminDB.Exec("DELETE FROM users WHERE id = ?", user.ID).Error }()

	sessions := NewSessionRepository(appDB)
	session, err := sessions.CreateSession(ctx, biz.Session{
		ID: uuid.New(), UserID: user.ID, Title: "deadstat", Source: biz.SessionSourceWeb,
		Status: biz.SessionActive, Version: 1,
	}, testOutboxEvent(user.ID, "session.created"))
	if err != nil {
		t.Fatal(err)
	}
	runs := NewRunRepository(appDB)
	run, err := runs.CreateRun(ctx, biz.Run{
		ID: uuid.New(), SessionID: session.ID, UserID: user.ID, Mode: biz.AgentModeReact, Status: biz.RunQueued,
	})
	if err != nil {
		t.Fatal(err)
	}
	lane := "test-dead-" + suffix[:8]
	if err := adminDB.Exec(`INSERT INTO jobs (id, user_id, job_type, lane, dedupe_key, payload, status, attempts, max_attempts, available_at)
VALUES (?, ?, 'agent.run', ?, ?, '{}'::jsonb, 'dead', 8, 8, now())`,
		uuid.New(), user.ID, lane, "run:"+run.ID.String()).Error; err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adminDB.Exec("DELETE FROM jobs WHERE dedupe_key = ?", "run:"+run.ID.String()).Error }()

	stats, err := NewJobRepository(appDB).StatsByLane(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, stat := range stats {
		if stat.Lane == lane {
			found = true
			if stat.Pending < 1 {
				t.Fatalf("reclaimable dead job not counted: %#v", stat)
			}
		}
	}
	if !found {
		t.Fatalf("lane %q missing from stats", lane)
	}
}
