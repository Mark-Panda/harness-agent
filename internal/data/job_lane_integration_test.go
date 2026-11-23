//go:build integration

package data

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
)

// TestJobRepositoryClaimLaneIsolation 验证不同资源池只能认领自己的 lane，且 fencing token 正常递增。
func TestJobRepositoryClaimLaneIsolation(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	user, err := NewUserRepository(appDB, authDB).CreateUser(ctx, biz.User{
		Username: "lane_user_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	reactRunID, planRunID := uuid.New(), uuid.New()
	defer func() {
		_ = adminDB.Exec("DELETE FROM jobs WHERE dedupe_key IN (?, ?)", "run:"+reactRunID.String(), "run:"+planRunID.String()).Error
		_ = adminDB.Exec("DELETE FROM users WHERE id = ?", user.ID).Error
	}()

	jobs := NewJobRepository(appDB)
	if err := jobs.EnqueueRunLane(ctx, user.ID, reactRunID, biz.AgentModeReact, time.Time{}); err != nil {
		t.Fatalf("enqueue react run: %v", err)
	}
	if err := jobs.EnqueueRunLane(ctx, user.ID, planRunID, biz.AgentModePlan, time.Time{}); err != nil {
		t.Fatalf("enqueue plan run: %v", err)
	}
	var reactLane, planLane string
	if err := adminDB.Raw("SELECT lane FROM jobs WHERE dedupe_key = ?", "run:"+reactRunID.String()).Scan(&reactLane).Error; err != nil {
		t.Fatalf("read react job lane: %v", err)
	}
	if err := adminDB.Raw("SELECT lane FROM jobs WHERE dedupe_key = ?", "run:"+planRunID.String()).Scan(&planLane).Error; err != nil {
		t.Fatalf("read plan job lane: %v", err)
	}
	if reactLane != biz.JobLaneReact || planLane != biz.JobLanePlan {
		t.Fatalf("unexpected job lanes: react=%q plan=%q", reactLane, planLane)
	}

	now := time.Now().UTC()
	reactClaim, ok, err := jobs.ClaimLane(ctx, biz.JobClaim{
		WorkerID: "worker-react", JobType: "agent.run", Lanes: []string{biz.JobLaneReact},
		Lease: time.Minute, Now: now,
	})
	if err != nil || !ok {
		t.Fatalf("claim react lane = %v, %v", ok, err)
	}
	if reactClaim.Lane != biz.JobLaneReact || reactClaim.FencingToken != 1 {
		t.Fatalf("unexpected react claim: %#v", reactClaim)
	}
	// 同一池再次认领应拿不到任务；plan 任务仍留在队列。
	if _, ok, err := jobs.ClaimLane(ctx, biz.JobClaim{
		WorkerID: "worker-react", JobType: "agent.run", Lanes: []string{biz.JobLaneReact},
		Lease: time.Minute, Now: now,
	}); err != nil || ok {
		t.Fatalf("react pool claimed non-react job: ok=%v err=%v", ok, err)
	}
	planClaim, ok, err := jobs.ClaimLane(ctx, biz.JobClaim{
		WorkerID: "worker-plan", JobType: "agent.run", Lanes: []string{biz.JobLanePlan},
		Lease: time.Minute, Now: now,
	})
	if err != nil || !ok {
		t.Fatalf("claim plan lane = %v, %v", ok, err)
	}
	if planClaim.Lane != biz.JobLanePlan || planClaim.FencingToken != 1 {
		t.Fatalf("unexpected plan claim: %#v", planClaim)
	}
	if _, ok, err := jobs.ClaimLane(ctx, biz.JobClaim{
		WorkerID: "worker-super", JobType: "agent.run", Lanes: []string{biz.JobLaneSupervisor},
		Lease: time.Minute, Now: now,
	}); err != nil || ok {
		t.Fatalf("super pool claimed other lane: ok=%v err=%v", ok, err)
	}
}
