//go:build integration

package data

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// skillSynthesisIntegrationFixture 聚合真实 PostgreSQL 下的提炼链路依赖。
type skillSynthesisIntegrationFixture struct {
	adminDB          *gorm.DB
	appDB            *gorm.DB
	user             biz.User
	principal        biz.Principal
	policy           biz.ModelPolicy
	agentService     *biz.AgentService
	sessionService   *biz.SessionService
	sessionRepo      *SessionRepository
	userSkillRepo    *UserSkillRepository
	userSkillService *biz.UserSkillService
	bundleRepo       *SkillBundleRepository
	jobRepo          *JobRepository
}

// newSkillSynthesisIntegrationFixture 创建用户、已发布策略与真实仓储，并清理测试数据。
func newSkillSynthesisIntegrationFixture(t *testing.T) *skillSynthesisIntegrationFixture {
	t.Helper()
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	ctx := context.Background()
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	user, err := NewUserRepository(appDB, authDB).CreateUser(ctx, biz.User{
		Username: "skill_synthesis_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	catalog := NewCatalogRepository(appDB)
	providerID := uuid.New()
	provider, err := catalog.CreateProvider(ctx, user.ID, biz.Provider{
		ID: providerID, Name: "skill_provider_" + suffix, ProviderType: "openai_compatible",
		BaseURL: "https://models.example.com", Status: biz.ProviderEnabled,
	}, biz.ProviderCredential{Encrypted: []byte("ciphertext"), KeyVersion: "v1"},
		catalogEvent(user.ID, "provider.created", "llm_provider", providerID))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	modelID := uuid.New()
	model, err := catalog.CreateModel(ctx, user.ID, biz.Model{
		ID: modelID, ProviderID: provider.ID, RemoteModelName: "generation", Kind: biz.ModelKindChat,
		ContextWindowTokens: biz.MinGenerationContextTokens, MaxOutputTokens: 8192,
		Status: biz.ModelEnabled, VerificationSource: "integration-test", VerifiedAt: &now,
	}, catalogEvent(user.ID, "model.created", "llm_model", modelID))
	if err != nil {
		t.Fatal(err)
	}
	policyID := uuid.New()
	policy, err := catalog.CreatePolicy(ctx, user.ID, biz.ModelPolicy{
		ID: policyID, Name: "skill_policy_" + suffix, Version: 1,
		MinContextTokens: biz.MinGenerationContextTokens, Status: biz.PolicyDraft,
		Candidates: []biz.PolicyCandidate{{ModelID: model.ID, Priority: 1}},
	}, catalogEvent(user.ID, "model_policy.created", "model_policy", policyID))
	if err != nil {
		t.Fatal(err)
	}
	policy.Status = biz.PolicyPublished
	policy, err = catalog.UpdatePolicy(ctx, user.ID, policy, catalogEvent(user.ID, "model_policy.published", "model_policy", policyID))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := adminDB.Exec("DELETE FROM users WHERE id = ?", user.ID).Error; err != nil {
			t.Logf("cleanup user %s failed: %v", user.ID, err)
		}
		if err := adminDB.Exec("DELETE FROM model_policies WHERE id = ?", policyID).Error; err != nil {
			t.Logf("cleanup policy %s failed: %v", policyID, err)
		}
		if err := adminDB.Exec("DELETE FROM llm_models WHERE id = ?", model.ID).Error; err != nil {
			t.Logf("cleanup model %s failed: %v", model.ID, err)
		}
		if err := adminDB.Exec("DELETE FROM llm_providers WHERE id = ?", providerID).Error; err != nil {
			t.Logf("cleanup provider %s failed: %v", providerID, err)
		}
	})

	sessionRepo := NewSessionRepository(appDB)
	sessionService, err := biz.NewSessionService(sessionRepo, biz.QuotaLimits{})
	if err != nil {
		t.Fatal(err)
	}
	agentService, err := biz.NewAgentService(NewAgentRepository(appDB), biz.QuotaLimits{})
	if err != nil {
		t.Fatal(err)
	}
	userSkillRepo := NewUserSkillRepository(appDB)
	bundleRepo := NewSkillBundleRepository(appDB)
	userSkillService, err := biz.NewUserSkillService(userSkillRepo)
	if err != nil {
		t.Fatal(err)
	}
	userSkillService.WithBundles(bundleRepo)
	if _, err := userSkillRepo.UpsertUserSkillSynthesisSettings(ctx, biz.UserSkillSynthesisSettings{
		UserID: user.ID, Enabled: true,
	}, testOutboxEvent(user.ID, "test.skill_synthesis_settings")); err != nil {
		t.Fatal(err)
	}
	return &skillSynthesisIntegrationFixture{
		adminDB: adminDB, appDB: appDB, user: user,
		principal: biz.Principal{UserID: user.ID, Role: biz.RoleUser},
		policy:    policy, agentService: agentService,
		sessionService: sessionService, sessionRepo: sessionRepo,
		userSkillRepo: userSkillRepo, userSkillService: userSkillService,
		bundleRepo: bundleRepo, jobRepo: NewJobRepository(appDB),
	}
}

