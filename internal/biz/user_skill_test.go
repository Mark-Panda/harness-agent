package biz

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeUserSkillRepository 是 UserSkillRepository 的内存桩，用于隔离测试 UserSkillService。
type fakeUserSkillRepository struct {
	skills map[uuid.UUID]UserSkill
	err    error
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
		Name: "auto", Description: "auto skill", PromptTemplate: "auto {{x}}",
		SourceSessionIDs: []uuid.UUID{uuid.New()}, Confidence: 0.9,
	})
	if err != nil {
		t.Fatalf("CreateAuto() error = %v", err)
	}
	if created.Origin != UserSkillOriginAuto || created.Status != UserSkillAutoGenerated {
		t.Fatalf("unexpected auto skill: %#v", created)
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
	repo  *fakeUserSkillRepository
	info  SkillBundleInfo
	files []SkillBundleFile
}

func (f *fakeUserSkillBundleRepository) ReplaceUserSkillBundle(_ context.Context, _, skillID uuid.UUID, info SkillBundleInfo, files []SkillBundleFile, _ OutboxEvent) error {
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
