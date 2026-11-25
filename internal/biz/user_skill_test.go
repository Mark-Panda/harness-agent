package biz

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeUserSkillRepository 是 UserSkillRepository 的内存桩，用于隔离测试 UserSkillService。
type fakeUserSkillRepository struct {
	skills    map[uuid.UUID]UserSkill
	revisions []UserSkillRevision
	err       error
}

func newFakeUserSkillRepository() *fakeUserSkillRepository {
	return &fakeUserSkillRepository{skills: map[uuid.UUID]UserSkill{}}
}

func (f *fakeUserSkillRepository) CreateUserSkill(_ context.Context, skill UserSkill, _ OutboxEvent) (UserSkill, error) {
	if f.err != nil {
		return UserSkill{}, f.err
	}
	skill.CreatedAt = time.Now().UTC()
	skill.UpdatedAt = skill.CreatedAt
	f.skills[skill.ID] = skill
	return skill, nil
}

func (f *fakeUserSkillRepository) UpdateUserSkill(_ context.Context, skill UserSkill, _ OutboxEvent) (UserSkill, error) {
	if f.err != nil {
		return UserSkill{}, f.err
	}
	// 真实仓储只更新内容字段，技能包元数据与创建时间保持不变。
	if current, ok := f.skills[skill.ID]; ok {
		skill.Bundle = current.Bundle
		skill.CreatedAt = current.CreatedAt
	}
	skill.UpdatedAt = time.Now().UTC()
	f.skills[skill.ID] = skill
	return skill, nil
}

func (f *fakeUserSkillRepository) GetUserSkill(_ context.Context, userID, skillID uuid.UUID) (UserSkill, error) {
	if f.err != nil {
		return UserSkill{}, f.err
	}
	skill, ok := f.skills[skillID]
	if !ok || skill.UserID != userID {
		return UserSkill{}, ErrResourceNotFound
	}
	return skill, nil
}

func (f *fakeUserSkillRepository) ListUserSkills(_ context.Context, userID uuid.UUID, _ int) ([]UserSkill, error) {
	result := make([]UserSkill, 0)
	for _, skill := range f.skills {
		if skill.UserID == userID {
			result = append(result, skill)
		}
	}
	return result, nil
}

func (f *fakeUserSkillRepository) ListUserSkillsByIDs(_ context.Context, userID uuid.UUID, ids []uuid.UUID) ([]UserSkill, error) {
	result := make([]UserSkill, 0, len(ids))
	for _, id := range ids {
		skill, ok := f.skills[id]
		if ok && skill.UserID == userID {
			result = append(result, skill)
		}
	}
	return result, nil
}

func (f *fakeUserSkillRepository) ListUserSkillRevisions(_ context.Context, userID, skillID uuid.UUID, _ int) ([]UserSkillRevision, error) {
	result := make([]UserSkillRevision, 0)
	for _, revision := range f.revisions {
		if revision.UserID == userID && revision.SkillID == skillID {
			result = append(result, revision)
		}
	}
	return result, nil
}

func (f *fakeUserSkillRepository) GetUserSkillRevision(_ context.Context, userID, skillID, revisionID uuid.UUID) (UserSkillRevision, error) {
	for _, revision := range f.revisions {
		if revision.ID == revisionID && revision.UserID == userID && revision.SkillID == skillID {
			return revision, nil
		}
	}
	return UserSkillRevision{}, ErrResourceNotFound
}

func (f *fakeUserSkillRepository) PublishUserSkill(_ context.Context, userID, skillID uuid.UUID, _ OutboxEvent) (UserSkill, error) {
	skill, err := f.GetUserSkill(context.Background(), userID, skillID)
	if err != nil {
		return UserSkill{}, err
	}
	skill.Status = UserSkillPublished
	skill.UpdatedAt = time.Now().UTC()
	f.skills[skillID] = skill
	return skill, nil
}

func (f *fakeUserSkillRepository) DisableUserSkill(_ context.Context, userID, skillID uuid.UUID, _ OutboxEvent) (UserSkill, error) {
	skill, err := f.GetUserSkill(context.Background(), userID, skillID)
	if err != nil {
		return UserSkill{}, err
	}
	skill.Status = UserSkillDisabled
	skill.UpdatedAt = time.Now().UTC()
	f.skills[skillID] = skill
	return skill, nil
}

func (f *fakeUserSkillRepository) DeleteUserSkill(_ context.Context, userID, skillID uuid.UUID, _ OutboxEvent) error {
	skill, err := f.GetUserSkill(context.Background(), userID, skillID)
	if err != nil {
		return err
	}
	delete(f.skills, skill.ID)
	return nil
}

// fakeSkillPolicyRepository 提供设置与提炼测试所需的策略和模型目录。
type fakeSkillPolicyRepository struct {
	policies       map[uuid.UUID]ModelPolicy
	models         map[uuid.UUID]Model
	getErr         error
	listModelsErr  error
	getPolicyCalls []uuid.UUID
}

