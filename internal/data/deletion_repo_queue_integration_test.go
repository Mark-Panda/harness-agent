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

// TestDeletionRepositoryQueuePaths 验证删除请求的读取、游标分页、认领栅栏、续租与完成。
func TestDeletionRepositoryQueuePaths(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	users := NewUserRepository(appDB, authDB)
	owner, err := users.CreateUser(ctx, biz.User{
		Username: "del_owner_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := users.CreateUser(ctx, biz.User{
		Username: "del_other_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adminDB.Exec("DELETE FROM users WHERE id IN (?, ?)", owner.ID, stranger.ID).Error }()

	repo := NewDeletionRepository(appDB)
	now := time.Now().UTC().Truncate(time.Millisecond)
	first, err := repo.Create(ctx, biz.DeletionRequest{
		ID: uuid.New(), UserID: owner.ID, ResourceType: biz.DeletionResourceSession, ResourceID: uuid.NewString(),
		Status: biz.DeletionPending, RequestedBy: owner.ID.String(), RequestedSource: "integration-test",
		MaxAttempts: 3, AvailableAt: now,
	}, testOutboxEvent(owner.ID, "privacy.deletion.requested"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := repo.Create(ctx, biz.DeletionRequest{
		ID: uuid.New(), UserID: owner.ID, ResourceType: biz.DeletionResourceMemory, ResourceID: owner.ID.String(),
		Status: biz.DeletionPending, RequestedBy: owner.ID.String(), RequestedSource: "integration-test",
		MaxAttempts: 3, AvailableAt: now,
	}, testOutboxEvent(owner.ID, "privacy.deletion.requested"))
	if err != nil {
		t.Fatal(err)
	}

	// Get：本人可读，跨用户不可读。
	got, err := repo.Get(ctx, owner.ID, first.ID)
	if err != nil || got.ID != first.ID {
		t.Fatalf("Get() = %#v, %v", got, err)
	}
	if _, err := repo.Get(ctx, stranger.ID, first.ID); err == nil {
		t.Fatal("cross-user Get succeeded")
	}

	// List：按用户分页返回，游标翻页不重复。
	page1, err := repo.List(ctx, owner.ID, 1, "")
	if err != nil || len(page1.Items) != 1 {
		t.Fatalf("List(page1) = %#v, %v", page1, err)
	}
	page2, err := repo.List(ctx, owner.ID, 10, page1.NextCursor)
	if err != nil || len(page2.Items) != 1 || page2.Items[0].ID == page1.Items[0].ID {
		t.Fatalf("List(page2) = %#v, %v", page2, err)
	}
	if listed, err := repo.List(ctx, stranger.ID, 10, ""); err != nil || len(listed.Items) != 0 {
		t.Fatalf("cross-user List = %#v, %v", listed, err)
	}

	// Claim：获取最早 pending 请求并颁发栅栏令牌。
	claimed, found, err := repo.Claim(ctx, "deleter-1", time.Minute, now)
	if err != nil || !found {
		t.Fatalf("Claim() found=%v err=%v", found, err)
	}
	if claimed.ID != first.ID {
		t.Fatalf("claimed unexpected request %#v", claimed)
	}
	if claimed.FencingToken == 0 || claimed.LeaseOwner != "deleter-1" {
		t.Fatalf("claim lease fields = %#v", claimed)
	}

	// Renew：正确栅栏令牌可续租，错误令牌被拒。
	if err := repo.Renew(ctx, claimed.ID, "deleter-1", claimed.FencingToken, time.Minute, now); err != nil {
		t.Fatalf("Renew() = %v", err)
	}
	if err := repo.Renew(ctx, claimed.ID, "deleter-1", claimed.FencingToken+99, time.Minute, now); err == nil {
		t.Fatal("Renew with stale fencing token succeeded")
	}
	if err := repo.Renew(ctx, claimed.ID, "deleter-other", claimed.FencingToken, time.Minute, now); err == nil {
		t.Fatal("Renew with wrong owner succeeded")
	}

	// Complete：标记完成并写 outbox；重复完成按冲突拒绝。
	if err := repo.Complete(ctx, claimed.ID, "deleter-1", claimed.FencingToken, now, testOutboxEvent(owner.ID, "privacy.deletion.completed")); err != nil {
		t.Fatalf("Complete() = %v", err)
	}
	if err := repo.Complete(ctx, claimed.ID, "deleter-1", claimed.FencingToken, now, testOutboxEvent(owner.ID, "privacy.deletion.completed")); err == nil {
		t.Fatal("duplicate Complete succeeded")
	}
	var status string
	if err := adminDB.Raw("SELECT status FROM deletion_requests WHERE id = ?", claimed.ID).Scan(&status).Error; err != nil || status != string(biz.DeletionCompleted) {
		t.Fatalf("completed status = %q, %v", status, err)
	}

	// Fail：认领第二个请求后标记失败，进入可重试状态。
	retryAt := now.Add(5 * time.Minute)
	claimed2, found2, err := repo.Claim(ctx, "deleter-1", time.Minute, now)
	if err != nil || !found2 || claimed2.ID != second.ID {
		t.Fatalf("claim for fail: %#v found=%v err=%v", claimed2, found2, err)
	}
	if err := repo.Fail(ctx, claimed2.ID, "deleter-1", claimed2.FencingToken, retryAt, "db timeout", now); err != nil {
		t.Fatalf("Fail() = %v", err)
	}
	var failed struct {
		Status     string
		LastError  string
		Available  time.Time `gorm:"column:available_at"`
		LeaseOwner string
	}
	if err := adminDB.Raw("SELECT status, last_error, available_at, lease_owner FROM deletion_requests WHERE id = ? AND status = 'pending' AND last_error = 'db timeout'", second.ID).Scan(&failed).Error; err != nil {
		t.Fatalf("failed request row = %v", err)
	}
	if failed.LastError != "db timeout" || failed.LeaseOwner != "" {
		t.Fatalf("failed request not reset for retry: %#v", failed)
	}
}
