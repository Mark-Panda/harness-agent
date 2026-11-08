//go:build integration

package data

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
)

// TestAgentRepositoryAdminPaths 验证管理员视角的 Agent 读取、目录分页与删除，以及角色变更。
func TestAgentRepositoryAdminPaths(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	users := NewUserRepository(appDB, authDB)
	admin, err := users.CreateUser(ctx, biz.User{
		Username: "agent_admin_" + suffix, PasswordHash: "hash", Role: biz.RoleAdmin,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	owner, err := users.CreateUser(ctx, biz.User{
		Username: "agent_owner_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adminDB.Exec("DELETE FROM users WHERE id IN (?, ?)", admin.ID, owner.ID).Error }()

	repo := NewAgentRepository(appDB)
	agent, err := repo.CreateAgent(ctx, biz.Agent{
		ID: uuid.New(), UserID: owner.ID, Name: "admin-visible-" + suffix, Mode: biz.AgentModeReact,
		Version: 1, Status: biz.AgentDraft,
	}, testOutboxEvent(owner.ID, "agent.created"))
	if err != nil {
		t.Fatal(err)
	}

	// GetAgentAdmin：管理员可读任意用户的 Agent，含绑定信息为空时不报错。
	got, err := repo.GetAgentAdmin(ctx, agent.ID)
	if err != nil || got.ID != agent.ID || got.Name != agent.Name {
		t.Fatalf("GetAgentAdmin() = %#v, %v", got, err)
	}
	if _, err := repo.GetAgentAdmin(ctx, uuid.New()); !errors.Is(err, biz.ErrResourceNotFound) {
		t.Fatalf("GetAgentAdmin(missing) error = %v", err)
	}

	// ListAllAgents：按用户过滤与游标分页。
	page, err := repo.ListAllAgents(ctx, biz.AdminAgentFilter{UserID: &owner.ID}, 10, "")
	if err != nil || len(page.Items) != 1 || page.Items[0].Agent.ID != agent.ID {
		t.Fatalf("ListAllAgents(filter) = %#v, %v", page, err)
	}
	strangerID := uuid.New()
	if page, err := repo.ListAllAgents(ctx, biz.AdminAgentFilter{UserID: &strangerID}, 10, ""); err != nil || len(page.Items) != 0 {
		t.Fatalf("ListAllAgents(stranger) = %#v, %v", page, err)
	}

	// UpdateUserRole：普通用户提升为管理员并写 outbox，非法角色被拒绝。
	if err := users.UpdateUserRole(ctx, owner.ID, biz.RoleAdmin, testOutboxEvent(uuid.Nil, "admin.user.role")); err != nil {
		t.Fatalf("UpdateUserRole() = %v", err)
	}
	if got, err := users.GetUserByID(ctx, owner.ID); err != nil || got.Role != biz.RoleAdmin {
		t.Fatalf("role after update = %#v, %v", got, err)
	}
	if err := users.UpdateUserRole(ctx, owner.ID, "superroot", testOutboxEvent(uuid.Nil, "admin.user.role")); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("invalid role error = %v", err)
	}

	// DeleteAgentAdmin：管理员删除后 Get 未命中，重复删除返回未命中。
	if err := repo.DeleteAgentAdmin(ctx, admin.ID, agent.ID, testOutboxEvent(admin.ID, "admin.agent.deleted")); err != nil {
		t.Fatalf("DeleteAgentAdmin() = %v", err)
	}
	if _, err := repo.GetAgentAdmin(ctx, agent.ID); !errors.Is(err, biz.ErrResourceNotFound) {
		t.Fatalf("deleted agent error = %v", err)
	}
	if err := repo.DeleteAgentAdmin(ctx, admin.ID, agent.ID, testOutboxEvent(admin.ID, "admin.agent.deleted")); !errors.Is(err, biz.ErrResourceNotFound) {
		t.Fatalf("double delete error = %v", err)
	}
}