// createPublishedAgent 创建并发布一个绑定指定用户技能的助手版本。
func (f *skillSynthesisIntegrationFixture) createPublishedAgent(t *testing.T, name string, skillIDs []uuid.UUID) biz.Agent {
	t.Helper()
	ctx := context.Background()
	agent, err := f.agentService.Create(ctx, f.principal, biz.Agent{
		Name: name, Mode: biz.AgentModeReact,
		ModelPolicyID: &f.policy.ID, ModelPolicyVersion: &f.policy.Version,
		UserSkillIDs: skillIDs,
	})
	if err != nil {
		t.Fatal(err)
	}
	published, err := f.agentService.Publish(ctx, f.principal, agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	return published
}

// createSessionWithMessages 创建绑定助手的会话并写入两条用户消息。
func (f *skillSynthesisIntegrationFixture) createSessionWithMessages(t *testing.T, agentID uuid.UUID, title string, contents ...string) biz.Session {
	t.Helper()
	ctx := context.Background()
	session, err := f.sessionService.Create(ctx, f.principal, biz.Session{AgentID: &agentID, Title: title})
	if err != nil {
		t.Fatal(err)
	}
	for _, content := range contents {
		if _, err := f.sessionService.AppendMessage(ctx, f.principal, biz.Message{SessionID: session.ID, Content: content}); err != nil {
			t.Fatal(err)
		}
	}
	return session
}

// createPublishedUserSkill 创建并发布用户技能；auto 为 true 时模拟每日提炼来源。
func (f *skillSynthesisIntegrationFixture) createPublishedUserSkill(t *testing.T, name string, auto bool) biz.UserSkill {
	t.Helper()
	ctx := context.Background()
	var (
		skill biz.UserSkill
		err   error
	)
	if auto {
		skill, err = f.userSkillService.CreateAuto(ctx, f.user.ID, biz.UserSkill{
			Name:             name,
			Description:      "把重复出现的周报整理流程沉淀为可复用技能，适用于每周固定汇报场景。",
			PromptTemplate:   integrationWeeklyReportPrompt(""),
			Parameters:       map[string]any{},
			SourceSessionIDs: []uuid.UUID{uuid.New()},
			Confidence:       0.7,
		})
	} else {
		skill, err = f.userSkillService.Create(ctx, f.principal, biz.UserSkill{
			Name:           name,
			Description:    "手动维护的周报技能，用于验证发布继承只包含自动技能。",
			PromptTemplate: integrationWeeklyReportPrompt(""),
			Parameters:     map[string]any{},
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	published, err := f.userSkillService.Publish(ctx, f.principal, skill.ID)
	if err != nil {
		t.Fatal(err)
	}
	return published
}

// runSkillSynthesis 入队并执行一次技能提炼任务。
func (f *skillSynthesisIntegrationFixture) runSkillSynthesis(t *testing.T, extractor biz.SkillExtractor, now time.Time, requestID string) {
	t.Helper()
	ctx := context.Background()
	task := biz.SkillSynthesisTask{UserID: f.user.ID, Date: now.Format("2006-01-02"), RequestID: requestID}
	if err := f.jobRepo.EnqueueSkillSynthesis(ctx, task, now); err != nil {
		t.Fatal(err)
	}
	worker, err := biz.NewSkillSynthesisWorker(biz.SkillSynthesisWorkerDependencies{
		Jobs: f.jobRepo, Settings: f.userSkillRepo, Sessions: f.sessionRepo,
		UserSkills: f.userSkillService, Extractor: extractor, Runs: f.userSkillRepo,
		Agents: f.agentService, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Clock: func() time.Time { return now },
	}, biz.SkillSynthesisConfig{
		Enabled: true, MinConfidence: 0.5, MaxSessionsPerUser: 10,
		MaxMessagesPerSession: 50, MaxSkillsPerUserPerDay: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	processed, err := worker.ProcessOne(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !processed {
		t.Fatal("expected a skill synthesis job to be processed")
	}
}

// integrationSkillExtractor 返回预设候选，并按实际会话消息补齐证据。
type integrationSkillExtractor struct {
	candidate biz.SkillCandidate
	calls     int
}

func (e *integrationSkillExtractor) Extract(_ context.Context, _ uuid.UUID, _ biz.SkillSynthesisTarget,
	sessions []biz.Session, messagesBySession map[uuid.UUID][]biz.Message, _ []biz.SkillDigest) (biz.SkillCandidate, error) {
	e.calls++
	candidate := e.candidate
	for _, session := range sessions {
		history := messagesBySession[session.ID]
		if len(history) < 2 {
			continue
		}
		title := strings.TrimSpace(session.Title)
		if title == "" {
			title = "(无标题)"
		}
		candidate.Evidence = []biz.SkillEvidence{
			{Session: title, Quote: history[0].Content},
			{Session: title, Quote: history[1].Content},
		}
		break
	}
	return candidate, nil
}

// integrationWeeklyReportPrompt 构造包含标准标题的候选正文。
func integrationWeeklyReportPrompt(extraStep string) string {
	prompt := "# weekly-report\n\n## 何时使用\n每周需要整理工作周报时使用。\n\n## 步骤\n1. 收集本周进展。\n"
	if extraStep != "" {
		prompt += extraStep + "\n"
	}
	return prompt + "\n## 输出格式\nMarkdown。\n\n## 约束\n不得编造。"
}

// TestSkillSynthesisAutoMountIntegration 验证每日提炼会创建并发布技能、生成助手新版本并回绑来源会话。
func TestSkillSynthesisAutoMountIntegration(t *testing.T) {
	fixture := newSkillSynthesisIntegrationFixture(t)
	ctx := context.Background()
	agent := fixture.createPublishedAgent(t, "synthesis-agent", nil)
	draftV2, err := fixture.agentService.CreateVersion(ctx, fixture.principal, agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	agentV2, err := fixture.agentService.Publish(ctx, fixture.principal, draftV2.ID)
	if err != nil {
		t.Fatal(err)
	}
	session := fixture.createSessionWithMessages(t, agent.ID, "周报整理",
		"请帮我整理一份本周工作周报，覆盖项目进展、风险和下周计划。",
		"这是本周的项目进展、风险与下周计划素材，请按固定格式汇总。",
	)
	// 任务可用时间放到未来一小时，避免正在运行的实例抢先消费测试任务。
	now := time.Now().UTC().Add(time.Hour)
	extractor := &integrationSkillExtractor{candidate: biz.SkillCandidate{
		ShouldCreate: true, Confidence: 0.9, Name: "weekly-report",
		Description:    "把重复出现的周报整理流程沉淀为可复用技能，适用于每周固定汇报场景。",
		WhenToUse:      "当需要按固定格式整理周报并汇总风险时使用。",
		PromptTemplate: integrationWeeklyReportPrompt("2. 汇总风险项与下周计划。"),
		Parameters:     map[string]any{},
	}}
	fixture.runSkillSynthesis(t, extractor, now, "")
	if extractor.calls != 1 {
		t.Fatalf("extractor calls = %d", extractor.calls)
	}

	skills, err := fixture.userSkillRepo.ListUserSkills(ctx, fixture.user.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 {
		t.Fatalf("expected 1 auto skill, got %d", len(skills))
	}
	skill := skills[0]
	if skill.Status != biz.UserSkillPublished || skill.Origin != biz.UserSkillOriginAuto {
		t.Fatalf("unexpected skill lifecycle: %#v", skill)
	}
	if skill.Bundle.Format != biz.SkillBundleFormat || skill.Bundle.FileCount == 0 {
		t.Fatalf("auto skill bundle was not persisted: %#v", skill.Bundle)
	}
	file, err := fixture.bundleRepo.GetUserSkillFile(ctx, fixture.user.ID, skill.ID, "SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(file.Content), "汇总风险项与下周计划") {
		t.Fatalf("SKILL.md missing generated content: %s", file.Content)
	}

	for _, current := range []biz.Agent{agent, agentV2} {
		stored, err := fixture.agentService.Get(ctx, fixture.principal, current.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Version != current.Version || stored.ID != current.ID {
			t.Fatalf("agent version changed during attach: %#v", stored)
		}
		if !containsIntegrationUUID(stored.UserSkillIDs, skill.ID) {
			t.Fatalf("agent %s missing skill binding: %#v", stored.ID, stored.UserSkillIDs)
		}
		var bindingCount int64
		if err := fixture.adminDB.Table("agent_user_skills").
			Where("agent_id = ? AND user_skill_id = ? AND user_id = ?", current.ID, skill.ID, fixture.user.ID).
			Count(&bindingCount).Error; err != nil {
			t.Fatal(err)
		}
		if bindingCount != 1 {
			t.Fatalf("agent_user_skills binding count = %d", bindingCount)
		}
		var publishedSnapshot struct {
			ConfigSnapshot []byte
		}
		if err := fixture.adminDB.Table("agents").Select("config_snapshot").
			Where("id = ?", current.ID).Scan(&publishedSnapshot).Error; err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(publishedSnapshot.ConfigSnapshot), skill.ID.String()) ||
			!strings.Contains(string(publishedSnapshot.ConfigSnapshot), "汇总风险项与下周计划") {
			t.Fatalf("published snapshot does not contain skill content: %s", publishedSnapshot.ConfigSnapshot)
		}
	}

	sessionAfter, err := fixture.sessionService.Get(ctx, fixture.principal, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sessionAfter.AgentID == nil || *sessionAfter.AgentID != agent.ID {
		t.Fatalf("session agent binding changed: %#v", sessionAfter.AgentID)
	}

	runs, err := fixture.userSkillRepo.ListSkillSynthesisRuns(ctx, fixture.user.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Outcome != "created" || runs[0].SkillID == nil || *runs[0].SkillID != skill.ID {
		t.Fatalf("synthesis run = %#v", runs)
	}
	if len(runs[0].Results) != 1 || runs[0].Results[0].AgentName != agent.Name ||
		runs[0].Results[0].Outcome != "created" || runs[0].Results[0].SkillID == nil ||
		*runs[0].Results[0].SkillID != skill.ID {
		t.Fatalf("synthesis run results = %#v", runs[0].Results)
	}
	watermark, found, err := fixture.userSkillRepo.GetSkillSynthesisWatermark(ctx, fixture.user.ID)
	if err != nil || !found || watermark.IsZero() {
		t.Fatalf("watermark = %v, found=%v, err=%v", watermark, found, err)
	}

	// 快照历史应保存技能绑定前的版本，并支持恢复。
	revisions, err := fixture.agentService.ListSnapshotRevisions(ctx, fixture.principal, agent.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(revisions) != 1 {
		t.Fatalf("expected 1 snapshot revision, got %d", len(revisions))
	}
	restored, err := fixture.agentService.RestoreSnapshotRevision(ctx, fixture.principal, agent.ID, revisions[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	// 恢复的是绑定前的快照：内容与绑定一起回滚，并记录显式移除标记。
	if containsIntegrationUUID(restored.UserSkillIDs, skill.ID) {
		t.Fatalf("snapshot restore should reconcile bindings: %#v", restored.UserSkillIDs)
	}
	optOuts, err := NewAgentRepository(fixture.appDB).ListUserSkillOptOuts(ctx, fixture.user.ID, agent.ID)
	if err != nil || !containsIntegrationUUID(optOuts, skill.ID) {
		t.Fatalf("snapshot restore should record opt-out: %v, %v", optOuts, err)
	}
	var restoredSnapshot struct {
		ConfigSnapshot []byte
	}
	if err := fixture.adminDB.Table("agents").Select("config_snapshot").
		Where("id = ?", agent.ID).Scan(&restoredSnapshot).Error; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(restoredSnapshot.ConfigSnapshot), "汇总风险项与下周计划") {
		t.Fatalf("snapshot restore did not roll back skill content: %s", restoredSnapshot.ConfigSnapshot)
	}
}

// TestSkillSynthesisOptimizeMountedSkillIntegration 验证重复候选会原地优化已挂载技能，并保持挂载幂等。
func TestSkillSynthesisOptimizeMountedSkillIntegration(t *testing.T) {
	fixture := newSkillSynthesisIntegrationFixture(t)
	ctx := context.Background()
	skill, err := fixture.userSkillService.CreateAuto(ctx, fixture.user.ID, biz.UserSkill{
		Name:           "weekly-report",
		Description:    "把重复出现的周报整理流程沉淀为可复用技能，适用于每周固定汇报场景。",
		PromptTemplate: integrationWeeklyReportPrompt(""),
		Parameters:     map[string]any{},
		SourceSessionIDs: []uuid.UUID{
			uuid.New(),
		},
		Confidence: 0.7,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.userSkillService.Publish(ctx, fixture.principal, skill.ID); err != nil {
		t.Fatal(err)
	}
	agent := fixture.createPublishedAgent(t, "synthesis-agent", []uuid.UUID{skill.ID})
	firstSession := fixture.createSessionWithMessages(t, agent.ID, "周报优化",
		"请帮我整理一份本周工作周报，并补充风险项和下周计划。",
		"这是本周的项目进展、风险与下周计划素材，请合并到周报并保留原有步骤。",
	)
	// 任务可用时间放到未来一小时，避免正在运行的实例抢先消费测试任务。
	now := time.Now().UTC().Add(time.Hour)
	extractor := &integrationSkillExtractor{candidate: biz.SkillCandidate{
		ShouldCreate: true, Confidence: 0.9, Name: "weekly-report",
		Description:    "把周报整理与风险汇总流程沉淀为可复用技能，适用于每周固定汇报场景。",
		WhenToUse:      "当需要按固定格式整理周报并汇总风险时使用。",
		PromptTemplate: integrationWeeklyReportPrompt("2. 汇总风险项与下周计划。"),
		Parameters:     map[string]any{},
	}}
	fixture.runSkillSynthesis(t, extractor, now, "")

	skills, err := fixture.userSkillRepo.ListUserSkills(ctx, fixture.user.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 || skills[0].ID != skill.ID {
		t.Fatalf("optimization created a new skill: %#v", skills)
	}
	optimized := skills[0]
	if !strings.Contains(optimized.PromptTemplate, "汇总风险项与下周计划") || optimized.Confidence != 0.9 {
		t.Fatalf("skill was not optimized in place: %#v", optimized)
	}
	if len(optimized.SourceSessionIDs) != 2 || !containsIntegrationUUID(optimized.SourceSessionIDs, firstSession.ID) {
		t.Fatalf("source sessions = %#v", optimized.SourceSessionIDs)
	}
	file, err := fixture.bundleRepo.GetUserSkillFile(ctx, fixture.user.ID, skill.ID, "SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(file.Content), "汇总风险项与下周计划") {
		t.Fatalf("SKILL.md was not refreshed: %s", file.Content)
	}
	var revision struct {
		PromptTemplate string
	}
	if err := fixture.adminDB.Table("user_skill_revisions").
		Select("prompt_template").
		Where("user_id = ? AND skill_id = ?", fixture.user.ID, skill.ID).
		Order("created_at DESC").First(&revision).Error; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(revision.PromptTemplate, "1. 收集本周进展。") {
		t.Fatalf("previous skill content was not preserved: %s", revision.PromptTemplate)
	}

	stored, err := fixture.agentService.Get(ctx, fixture.principal, agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Version != agent.Version || !containsIntegrationUUID(stored.UserSkillIDs, skill.ID) {
		t.Fatalf("agent was not refreshed in place: %#v", stored)
	}
	var snapshot struct {
		ConfigSnapshot []byte
	}
	if err := fixture.adminDB.Table("agents").Select("config_snapshot").
		Where("id = ?", agent.ID).Scan(&snapshot).Error; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(snapshot.ConfigSnapshot), "汇总风险项与下周计划") {
		t.Fatalf("agent snapshot was not refreshed: %s", snapshot.ConfigSnapshot)
	}
	rebound, err := fixture.sessionService.Get(ctx, fixture.principal, firstSession.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rebound.AgentID == nil || *rebound.AgentID != agent.ID {
		t.Fatalf("session agent binding changed: %#v", rebound.AgentID)
	}

	// 第二批相同内容再次提炼：不应新增技能或助手版本，也不应改写技能内容。
	secondSession := fixture.createSessionWithMessages(t, agent.ID, "周报重复",
		"请帮我再整理一份本周工作周报，并补充风险项和下周计划。",
		"这是同一格式的本周项目进展、风险与下周计划素材，请按原有周报步骤汇总。",
	)
	secondRunAt := time.Now().UTC().Add(2 * time.Hour)
	fixture.runSkillSynthesis(t, extractor, secondRunAt, "manual:idempotent")

	skills, err = fixture.userSkillRepo.ListUserSkills(ctx, fixture.user.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 || skills[0].ID != skill.ID {
		t.Fatalf("idempotent run changed skills: %#v", skills)
	}
	var revisionCount int64
	if err := fixture.adminDB.Table("user_skill_revisions").
		Where("user_id = ? AND skill_id = ?", fixture.user.ID, skill.ID).
		Count(&revisionCount).Error; err != nil {
		t.Fatal(err)
	}
	if revisionCount != 1 {
		t.Fatalf("idempotent run added history rows: %d", revisionCount)
	}
	var versionCount int64
	if err := fixture.adminDB.Table("agents").
		Where("user_id = ? AND name = ?", fixture.user.ID, agent.Name).
		Count(&versionCount).Error; err != nil {
		t.Fatal(err)
	}
	if versionCount != 1 {
		t.Fatalf("idempotent run created extra agent versions: %d", versionCount)
	}
	secondAfter, err := fixture.sessionService.Get(ctx, fixture.principal, secondSession.ID)
	if err != nil {
		t.Fatal(err)
	}
	if secondAfter.AgentID == nil || *secondAfter.AgentID != agent.ID {
		t.Fatalf("second session agent binding changed: %#v", secondAfter.AgentID)
	}
	runs, err := fixture.userSkillRepo.ListSkillSynthesisRuns(ctx, fixture.user.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Outcome != "duplicate_skill" {
		t.Fatalf("idempotent run outcome = %#v", runs)
	}
}

// TestSkillSynthesisRespectsUserEditedSkillIntegration 验证用户手动编辑过的技能不会被后续提炼覆盖。
func TestSkillSynthesisRespectsUserEditedSkillIntegration(t *testing.T) {
	fixture := newSkillSynthesisIntegrationFixture(t)
	ctx := context.Background()
	skill, err := fixture.userSkillService.CreateAuto(ctx, fixture.user.ID, biz.UserSkill{
		Name:             "weekly-report",
		Description:      "把重复出现的周报整理流程沉淀为可复用技能，适用于每周固定汇报场景。",
		PromptTemplate:   integrationWeeklyReportPrompt(""),
		Parameters:       map[string]any{},
		SourceSessionIDs: []uuid.UUID{uuid.New()},
		Confidence:       0.7,
	})
	if err != nil {
		t.Fatal(err)
	}
	editedPrompt := integrationWeeklyReportPrompt("2. 用户手工补充的检查项。")
	edited, err := fixture.userSkillService.Update(ctx, fixture.principal, biz.UserSkill{
		ID: skill.ID, Description: skill.Description, PromptTemplate: editedPrompt, Parameters: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if edited.UserEditedAt == nil {
		t.Fatal("manual edit did not mark user_edited_at")
	}
	if _, err := fixture.userSkillService.Publish(ctx, fixture.principal, skill.ID); err != nil {
		t.Fatal(err)
	}
	agent := fixture.createPublishedAgent(t, "synthesis-agent", []uuid.UUID{skill.ID})
	fixture.createSessionWithMessages(t, agent.ID, "周报会话",
		"请帮我整理一份本周工作周报，并补充风险项和下周计划。",
		"这是本周的项目进展、风险与下周计划素材，请合并到周报。",
	)
	now := time.Now().UTC().Add(time.Hour)
	extractor := &integrationSkillExtractor{candidate: biz.SkillCandidate{
		ShouldCreate: true, Confidence: 0.95, Name: "weekly-report",
		Description:    "把周报整理与风险汇总流程沉淀为可复用技能，适用于每周固定汇报场景。",
		WhenToUse:      "当需要按固定格式整理周报并汇总风险时使用。",
		PromptTemplate: integrationWeeklyReportPrompt("2. 自动提炼的新步骤。"),
		Parameters:     map[string]any{},
	}}
	fixture.runSkillSynthesis(t, extractor, now, "")

	skills, err := fixture.userSkillRepo.ListUserSkills(ctx, fixture.user.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 || !strings.Contains(skills[0].PromptTemplate, "用户手工补充的检查项") {
		t.Fatalf("user-edited skill was overwritten: %#v", skills)
	}
	if strings.Contains(skills[0].PromptTemplate, "自动提炼的新步骤") {
		t.Fatalf("automatic candidate replaced user content: %#v", skills[0])
	}
	runs, err := fixture.userSkillRepo.ListSkillSynthesisRuns(ctx, fixture.user.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Outcome != "duplicate_skill" ||
		!strings.Contains(runs[0].Detail, "手动修改") {
		t.Fatalf("run = %#v", runs)
	}
}

// TestSkillSynthesisRefreshesAllBoundAgentsIntegration 验证技能优化后所有绑定助手快照同步刷新且版本不变。
func TestSkillSynthesisRefreshesAllBoundAgentsIntegration(t *testing.T) {
	fixture := newSkillSynthesisIntegrationFixture(t)
	ctx := context.Background()
	skill, err := fixture.userSkillService.CreateAuto(ctx, fixture.user.ID, biz.UserSkill{
		Name:             "weekly-report",
		Description:      "把重复出现的周报整理流程沉淀为可复用技能，适用于每周固定汇报场景。",
		PromptTemplate:   integrationWeeklyReportPrompt(""),
		Parameters:       map[string]any{},
		SourceSessionIDs: []uuid.UUID{uuid.New()},
		Confidence:       0.7,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.userSkillService.Publish(ctx, fixture.principal, skill.ID); err != nil {
		t.Fatal(err)
	}
	agentA := fixture.createPublishedAgent(t, "synthesis-agent-a", []uuid.UUID{skill.ID})
	agentB := fixture.createPublishedAgent(t, "synthesis-agent-b", []uuid.UUID{skill.ID})
	fixture.createSessionWithMessages(t, agentA.ID, "周报优化",
		"请帮我整理一份本周工作周报，并补充风险项和下周计划。",
		"这是本周的项目进展、风险与下周计划素材，请合并到周报并保留原有步骤。",
	)
	now := time.Now().UTC().Add(time.Hour)
	extractor := &integrationSkillExtractor{candidate: biz.SkillCandidate{
		ShouldCreate: true, Confidence: 0.9, Name: "weekly-report",
		Description:    "把周报整理与风险汇总流程沉淀为可复用技能，适用于每周固定汇报场景。",
		WhenToUse:      "当需要按固定格式整理周报并汇总风险时使用。",
		PromptTemplate: integrationWeeklyReportPrompt("2. 汇总风险项与下周计划。"),
		Parameters:     map[string]any{},
	}}
	fixture.runSkillSynthesis(t, extractor, now, "")

	var agentCount int64
	if err := fixture.adminDB.Table("agents").Where("user_id = ?", fixture.user.ID).Count(&agentCount).Error; err != nil {
		t.Fatal(err)
	}
	if agentCount != 2 {
		t.Fatalf("agent versions changed: %d", agentCount)
	}
	for _, agent := range []biz.Agent{agentA, agentB} {
		stored, err := fixture.agentService.Get(ctx, fixture.principal, agent.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Version != agent.Version || !containsIntegrationUUID(stored.UserSkillIDs, skill.ID) {
			t.Fatalf("agent %s was not refreshed in place: %#v", agent.Name, stored)
		}
		var snapshot struct {
			ConfigSnapshot []byte
		}
		if err := fixture.adminDB.Table("agents").Select("config_snapshot").
			Where("id = ?", agent.ID).Scan(&snapshot).Error; err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(snapshot.ConfigSnapshot), "汇总风险项与下周计划") {
			t.Fatalf("agent %s snapshot was not refreshed: %s", agent.Name, snapshot.ConfigSnapshot)
		}
	}
}

// TestAgentDraftUpdateRejectsStaleAfterAutoAttachIntegration 验证草稿被自动挂载后，基于旧 updated_at 的保存会被拒绝，
// 避免把刚绑定上的技能覆盖掉。
func TestAgentDraftUpdateRejectsStaleAfterAutoAttachIntegration(t *testing.T) {
	fixture := newSkillSynthesisIntegrationFixture(t)
	ctx := context.Background()
	draft, err := fixture.agentService.Create(ctx, fixture.principal, biz.Agent{
		Name: "draft-auto-attach-" + strings.ReplaceAll(uuid.NewString(), "-", ""), Mode: biz.AgentModeReact,
		ModelPolicyID: &fixture.policy.ID, ModelPolicyVersion: &fixture.policy.Version,
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded := draft
	skill, err := fixture.userSkillService.CreateAuto(ctx, fixture.user.ID, biz.UserSkill{
		Name:             "weekly-report",
		Description:      "把重复出现的周报整理流程沉淀为可复用技能，适用于每周固定汇报场景。",
		PromptTemplate:   integrationWeeklyReportPrompt(""),
		Parameters:       map[string]any{},
		SourceSessionIDs: []uuid.UUID{uuid.New()},
		Confidence:       0.7,
	})
	if err != nil {
		t.Fatal(err)
	}
	publishedSkill, err := fixture.userSkillService.Publish(ctx, fixture.principal, skill.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.agentService.AttachUserSkillToAgentFamily(ctx, fixture.principal, draft.ID, publishedSkill); err != nil {
		t.Fatal(err)
	}
	stored, err := fixture.agentService.Get(ctx, fixture.principal, draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !containsIntegrationUUID(stored.UserSkillIDs, skill.ID) {
		t.Fatalf("draft was not attached: %#v", stored.UserSkillIDs)
	}
	if stored.UpdatedAt.Equal(loaded.UpdatedAt) {
		t.Fatalf("draft updated_at was not advanced after attach: %s", stored.UpdatedAt)
	}

	stale := loaded
	stale.Description = "edited from a stale page"
	if _, err := fixture.agentService.Update(ctx, fixture.principal, stale); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("stale update error = %v, want conflict", err)
	}
	// 仓储层同样以 updated_at 作为 UPDATE 条件，消除校验与写入之间的竞态窗口。
	if _, err := NewAgentRepository(fixture.appDB).UpdateAgent(ctx, stale,
		testOutboxEvent(fixture.user.ID, "test.agent_stale_update")); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("repo stale update error = %v, want conflict", err)
	}
	fresh := loaded
	fresh.Description = "edited after reload"
	fresh.UpdatedAt = stored.UpdatedAt
	fresh.UserSkillIDs = stored.UserSkillIDs
	updated, err := fixture.agentService.Update(ctx, fixture.principal, fresh)
	if err != nil {
		t.Fatal(err)
	}
	if !containsIntegrationUUID(updated.UserSkillIDs, skill.ID) {
		t.Fatalf("binding was lost on update: %#v", updated.UserSkillIDs)
	}
}

// TestAgentPublishInheritsFamilyAutoUserSkillIntegration 验证草稿创建后发生的自动挂载会在发布时补齐，
// 且只继承自动技能，不继承手动技能。
func TestAgentPublishInheritsFamilyAutoUserSkillIntegration(t *testing.T) {
	fixture := newSkillSynthesisIntegrationFixture(t)
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	v1 := fixture.createPublishedAgent(t, "inherit-agent-"+suffix, nil)
	v2, err := fixture.agentService.CreateVersion(ctx, fixture.principal, v1.ID)
	if err != nil {
		t.Fatal(err)
	}
	autoSkill := fixture.createPublishedUserSkill(t, "weekly-report-auto", true)
	manualSkill := fixture.createPublishedUserSkill(t, "weekly-report-manual", false)
	for _, skill := range []biz.UserSkill{autoSkill, manualSkill} {
		if _, err := fixture.agentService.AttachUserSkillToAgentFamily(ctx, fixture.principal, v1.ID, skill); err != nil {
			t.Fatal(err)
		}
	}
	published, err := fixture.agentService.Publish(ctx, fixture.principal, v2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !containsIntegrationUUID(published.UserSkillIDs, autoSkill.ID) {
		t.Fatalf("auto skill was not inherited on publish: %#v", published.UserSkillIDs)
	}
	if containsIntegrationUUID(published.UserSkillIDs, manualSkill.ID) {
		t.Fatalf("manual skill must not be inherited: %#v", published.UserSkillIDs)
	}
	var snapshot struct {
		ConfigSnapshot []byte
	}
	if err := fixture.adminDB.Table("agents").Select("config_snapshot").
		Where("id = ?", published.ID).Scan(&snapshot).Error; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(snapshot.ConfigSnapshot), "weekly-report-auto") {
		t.Fatalf("inherited skill missing from snapshot: %s", snapshot.ConfigSnapshot)
	}
}

// TestAgentUserSkillOptOutLifecycleIntegration 验证按版本移除自动技能后，
// 发布继承与后续自动挂载都不会加回，重新添加则清除移除标记。
func TestAgentUserSkillOptOutLifecycleIntegration(t *testing.T) {
	fixture := newSkillSynthesisIntegrationFixture(t)
	ctx := context.Background()
	repo := NewAgentRepository(fixture.appDB)
	name := "optout-agent-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	autoSkill := fixture.createPublishedUserSkill(t, "weekly-report", true)
	v1 := fixture.createPublishedAgent(t, name, nil)
	if _, err := fixture.agentService.AttachUserSkillToAgentFamily(ctx, fixture.principal, v1.ID, autoSkill); err != nil {
		t.Fatal(err)
	}
	v2, err := fixture.agentService.CreateVersion(ctx, fixture.principal, v1.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 用户在 v2 草稿中显式移除该技能。
	v2Current, err := fixture.agentService.Get(ctx, fixture.principal, v2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !containsIntegrationUUID(v2Current.UserSkillIDs, autoSkill.ID) {
		t.Fatalf("draft did not copy the auto skill binding: %#v", v2Current.UserSkillIDs)
	}
	if _, err := fixture.agentService.Update(ctx, fixture.principal, biz.Agent{
		ID: v2.ID, Description: v2Current.Description, Mode: v2Current.Mode,
		ModelPolicyID: v2Current.ModelPolicyID, ModelPolicyVersion: v2Current.ModelPolicyVersion,
		UserSkillIDs: nil, UpdatedAt: v2Current.UpdatedAt,
	}); err != nil {
		t.Fatal(err)
	}
	optOuts, err := repo.ListUserSkillOptOuts(ctx, fixture.user.ID, v2.ID)
	if err != nil || !containsIntegrationUUID(optOuts, autoSkill.ID) {
		t.Fatalf("remove should record opt-out: %v, %v", optOuts, err)
	}

	// 新版本沿用移除标记；发布时不会被同家族自动技能继承覆盖。
	v3, err := fixture.agentService.CreateVersion(ctx, fixture.principal, v2.ID)
	if err != nil {
		t.Fatal(err)
	}
	optOuts, err = repo.ListUserSkillOptOuts(ctx, fixture.user.ID, v3.ID)
	if err != nil || !containsIntegrationUUID(optOuts, autoSkill.ID) {
		t.Fatalf("new version should inherit opt-out: %v, %v", optOuts, err)
	}
	publishedV3, err := fixture.agentService.Publish(ctx, fixture.principal, v3.ID)
	if err != nil {
		t.Fatal(err)
	}
	if containsIntegrationUUID(publishedV3.UserSkillIDs, autoSkill.ID) {
		t.Fatalf("opted-out skill was re-inherited on publish: %#v", publishedV3.UserSkillIDs)
	}

	// 后续自动挂载同样跳过显式移除的版本，其他版本保持绑定。
	if _, err := fixture.agentService.AttachUserSkillToAgentFamily(ctx, fixture.principal, v1.ID, autoSkill); err != nil {
		t.Fatal(err)
	}
	storedV3, err := fixture.agentService.Get(ctx, fixture.principal, v3.ID)
	if err != nil {
		t.Fatal(err)
	}
	if containsIntegrationUUID(storedV3.UserSkillIDs, autoSkill.ID) {
		t.Fatalf("auto attach must respect opt-out: %#v", storedV3.UserSkillIDs)
	}
	storedV1, err := fixture.agentService.Get(ctx, fixture.principal, v1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !containsIntegrationUUID(storedV1.UserSkillIDs, autoSkill.ID) {
		t.Fatalf("other versions must keep the binding: %#v", storedV1.UserSkillIDs)
	}

	// 在草稿中重新添加会清除移除标记。
	v2Current, err = fixture.agentService.Get(ctx, fixture.principal, v2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.agentService.Update(ctx, fixture.principal, biz.Agent{
		ID: v2.ID, Description: v2Current.Description, Mode: v2Current.Mode,
		ModelPolicyID: v2Current.ModelPolicyID, ModelPolicyVersion: v2Current.ModelPolicyVersion,
		UserSkillIDs: []uuid.UUID{autoSkill.ID}, UpdatedAt: v2Current.UpdatedAt,
	}); err != nil {
		t.Fatal(err)
	}
	optOuts, err = repo.ListUserSkillOptOuts(ctx, fixture.user.ID, v2.ID)
	if err != nil || len(optOuts) != 0 {
		t.Fatalf("re-add should clear opt-out: %v, %v", optOuts, err)
	}
}

// TestAgentExplicitOptOutSkipsInheritanceIntegration 验证草稿可以在发布前取消继承自动技能，
// 取消后发布不再补回，且预览不再展示。
func TestAgentExplicitOptOutSkipsInheritanceIntegration(t *testing.T) {
	fixture := newSkillSynthesisIntegrationFixture(t)
	ctx := context.Background()
	repo := NewAgentRepository(fixture.appDB)
	name := "explicit-optout-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	v1 := fixture.createPublishedAgent(t, name, nil)
	v2, err := fixture.agentService.CreateVersion(ctx, fixture.principal, v1.ID)
	if err != nil {
		t.Fatal(err)
	}
	autoSkill := fixture.createPublishedUserSkill(t, "weekly-report-auto", true)
	if _, err := fixture.agentService.AttachUserSkillToAgentFamily(ctx, fixture.principal, v1.ID, autoSkill); err != nil {
		t.Fatal(err)
	}
	inheritable, err := fixture.agentService.InheritableUserSkills(ctx, fixture.principal, v2.ID)
	if err != nil || !containsIntegrationUUID(userSkillIDs(inheritable), autoSkill.ID) {
		t.Fatalf("inheritable preview = %#v, %v", inheritable, err)
	}

	current, err := fixture.agentService.Get(ctx, fixture.principal, v2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.agentService.Update(ctx, fixture.principal, biz.Agent{
		ID: v2.ID, Description: current.Description, Mode: current.Mode,
		ModelPolicyID: current.ModelPolicyID, ModelPolicyVersion: current.ModelPolicyVersion,
		UserSkillIDs: nil, UserSkillOptOuts: []uuid.UUID{autoSkill.ID}, UpdatedAt: current.UpdatedAt,
	}); err != nil {
		t.Fatal(err)
	}
	inheritable, err = fixture.agentService.InheritableUserSkills(ctx, fixture.principal, v2.ID)
	if err != nil || len(inheritable) != 0 {
		t.Fatalf("preview after opt-out = %#v, %v", inheritable, err)
	}
	published, err := fixture.agentService.Publish(ctx, fixture.principal, v2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if containsIntegrationUUID(published.UserSkillIDs, autoSkill.ID) {
		t.Fatalf("explicitly canceled skill was inherited: %#v", published.UserSkillIDs)
	}
	optOuts, err := repo.ListUserSkillOptOuts(ctx, fixture.user.ID, v2.ID)
	if err != nil || !containsIntegrationUUID(optOuts, autoSkill.ID) {
		t.Fatalf("opt-outs = %v, %v", optOuts, err)
	}
}

// TestAgentRestoreSnapshotReconcilesUserSkillBindingsIntegration 验证恢复历史快照时同步绑定与移除标记，
// 避免运行时快照与绑定表不一致。
func TestAgentRestoreSnapshotReconcilesUserSkillBindingsIntegration(t *testing.T) {
	fixture := newSkillSynthesisIntegrationFixture(t)
	ctx := context.Background()
	repo := NewAgentRepository(fixture.appDB)
	name := "restore-agent-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	skillX := fixture.createPublishedUserSkill(t, "restore-x", true)
	skillY := fixture.createPublishedUserSkill(t, "restore-y", true)
	agent := fixture.createPublishedAgent(t, name, []uuid.UUID{skillX.ID})
	if _, err := fixture.agentService.AttachUserSkillToAgentFamily(ctx, fixture.principal, agent.ID, skillY); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.agentService.Unpublish(ctx, fixture.principal, agent.ID); err != nil {
		t.Fatal(err)
	}
	current, err := fixture.agentService.Get(ctx, fixture.principal, agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.agentService.Update(ctx, fixture.principal, biz.Agent{
		ID: agent.ID, Description: current.Description, Mode: current.Mode,
		ModelPolicyID: current.ModelPolicyID, ModelPolicyVersion: current.ModelPolicyVersion,
		UserSkillIDs: []uuid.UUID{skillY.ID}, UpdatedAt: current.UpdatedAt,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.agentService.Publish(ctx, fixture.principal, agent.ID); err != nil {
		t.Fatal(err)
	}
	revisions, err := fixture.agentService.ListSnapshotRevisions(ctx, fixture.principal, agent.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	var target biz.AgentSnapshotRevision
	for _, revision := range revisions {
		if revision.Reason == "user_skill_attached" {
			target = revision
			break
		}
	}
	if target.ID == uuid.Nil {
		t.Fatalf("missing snapshot revision: %#v", revisions)
	}
	restored, err := fixture.agentService.RestoreSnapshotRevision(ctx, fixture.principal, agent.ID, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !containsIntegrationUUID(restored.UserSkillIDs, skillX.ID) || containsIntegrationUUID(restored.UserSkillIDs, skillY.ID) {
		t.Fatalf("restored bindings = %#v", restored.UserSkillIDs)
	}
	optOuts, err := repo.ListUserSkillOptOuts(ctx, fixture.user.ID, agent.ID)
	if err != nil || !containsIntegrationUUID(optOuts, skillY.ID) || containsIntegrationUUID(optOuts, skillX.ID) {
		t.Fatalf("opt-outs after restore = %v, %v", optOuts, err)
	}
	var snapshot struct {
		ConfigSnapshot []byte
	}
	if err := fixture.adminDB.Table("agents").Select("config_snapshot").
		Where("id = ?", agent.ID).Scan(&snapshot).Error; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(snapshot.ConfigSnapshot), "restore-x") {
		t.Fatalf("restored snapshot missing skill x: %s", snapshot.ConfigSnapshot)
	}
}

// userSkillIDs 提取技能 ID，便于断言预览结果。
func userSkillIDs(skills []biz.UserSkill) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(skills))
	for _, skill := range skills {
		ids = append(ids, skill.ID)
	}
	return ids
}

// containsIntegrationUUID 判断 UUID 切片是否包含目标值。
func containsIntegrationUUID(values []uuid.UUID, target uuid.UUID) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