func (f *fakeSkillPolicyRepository) CreatePolicy(context.Context, uuid.UUID, ModelPolicy, OutboxEvent) (ModelPolicy, error) {
	return ModelPolicy{}, errors.New("not implemented")
}
func (f *fakeSkillPolicyRepository) UpdatePolicy(context.Context, uuid.UUID, ModelPolicy, OutboxEvent) (ModelPolicy, error) {
	return ModelPolicy{}, errors.New("not implemented")
}
func (f *fakeSkillPolicyRepository) DeletePolicy(context.Context, uuid.UUID, uuid.UUID, OutboxEvent) error {
	return errors.New("not implemented")
}
func (f *fakeSkillPolicyRepository) GetPolicy(_ context.Context, policyID uuid.UUID) (ModelPolicy, error) {
	f.getPolicyCalls = append(f.getPolicyCalls, policyID)
	if f.getErr != nil {
		return ModelPolicy{}, f.getErr
	}
	policy, ok := f.policies[policyID]
	if !ok {
		return ModelPolicy{}, ErrResourceNotFound
	}
	return policy, nil
}
func (f *fakeSkillPolicyRepository) ListPolicies(context.Context) ([]ModelPolicy, error) {
	result := make([]ModelPolicy, 0, len(f.policies))
	for _, policy := range f.policies {
		result = append(result, policy)
	}
	return result, nil
}
func (f *fakeSkillPolicyRepository) ListModelsByIDs(_ context.Context, modelIDs []uuid.UUID) ([]Model, error) {
	if f.listModelsErr != nil {
		return nil, f.listModelsErr
	}
	result := make([]Model, 0, len(modelIDs))
	for _, modelID := range modelIDs {
		if catalogModel, ok := f.models[modelID]; ok {
			result = append(result, catalogModel)
		}
	}
	return result, nil
}
func (f *fakeSkillPolicyRepository) NextPolicyVersion(context.Context, string) (int, error) {
	return 1, nil
}

// userSkillPrincipal 构造一个普通用户主体。
func userSkillPrincipal() Principal {
	return Principal{UserID: uuid.New(), Role: RoleUser}
}

