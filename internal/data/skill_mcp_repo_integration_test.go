//go:build integration

package data

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
)

// TestSkillAndMCPRepositoryPersistence 验证 Skill 模板与 MCP 加密凭证的真实落库行为。
func TestSkillAndMCPRepositoryPersistence(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	actor, err := NewUserRepository(appDB, authDB).CreateUser(ctx, biz.User{
		Username: "catalog_skill_" + suffix, PasswordHash: "hash", Role: biz.RoleAdmin, Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	repository := NewCatalogRepository(appDB)
	skillID := uuid.New()
	mcpID := uuid.New()
	defer func() {
		_ = adminDB.Exec("DELETE FROM skills WHERE id = ?", skillID).Error
		_ = adminDB.Exec("DELETE FROM mcps WHERE id = ?", mcpID).Error
		_ = adminDB.Exec("DELETE FROM users WHERE id = ?", actor.ID).Error
	}()

	skill, err := repository.CreateSkill(ctx, actor.ID, biz.SkillTemplate{
		ID: skillID, Name: "skill_" + suffix, Version: 1, Description: "test", PromptTemplate: "Summarize {{input}}",
		Parameters: map[string]any{"type": "object"}, Status: biz.SkillDraft,
	}, catalogEvent(actor.ID, "skill.created", "skill", skillID))
	if err != nil {
		t.Fatal(err)
	}
	if skill.Parameters["type"] != "object" {
		t.Fatalf("skill parameters were not persisted: %#v", skill.Parameters)
	}
	if _, err := repository.UpdateSkill(ctx, actor.ID, biz.SkillTemplate{
		ID: skill.ID, Name: skill.Name, Version: skill.Version, PromptTemplate: "Updated {{input}}", Status: biz.SkillPublished,
	}, catalogEvent(actor.ID, "skill.updated", "skill", skill.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.GetSkill(ctx, skill.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ListSkills(ctx); err != nil {
		t.Fatal(err)
	}

	mcp, err := repository.CreateMCP(ctx, actor.ID, biz.MCP{
		ID: mcpID, Name: "mcp_" + suffix, Endpoint: "https://tools.example.com/mcp", AuthType: "bearer", RiskLevel: "medium", Status: string(biz.MCPEnabled),
	}, &biz.ProviderCredential{Encrypted: []byte("encrypted-mcp-token"), KeyVersion: "v1"}, catalogEvent(actor.ID, "mcp.created", "mcp", mcpID))
	if err != nil {
		t.Fatal(err)
	}
	if mcp.KeyVersion != "v1" {
		t.Fatalf("MCP key version was not exposed as metadata: %#v", mcp)
	}
	var stored struct {
		Credentials []byte
	}
	if err := adminDB.Raw("SELECT auth_credentials_encrypted AS credentials FROM mcps WHERE id = ?", mcp.ID).Scan(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if string(stored.Credentials) != "encrypted-mcp-token" {
		t.Fatalf("unexpected stored MCP credential: %q", stored.Credentials)
	}
	if _, err := repository.GetMCP(ctx, mcp.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ListMCPs(ctx); err != nil {
		t.Fatal(err)
	}
	if err := repository.DeleteMCP(ctx, actor.ID, mcp.ID, catalogEvent(actor.ID, "mcp.deleted", "mcp", mcp.ID)); err != nil {
		t.Fatal(err)
	}
	if err := repository.DeleteSkill(ctx, actor.ID, skill.ID, catalogEvent(actor.ID, "skill.deleted", "skill", skill.ID)); err != nil {
		t.Fatal(err)
	}
}
