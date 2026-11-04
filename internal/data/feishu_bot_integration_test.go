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

// TestFeishuBotRepositoryBindsExclusivelyAndReleasesOnUnpublish 验证机器人唯一绑定、用户视图脱敏与状态变更解绑。
func TestFeishuBotRepositoryBindsExclusivelyAndReleasesOnUnpublish(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	users := NewUserRepository(appDB, authDB)
	owner, err := users.CreateUser(ctx, biz.User{Username: "bot_owner_" + suffix, PasswordHash: "hash", Role: biz.RoleUser, Status: biz.UserStatusActive, CredentialVersion: 1}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	other, err := users.CreateUser(ctx, biz.User{Username: "bot_other_" + suffix, PasswordHash: "hash", Role: biz.RoleUser, Status: biz.UserStatusActive, CredentialVersion: 1}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	agentID, secondAgentID := uuid.New(), uuid.New()
	insertAgent := func(id uuid.UUID, name string) {
		t.Helper()
		err := adminDB.Exec(`INSERT INTO agents (id, user_id, name, description, mode, version, status, config_snapshot)
VALUES (?, ?, ?, '', 'react', 1, 'published', '{}'::jsonb)`, id, owner.ID, name).Error
		if err != nil {
			t.Fatal(err)
		}
	}
	insertAgent(agentID, "bot-agent-1")
	insertAgent(secondAgentID, "bot-agent-2")
	botID := uuid.New()
	defer func() {
		_ = adminDB.Exec("DELETE FROM feishu_bots WHERE id = ?", botID).Error
		_ = adminDB.Exec("DELETE FROM users WHERE id IN (?, ?)", owner.ID, other.ID).Error
	}()

	repo := NewFeishuBotRepository(appDB)
	bot, err := repo.CreateFeishuBot(ctx, owner.ID, biz.FeishuBot{
		ID: botID, Name: "integration-bot-" + suffix, AppID: "cli_" + suffix, BotOpenID: "ou_" + suffix, Status: biz.FeishuBotEnabled,
	}, biz.ProviderCredential{Encrypted: []byte("ciphertext"), KeyVersion: "v1"}, testOutboxEvent(owner.ID, "feishu_bot.created"))
	if err != nil {
		t.Fatal(err)
	}
	if bot.AppID != "cli_"+suffix || bot.Status != biz.FeishuBotEnabled {
		t.Fatalf("created bot = %#v", bot)
	}

	if err := repo.BindFeishuBot(ctx, biz.FeishuBotBinding{
		BotID: botID, UserID: owner.ID, AgentID: agentID, AgentName: "bot-agent-1", AgentVersion: 1,
	}, testOutboxEvent(owner.ID, "feishu_bot.bound")); err != nil {
		t.Fatal(err)
	}
	// 同一机器人不能被第二个助手绑定。
	if err := repo.BindFeishuBot(ctx, biz.FeishuBotBinding{
		BotID: botID, UserID: owner.ID, AgentID: secondAgentID, AgentName: "bot-agent-2", AgentVersion: 1,
	}, testOutboxEvent(owner.ID, "feishu_bot.bound")); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("duplicate bind error = %v", err)
	}

	// 所有者可见自己的绑定，其他用户只能看到占用状态。
	findBot := func(bots []biz.FeishuBot) (biz.FeishuBot, bool) {
		for _, item := range bots {
			if item.ID == botID {
				return item, true
			}
		}
		return biz.FeishuBot{}, false
	}
	ownerBots, err := repo.ListFeishuBotsForUser(ctx, owner.ID)
	ownerView, ok := findBot(ownerBots)
	if err != nil || !ok || ownerView.BoundAgentID == nil || *ownerView.BoundAgentID != agentID {
		t.Fatalf("owner bots = %#v, %v", ownerBots, err)
	}
	otherBots, err := repo.ListFeishuBotsForUser(ctx, other.ID)
	otherView, ok := findBot(otherBots)
	if err != nil || !ok || otherView.BoundAgentID != nil {
		t.Fatalf("other bots = %#v, %v", otherBots, err)
	}
	found, exists, err := repo.LookupFeishuBot(ctx, "cli_"+suffix)
	if err != nil || !exists || found.ID != botID {
		t.Fatalf("lookup bot = %#v, %v", found, err)
	}
	binding, exists, err := repo.FindFeishuBotBinding(ctx, botID)
	if err != nil || !exists || binding.AgentID != agentID || binding.UserID != owner.ID || binding.AgentStatus != biz.AgentPublished {
		t.Fatalf("binding = %#v, %v", binding, err)
	}

	// 取消发布会解除绑定，机器人重新可用。
	agents := NewAgentRepository(appDB)
	if err := agents.UpdateAgentStatus(ctx, owner.ID, agentID, biz.AgentPublished, biz.AgentDraft, true, testOutboxEvent(owner.ID, "agent.unpublished")); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := repo.FindFeishuBotBinding(ctx, botID); err != nil || exists {
		t.Fatalf("binding after unpublish exists=%t err=%v", exists, err)
	}
	available, err := repo.ListFeishuBotsForUser(ctx, owner.ID)
	availableView, ok := findBot(available)
	if err != nil || !ok || availableView.BoundUserID != nil {
		t.Fatalf("available bots = %#v, %v", available, err)
	}
}