func TestUserSkillServiceCreateManual(t *testing.T) {
	repo := newFakeUserSkillRepository()
	service, err := NewUserSkillService(repo)
	if err != nil {
		t.Fatal(err)
	}
	principal := userSkillPrincipal()
	created, err := service.Create(context.Background(), principal, UserSkill{
		Name: "my-skill", Description: "test", PromptTemplate: "do {{thing}}",
		Parameters: map[string]any{"thing": "string"},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if created.UserID != principal.UserID || created.Origin != UserSkillOriginManual || created.Status != UserSkillAutoGenerated {
		t.Fatalf("unexpected created skill: %#v", created)
	}
	if created.Version != 1 {
		t.Fatalf("expected version 1, got %d", created.Version)
	}
}

func TestUserSkillServiceCreateRejectsEmptyName(t *testing.T) {
	repo := newFakeUserSkillRepository()
	service, _ := NewUserSkillService(repo)
	if _, err := service.Create(context.Background(), userSkillPrincipal(), UserSkill{PromptTemplate: "x"}); err == nil {
		t.Fatal("expected error for empty name")
	}
}

func TestUserSkillServiceUpdateOnlyAutoGenerated(t *testing.T) {
	repo := newFakeUserSkillRepository()
	service, _ := NewUserSkillService(repo)
	principal := userSkillPrincipal()
	created, _ := service.Create(context.Background(), principal, UserSkill{
		Name: "s", Description: "d", PromptTemplate: "p",
	})
	if _, err := service.Update(context.Background(), principal, UserSkill{
		ID: created.ID, Description: "updated", PromptTemplate: "new prompt",
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	// 发布后不可再编辑。
	published, _ := service.Publish(context.Background(), principal, created.ID)
	if _, err := service.Update(context.Background(), principal, UserSkill{
		ID: published.ID, Description: "x", PromptTemplate: "y",
	}); !errors.Is(err, ErrUserSkillStatusConflict) {
		t.Fatalf("expected status conflict, got %v", err)
	}
}

func TestUserSkillServicePublishAndDisable(t *testing.T) {
	repo := newFakeUserSkillRepository()
	service, _ := NewUserSkillService(repo)
	principal := userSkillPrincipal()
	created, _ := service.Create(context.Background(), principal, UserSkill{
		Name: "s", PromptTemplate: "p",
	})
	published, err := service.Publish(context.Background(), principal, created.ID)
	if err != nil || published.Status != UserSkillPublished {
		t.Fatalf("Publish() = %#v, %v", published, err)
	}
	disabled, err := service.Disable(context.Background(), principal, created.ID)
	if err != nil || disabled.Status != UserSkillDisabled {
		t.Fatalf("Disable() = %#v, %v", disabled, err)
	}
}

func TestUserSkillServiceDelete(t *testing.T) {
	repo := newFakeUserSkillRepository()
	service, _ := NewUserSkillService(repo)
	principal := userSkillPrincipal()
	created, _ := service.Create(context.Background(), principal, UserSkill{Name: "s", PromptTemplate: "p"})
	if err := service.Delete(context.Background(), principal, created.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := service.Get(context.Background(), principal, created.ID); !errors.Is(err, ErrResourceNotFound) {
		t.Fatalf("expected not found after delete, got %v", err)
	}
}

func TestUserSkillServiceIsolation(t *testing.T) {
	repo := newFakeUserSkillRepository()
	service, _ := NewUserSkillService(repo)
	owner := userSkillPrincipal()
	other := userSkillPrincipal()
	created, _ := service.Create(context.Background(), owner, UserSkill{Name: "s", PromptTemplate: "p"})
	// 其他用户无法读取。
	if _, err := service.Get(context.Background(), other, created.ID); !errors.Is(err, ErrResourceNotFound) {
		t.Fatalf("expected not found for other user, got %v", err)
	}
}

func TestUserSkillServiceCreateAutoSetsOrigin(t *testing.T) {
	repo := newFakeUserSkillRepository()
	service, _ := NewUserSkillService(repo)
	userID := uuid.New()
	created, err := service.CreateAuto(context.Background(), userID, UserSkill{
		Name: "auto-skill", Description: "自动生成的测试技能，用于覆盖创建流程与来源字段。",
		PromptTemplate:   "# auto-skill\n\n## 何时使用\n测试自动技能时使用。\n\n## 步骤\n1. 执行测试。\n\n## 输出格式\n文本结果。\n\n## 约束\n不得编造。",
		SourceSessionIDs: []uuid.UUID{uuid.New()}, Confidence: 0.9,
	})
	if err != nil {
		t.Fatalf("CreateAuto() error = %v", err)
	}
	if created.Origin != UserSkillOriginAuto || created.Status != UserSkillAutoGenerated {
		t.Fatalf("unexpected auto skill: %#v", created)
	}
}

func TestUserSkillServiceCreateAutoWritesStandardBundle(t *testing.T) {
	repo := newFakeUserSkillRepository()
	bundles := &fakeUserSkillBundleRepository{repo: repo}
	service, _ := NewUserSkillService(repo)
	service.WithBundles(bundles)
	created, err := service.CreateAuto(context.Background(), uuid.New(), UserSkill{
		Name: "auto-bundle", Description: "自动生成的标准包技能，用于验证 SKILL.md 文件的写入。",
		PromptTemplate: "# auto-bundle\n\n## 何时使用\n验证标准包时使用。\n\n## 步骤\n1. 生成文件。\n\n## 输出格式\nMarkdown。\n\n## 约束\n不得编造。",
	})
	if err != nil {
		t.Fatalf("CreateAuto() error = %v", err)
	}
	if created.Bundle.Format != SkillBundleFormat || created.Bundle.FileCount != 1 || created.Bundle.Entrypoint != "SKILL.md" {
		t.Fatalf("unexpected bundle metadata: %#v", created.Bundle)
	}
	if len(bundles.files) != 1 || bundles.files[0].Path != "SKILL.md" {
		t.Fatalf("unexpected bundle files: %#v", bundles.files)
	}
	if !strings.Contains(string(bundles.files[0].Content), "name: auto-bundle") {
		t.Fatalf("bundle missing frontmatter: %s", bundles.files[0].Content)
	}
}

func TestUserSkillServiceCreateAutoRollsBackWhenBundleFails(t *testing.T) {
	repo := newFakeUserSkillRepository()
	bundles := &fakeUserSkillBundleRepository{repo: repo, replaceErr: errors.New("bundle write failed")}
	service, _ := NewUserSkillService(repo)
	service.WithBundles(bundles)
	if _, err := service.CreateAuto(context.Background(), uuid.New(), UserSkill{
		Name: "auto-rollback", Description: "自动生成的标准包技能，用于验证包写入失败时的回滚。",
		PromptTemplate: "# auto-rollback\n\n## 何时使用\n验证失败回滚时使用。\n\n## 步骤\n1. 写入失败。\n\n## 输出格式\nMarkdown。\n\n## 约束\n不得编造。",
	}); err == nil {
		t.Fatal("expected bundle failure")
	}
	if len(repo.skills) != 0 {
		t.Fatalf("skill must be removed after bundle failure: %#v", repo.skills)
	}
}

func TestUserSkillSynthesisSettingsDefaultDisabledAndIsolated(t *testing.T) {
	settings := &fakeSkillSynthesisSettingsStore{}
	policies := &fakeSkillPolicyRepository{}
	repo := newFakeUserSkillRepository()
	service, err := NewUserSkillService(repo)
	if err != nil {
		t.Fatal(err)
	}
	service.WithSynthesisSettings(settings, policies)
	owner := userSkillPrincipal()
	other := userSkillPrincipal()
	policyID := uuid.New()
	version := 1
	policies.policies = map[uuid.UUID]ModelPolicy{
		policyID: {ID: policyID, Version: version, Status: PolicyPublished},
	}
	if _, err := service.UpdateSynthesisSettings(context.Background(), owner, UserSkillSynthesisSettings{
		Enabled: true, ModelPolicyID: &policyID, ModelPolicyVersion: &version,
	}); err != nil {
		t.Fatalf("UpdateSynthesisSettings() error = %v", err)
	}
	got, err := service.GetSynthesisSettings(context.Background(), other)
	if err != nil {
		t.Fatalf("GetSynthesisSettings() error = %v", err)
	}
	if got.UserID != other.UserID || got.Enabled || got.ModelPolicyID != nil || got.ModelPolicyVersion != nil {
		t.Fatalf("other user should receive default disabled settings, got %#v", got)
	}
}

func TestUserSkillSynthesisSettingsUpdateAndClearPolicy(t *testing.T) {
	settings := &fakeSkillSynthesisSettingsStore{}
	policyID := uuid.New()
	version := 2
	policies := &fakeSkillPolicyRepository{policies: map[uuid.UUID]ModelPolicy{
		policyID: {ID: policyID, Version: version, Status: PolicyPublished},
	}}
	service, _ := NewUserSkillService(newFakeUserSkillRepository())
	service.WithSynthesisSettings(settings, policies)
	principal := userSkillPrincipal()
	createdAt := time.Now().Add(-time.Hour)
	updated, err := service.UpdateSynthesisSettings(context.Background(), principal, UserSkillSynthesisSettings{
		UserID: uuid.New(), Enabled: true, ModelPolicyID: &policyID, ModelPolicyVersion: &version,
		CreatedAt: createdAt, UpdatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("UpdateSynthesisSettings() error = %v", err)
	}
	if updated.UserID != principal.UserID || !updated.Enabled || updated.ModelPolicyID == nil || *updated.ModelPolicyID != policyID {
		t.Fatalf("unexpected saved settings: %#v", updated)
	}
	if updated.CreatedAt.Equal(createdAt) || updated.UpdatedAt.Equal(createdAt) {
		t.Fatal("client timestamps must not be persisted")
	}
	cleared, err := service.UpdateSynthesisSettings(context.Background(), principal, UserSkillSynthesisSettings{Enabled: false})
	if err != nil {
		t.Fatalf("clear settings error = %v", err)
	}
	if cleared.Enabled || cleared.ModelPolicyID != nil || cleared.ModelPolicyVersion != nil {
		t.Fatalf("expected complete replacement with policy cleared, got %#v", cleared)
	}
	if !cleared.CreatedAt.Equal(updated.CreatedAt) {
		t.Fatal("upsert must preserve the original creation timestamp")
	}
}

func TestUserSkillSynthesisSettingsRejectsInvalidPolicyReference(t *testing.T) {
	policyID := uuid.New()
	version := 1
	nilID := uuid.Nil
	cases := []struct {
		name     string
		settings UserSkillSynthesisSettings
		policy   ModelPolicy
	}{
		{name: "missing version", settings: UserSkillSynthesisSettings{ModelPolicyID: &policyID}},
		{name: "missing id", settings: UserSkillSynthesisSettings{ModelPolicyVersion: &version}},
		{name: "nil id", settings: UserSkillSynthesisSettings{ModelPolicyID: &nilID, ModelPolicyVersion: &version}},
		{name: "draft", settings: UserSkillSynthesisSettings{ModelPolicyID: &policyID, ModelPolicyVersion: &version}, policy: ModelPolicy{ID: policyID, Version: 1, Status: PolicyDraft}},
		{name: "version mismatch", settings: UserSkillSynthesisSettings{ModelPolicyID: &policyID, ModelPolicyVersion: &version}, policy: ModelPolicy{ID: policyID, Version: 2, Status: PolicyPublished}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			settings := &fakeSkillSynthesisSettingsStore{}
			policies := &fakeSkillPolicyRepository{policies: map[uuid.UUID]ModelPolicy{policyID: testCase.policy}}
			service, _ := NewUserSkillService(newFakeUserSkillRepository())
			service.WithSynthesisSettings(settings, policies)
			if _, err := service.UpdateSynthesisSettings(context.Background(), userSkillPrincipal(), testCase.settings); err == nil {
				t.Fatal("expected invalid policy reference to be rejected")
			}
			if len(settings.items) != 0 {
				t.Fatal("invalid settings must not be persisted")
			}
		})
	}
}

func TestValidateUserSkillConfidenceBounds(t *testing.T) {
	cases := []float64{-0.1, 1.1}
	for _, confidence := range cases {
		skill := UserSkill{
			ID: uuid.New(), UserID: uuid.New(), Name: "x", Version: 1,
			PromptTemplate: "p", Confidence: confidence,
		}
		if err := validateUserSkill(&skill); err == nil {
			t.Fatalf("expected error for confidence %f", confidence)
		}
	}
}

// fakeUserSkillBundleRepository 同步维护技能元数据，模拟真实仓储的事务更新。
type fakeUserSkillBundleRepository struct {
	repo       *fakeUserSkillRepository
	info       SkillBundleInfo
	files      []SkillBundleFile
	replaceErr error
}

func (f *fakeUserSkillBundleRepository) ReplaceUserSkillBundle(_ context.Context, _, skillID uuid.UUID, info SkillBundleInfo, files []SkillBundleFile, _ OutboxEvent) error {
	if f.replaceErr != nil {
		return f.replaceErr
	}
	f.info, f.files = info, append([]SkillBundleFile(nil), files...)
	skill := f.repo.skills[skillID]
	skill.Bundle = info
	f.repo.skills[skillID] = skill
	return nil
}

func (f *fakeUserSkillBundleRepository) ListUserSkillFileMeta(context.Context, uuid.UUID, uuid.UUID) ([]SkillBundleFile, error) {
	return append([]SkillBundleFile(nil), f.files...), nil
}

func (f *fakeUserSkillBundleRepository) GetUserSkillFile(_ context.Context, _, _ uuid.UUID, path string) (SkillBundleFile, error) {
	for _, file := range f.files {
		if file.Path == path {
			return file, nil
		}
	}
	return SkillBundleFile{}, ErrSkillBundleNotFound
}

func (f *fakeUserSkillBundleRepository) SetUserSkillBundleExecution(_ context.Context, _, skillID uuid.UUID, enabled bool, _ OutboxEvent) error {
	f.info.ExecutionEnabled = enabled
	skill := f.repo.skills[skillID]
	skill.Bundle = f.info
	f.repo.skills[skillID] = skill
	return nil
}

func newUserSkillTestArchive(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	entries := []struct{ name, content string }{
		{name: "SKILL.md", content: "---\nname: 测试技能\ndescription: 用于验证技能包\n---\n\n# 测试技能\n\n按步骤执行。\n"},
		{name: "scripts/hello.py", content: "print('hello')\n"},
		{name: "references/guide.md", content: "# 指南\n"},
	}
	for _, entry := range entries {
		file, err := writer.Create(entry.name)
		if err != nil {
			t.Fatalf("create zip entry: %v", err)
		}
		if _, err := file.Write([]byte(entry.content)); err != nil {
			t.Fatalf("write zip entry: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buffer.Bytes()
}

func TestUserSkillBundleLifecycleAndTenantIsolation(t *testing.T) {
	repo := newFakeUserSkillRepository()
	bundles := &fakeUserSkillBundleRepository{repo: repo}
	service, err := NewUserSkillService(repo)
	if err != nil {
		t.Fatal(err)
	}
	service.WithBundles(bundles)
	owner, other := userSkillPrincipal(), userSkillPrincipal()
	imported, err := service.ImportBundle(context.Background(), owner, newUserSkillTestArchive(t))
	if err != nil {
		t.Fatalf("ImportBundle() error = %v", err)
	}
	if imported.UserID != owner.UserID || imported.Bundle.FileCount != 3 || !imported.Bundle.HasScripts {
		t.Fatalf("unexpected imported skill: %#v", imported)
	}
	files, err := service.GetBundleFiles(context.Background(), owner, imported.ID)
	if err != nil || len(files) != 3 {
		t.Fatalf("GetBundleFiles() = %#v, %v", files, err)
	}
	file, err := service.ReadBundleFile(context.Background(), owner, imported.ID, "references/guide.md")
	if err != nil || string(file.Content) != "# 指南\n" || file.Owner != SkillFileOwnerUser {
		t.Fatalf("ReadBundleFile() = %#v, %v", file, err)
	}
	enabled, err := service.SetBundleExecution(context.Background(), owner, imported.ID, true)
	if err != nil || !enabled.Bundle.ExecutionEnabled {
		t.Fatalf("SetBundleExecution() = %#v, %v", enabled, err)
	}
	if _, err := service.ReadBundleFile(context.Background(), other, imported.ID, "references/guide.md"); !errors.Is(err, ErrResourceNotFound) {
		t.Fatalf("cross-tenant bundle read error = %v", err)
	}
	ownerSkills, err := service.List(context.Background(), owner, 0)
	if err != nil || len(ownerSkills) != 1 {
		t.Fatalf("owner List() = %#v, %v", ownerSkills, err)
	}
	otherSkills, err := service.List(context.Background(), other, 500)
	if err != nil || len(otherSkills) != 0 {
		t.Fatalf("other tenant List() = %#v, %v", otherSkills, err)
	}
}

func TestUserSkillListSynthesisPoliciesOnlyPublished(t *testing.T) {
	publishedID, draftID := uuid.New(), uuid.New()
	policies := &fakeSkillPolicyRepository{policies: map[uuid.UUID]ModelPolicy{
		publishedID: {ID: publishedID, Status: PolicyPublished},
		draftID:     {ID: draftID, Status: PolicyDraft},
	}}
	service, _ := NewUserSkillService(newFakeUserSkillRepository())
	service.WithSynthesisSettings(&fakeSkillSynthesisSettingsStore{}, policies)
	listed, err := service.ListSynthesisPolicies(context.Background(), userSkillPrincipal())
	if err != nil || len(listed) != 1 || listed[0].ID != publishedID {
		t.Fatalf("ListSynthesisPolicies() = %#v, %v", listed, err)
	}
}

// TestUserSkillServiceTriggerSynthesisGuards 验证手动触发的前置校验与按分钟去重标识。
func TestUserSkillServiceTriggerSynthesisGuards(t *testing.T) {
	userID := uuid.New()
	settings := &fakeSkillSynthesisSettingsStore{items: map[uuid.UUID]UserSkillSynthesisSettings{
		userID: {UserID: userID, Enabled: false},
	}}
	runs := &fakeSkillSynthesisRuns{}
	queue := &fakeSkillSynthesisQueue{}
	service := &UserSkillService{settings: settings, runs: runs, trigger: queue}
	principal := Principal{UserID: userID}

	if _, err := service.TriggerSynthesis(context.Background(), principal); !errors.Is(err, ErrSkillSynthesisDisabled) {
		t.Fatalf("disabled trigger error = %v", err)
	}

	settings.items[userID] = UserSkillSynthesisSettings{UserID: userID, Enabled: true}
	today := time.Now().UTC().Format("2006-01-02")
	runs.listed = []SkillSynthesisRun{{Date: today, Status: SkillSynthesisRunRunning}}
	if _, err := service.TriggerSynthesis(context.Background(), principal); !errors.Is(err, ErrSkillSynthesisInProgress) {
		t.Fatalf("in-progress trigger error = %v", err)
	}

	runs.listed = nil
	date, err := service.TriggerSynthesis(context.Background(), principal)
	if err != nil || date != today {
		t.Fatalf("trigger = %q, %v", date, err)
	}
	if len(queue.tasks) != 1 || queue.tasks[0].Date != today ||
		!strings.HasPrefix(queue.tasks[0].RequestID, "manual:") {
		t.Fatalf("queued tasks = %#v", queue.tasks)
	}
}

// TestUserSkillServiceListRevisions 验证历史快照按用户隔离读取。
func TestUserSkillServiceListRevisions(t *testing.T) {
	userID := uuid.New()
	skillID := uuid.New()
	repo := newFakeUserSkillRepository()
	repo.skills[skillID] = UserSkill{ID: skillID, UserID: userID, Name: "weekly-report", Version: 1}
	repo.revisions = []UserSkillRevision{
		{ID: uuid.New(), UserID: userID, SkillID: skillID, Version: 1, PromptTemplate: "old"},
		{ID: uuid.New(), UserID: uuid.New(), SkillID: skillID, Version: 1, PromptTemplate: "other"},
	}
	service, err := NewUserSkillService(repo)
	if err != nil {
		t.Fatal(err)
	}
	revisions, err := service.ListRevisions(context.Background(), Principal{UserID: userID}, skillID, 10)
	if err != nil {
		t.Fatalf("ListRevisions() error = %v", err)
	}
	if len(revisions) != 1 || revisions[0].PromptTemplate != "old" {
		t.Fatalf("revisions = %#v", revisions)
	}
	if _, err := service.ListRevisions(context.Background(), Principal{UserID: uuid.New()}, skillID, 10); !errors.Is(err, ErrResourceNotFound) {
		t.Fatalf("cross-user revisions error = %v", err)
	}
}

// TestUserSkillServiceUpdateRefreshesAutoBundle 验证手动编辑自动技能会同步重建 SKILL.md。
func TestUserSkillServiceUpdateRefreshesAutoBundle(t *testing.T) {
	userID := uuid.New()
	repo := newFakeUserSkillRepository()
	existing := UserSkill{
		ID: uuid.New(), UserID: userID, Name: "auto-bundle", Version: 1,
		Description:    "自动生成的标准包技能，用于验证手动编辑后的技能包刷新。",
		PromptTemplate: "# auto-bundle\n\n## 何时使用\n验证标准包时使用。\n\n## 步骤\n1. 旧步骤。\n\n## 输出格式\nMarkdown。\n\n## 约束\n不得编造。",
		Status:         UserSkillAutoGenerated, Origin: UserSkillOriginAuto,
		Bundle: SkillBundleInfo{Format: SkillBundleFormat, Entrypoint: "SKILL.md", FileCount: 1},
	}
	repo.skills[existing.ID] = existing
	bundles := &fakeUserSkillBundleRepository{repo: repo}
	service, err := NewUserSkillService(repo)
	if err != nil {
		t.Fatal(err)
	}
	service.WithBundles(bundles)
	updated, err := service.Update(context.Background(), Principal{UserID: userID, Role: RoleUser}, UserSkill{
		ID:             existing.ID,
		Description:    existing.Description,
		PromptTemplate: "# auto-bundle\n\n## 何时使用\n验证标准包时使用。\n\n## 步骤\n1. 新步骤。\n\n## 输出格式\nMarkdown。\n\n## 约束\n不得编造。",
		Parameters:     map[string]any{},
	})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if updated.Bundle.FileCount != 1 || len(bundles.files) != 1 ||
		!strings.Contains(string(bundles.files[0].Content), "新步骤") {
		t.Fatalf("auto bundle was not refreshed: %#v files=%d", updated.Bundle, len(bundles.files))
	}
}

// TestUserSkillServiceUpdateKeepsImportedBundle 验证手动导入的多文件技能包不会被自动重建。
func TestUserSkillServiceUpdateKeepsImportedBundle(t *testing.T) {
	userID := uuid.New()
	repo := newFakeUserSkillRepository()
	existing := UserSkill{
		ID: uuid.New(), UserID: userID, Name: "imported-skill", Version: 1,
		Description:    "从标准技能包导入的技能，包含脚本与参考资料。",
		PromptTemplate: "# imported-skill\n\n按步骤执行。",
		Status:         UserSkillAutoGenerated, Origin: UserSkillOriginManual,
		Bundle: SkillBundleInfo{Format: SkillBundleFormat, Entrypoint: "SKILL.md", FileCount: 3, HasScripts: true},
	}
	repo.skills[existing.ID] = existing
	bundles := &fakeUserSkillBundleRepository{repo: repo}
	service, err := NewUserSkillService(repo)
	if err != nil {
		t.Fatal(err)
	}
	service.WithBundles(bundles)
	if _, err := service.Update(context.Background(), Principal{UserID: userID, Role: RoleUser}, UserSkill{
		ID: existing.ID, Description: existing.Description,
		PromptTemplate: "# imported-skill\n\n按更新后的步骤执行。", Parameters: map[string]any{},
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if len(bundles.files) != 0 {
		t.Fatalf("imported bundle was rebuilt: %d files", len(bundles.files))
	}
}

// TestUserSkillServiceRestoreRevision 验证恢复历史快照会覆盖内容并标记用户已编辑。
func TestUserSkillServiceRestoreRevision(t *testing.T) {
	userID := uuid.New()
	skillID := uuid.New()
	revisionID := uuid.New()
	repo := newFakeUserSkillRepository()
	repo.skills[skillID] = UserSkill{
		ID: skillID, UserID: userID, Name: "weekly-report", Version: 1,
		Description: "当前描述", PromptTemplate: "当前正文",
		Status: UserSkillPublished, Origin: UserSkillOriginAuto,
	}
	repo.revisions = []UserSkillRevision{{
		ID: revisionID, UserID: userID, SkillID: skillID, Version: 1,
		Description: "历史描述", PromptTemplate: "历史正文", Parameters: map[string]any{},
		SourceSessionIDs: []uuid.UUID{uuid.New()}, Confidence: 0.6,
	}}
	service, err := NewUserSkillService(repo)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := service.RestoreRevision(context.Background(), Principal{UserID: userID}, skillID, revisionID)
	if err != nil {
		t.Fatalf("RestoreRevision() error = %v", err)
	}
	if restored.Description != "历史描述" || restored.PromptTemplate != "历史正文" || restored.Confidence != 0.6 {
		t.Fatalf("restored content = %#v", restored)
	}
	if restored.Name != "weekly-report" || restored.Version != 1 || restored.Status != UserSkillPublished || restored.Origin != UserSkillOriginAuto {
		t.Fatalf("restore changed identity: %#v", restored)
	}
	if restored.UserEditedAt == nil {
		t.Fatal("restored skill must be marked as user edited")
	}
}

// TestUserSkillServiceOptimizeAuto 验证自动技能原地优化保留身份与生命周期，并合并来源会话。
func TestUserSkillServiceOptimizeAuto(t *testing.T) {
	userID := uuid.New()
	firstSource, secondSource := uuid.New(), uuid.New()
	repo := newFakeUserSkillRepository()
	current := UserSkill{
		ID: uuid.New(), UserID: userID, Name: "weekly-report", Version: 2,
		Description:    "每周整理工作进展并输出可复用周报，适用于固定节奏的汇报场景。",
		PromptTemplate: "# weekly-report\n\n## 何时使用\n每周汇报时使用。\n\n## 步骤\n1. 汇总进展。\n\n## 输出格式\nMarkdown。\n\n## 约束\n不得编造。",
		Status:         UserSkillPublished, Origin: UserSkillOriginAuto,
		SourceSessionIDs: []uuid.UUID{firstSource}, Confidence: 0.6,
	}
	repo.skills[current.ID] = current
	service, err := NewUserSkillService(repo)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := service.OptimizeAuto(context.Background(), userID, UserSkill{
		ID:          current.ID,
		Description: "每周整理工作进展与风险项并输出可复用周报，适用于固定节奏的汇报场景。",
		PromptTemplate: "# weekly-report\n\n## 何时使用\n每周汇报时使用。\n\n## 步骤\n1. 汇总进展。\n2. 汇总风险项。\n\n" +
			"## 输出格式\nMarkdown。\n\n## 约束\n不得编造。",
		Parameters:       map[string]any{},
		SourceSessionIDs: []uuid.UUID{firstSource, secondSource},
		Confidence:       0.9,
	})
	if err != nil {
		t.Fatalf("OptimizeAuto() error = %v", err)
	}
	if updated.ID != current.ID || updated.Name != current.Name || updated.Version != current.Version ||
		updated.Status != current.Status || updated.Origin != current.Origin {
		t.Fatalf("optimized identity changed: %#v", updated)
	}
	if !strings.Contains(updated.PromptTemplate, "汇总风险项") || updated.Confidence != 0.9 {
		t.Fatalf("optimized content = %#v", updated)
	}
	if len(updated.SourceSessionIDs) != 2 || updated.SourceSessionIDs[0] != firstSource || updated.SourceSessionIDs[1] != secondSource {
		t.Fatalf("source sessions = %#v", updated.SourceSessionIDs)
	}
}

// TestUserSkillServiceOptimizeAutoRefreshesBundle 验证原地优化会同步重建标准技能包。
func TestUserSkillServiceOptimizeAutoRefreshesBundle(t *testing.T) {
	userID := uuid.New()
	repo := newFakeUserSkillRepository()
	existing := UserSkill{
		ID: uuid.New(), UserID: userID, Name: "weekly-report", Version: 1,
		Description:    "每周整理工作进展并输出可复用周报，适用于固定节奏的汇报场景。",
		PromptTemplate: "# weekly-report\n\n## 何时使用\n每周汇报时使用。\n\n## 步骤\n1. 汇总进展。\n\n## 输出格式\nMarkdown。\n\n## 约束\n不得编造。",
		Status:         UserSkillPublished, Origin: UserSkillOriginAuto, Confidence: 0.7,
		Bundle: SkillBundleInfo{Format: SkillBundleFormat, Entrypoint: "SKILL.md", FileCount: 1},
	}
	repo.skills[existing.ID] = existing
	bundles := &fakeUserSkillBundleRepository{repo: repo}
	service, err := NewUserSkillService(repo)
	if err != nil {
		t.Fatal(err)
	}
	service.WithBundles(bundles)
	optimized, err := service.OptimizeAuto(context.Background(), userID, UserSkill{
		ID:          existing.ID,
		Description: "每周整理工作进展与风险项并输出可复用周报，适用于固定节奏的汇报场景。",
		PromptTemplate: "# weekly-report\n\n## 何时使用\n每周汇报时使用。\n\n## 步骤\n1. 汇总进展。\n2. 汇总风险项。\n\n" +
			"## 输出格式\nMarkdown。\n\n## 约束\n不得编造。",
		Parameters: map[string]any{},
		Confidence: 0.9,
	})
	if err != nil {
		t.Fatalf("OptimizeAuto() error = %v", err)
	}
	if optimized.Bundle.Format != SkillBundleFormat || optimized.Bundle.FileCount != 1 ||
		optimized.Bundle.Entrypoint != "SKILL.md" {
		t.Fatalf("bundle metadata was not refreshed: %#v", optimized.Bundle)
	}
	file, err := bundles.GetUserSkillFile(context.Background(), userID, existing.ID, "SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(file.Content), "汇总风险项") || !strings.Contains(string(file.Content), "name: weekly-report") {
		t.Fatalf("SKILL.md was not rebuilt with optimized content: %s", file.Content)
	}
	if repo.skills[existing.ID].Bundle.FileCount != 1 {
		t.Fatalf("skill bundle metadata was not updated: %#v", repo.skills[existing.ID].Bundle)
	}
}

// TestUserSkillServiceOptimizeAutoRejectsNonAutoSkills 验证手动与停用技能不会被自动优化。
func TestUserSkillServiceOptimizeAutoRejectsNonAutoSkills(t *testing.T) {
	userID := uuid.New()
	for _, tc := range []struct {
		name   string
		origin UserSkillOrigin
		status UserSkillStatus
	}{
		{"manual", UserSkillOriginManual, UserSkillPublished},
		{"disabled", UserSkillOriginAuto, UserSkillDisabled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakeUserSkillRepository()
			existing := UserSkill{ID: uuid.New(), UserID: userID, Name: "weekly-report", Version: 1, Status: tc.status, Origin: tc.origin}
			repo.skills[existing.ID] = existing
			service, err := NewUserSkillService(repo)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.OptimizeAuto(context.Background(), userID, UserSkill{ID: existing.ID}); !errors.Is(err, ErrUserSkillStatusConflict) {
				t.Fatalf("OptimizeAuto() error = %v", err)
			}
		})
	}
}
