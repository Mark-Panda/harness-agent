package biz_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/service/mocks"
	"github.com/google/uuid"
	"go.uber.org/mock/gomock"
)

func TestProviderServiceLifecycleAndValidation(t *testing.T) {
	ctrl := gomock.NewController(t)
	repo := mocks.NewMockProviderRepository(ctrl)
	cipher := mocks.NewMockCredentialCipher(ctrl)
	admin := biz.Principal{UserID: uuid.New(), Role: biz.RoleAdmin}
	cipher.EXPECT().Seal([]byte("secret")).Return([]byte("encrypted"), "v1", nil)
	repo.EXPECT().CreateProvider(gomock.Any(), admin.UserID, gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _ uuid.UUID, provider biz.Provider, credential biz.ProviderCredential, event biz.OutboxEvent) (biz.Provider, error) {
			if credential.KeyVersion != "v1" || string(credential.Encrypted) != "encrypted" || event.Payload["outcome"] != "success" {
				t.Fatal("provider secret was not sealed or audit event is invalid")
			}
			return provider, nil
		},
	)
	repo.EXPECT().UpdateProvider(gomock.Any(), admin.UserID, gomock.Any(), gomock.Nil(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _ uuid.UUID, provider biz.Provider, _ *biz.ProviderCredential, _ biz.OutboxEvent) (biz.Provider, error) {
			return provider, nil
		},
	)
	repo.EXPECT().GetProvider(gomock.Any(), gomock.Any()).Return(biz.Provider{}, nil)
	repo.EXPECT().ListProviders(gomock.Any()).Return([]biz.Provider{}, nil)
	repo.EXPECT().DeleteProvider(gomock.Any(), admin.UserID, gomock.Any(), gomock.Any()).Return(nil)

	service, err := biz.NewProviderService(repo, cipher)
	if err != nil {
		t.Fatal(err)
	}
	input := biz.ProviderInput{Name: " primary ", BaseURL: "https://models.example.com/", APIKey: "secret"}
	created, err := service.Create(context.Background(), admin, input)
	if err != nil || created.ProviderType != "openai_compatible" || created.BaseURL != "https://models.example.com" {
		t.Fatalf("Create() = %#v, %v", created, err)
	}
	input.APIKey = ""
	if _, err := service.Update(context.Background(), admin, created.ID, input); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Get(context.Background(), admin, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.List(context.Background(), admin); err != nil {
		t.Fatal(err)
	}
	if err := service.Delete(context.Background(), admin, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Create(context.Background(), biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}, input); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("non-admin error = %v", err)
	}
	input.BaseURL = "http://localhost:11434"
	if _, err := service.Create(context.Background(), admin, input); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("insecure endpoint error = %v", err)
	}
	input.BaseURL = "https://models.example.com"
	input.APIKey = ""
	if _, err := service.Create(context.Background(), admin, input); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("empty API key error = %v", err)
	}
	if err := service.Delete(context.Background(), admin, uuid.Nil); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("nil provider id error = %v", err)
	}
	if _, err := biz.NewProviderService(nil, cipher); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("nil dependency error = %v", err)
	}
}

func TestModelServiceEnforcesVerificationAndContext(t *testing.T) {
	ctrl := gomock.NewController(t)
	repo := mocks.NewMockModelRepository(ctrl)
	admin := biz.Principal{UserID: uuid.New(), Role: biz.RoleAdmin}
	service, err := biz.NewModelService(repo)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	model := biz.Model{
		ProviderID: uuid.New(), RemoteModelName: " generation ", Kind: biz.ModelKindChat,
		ContextWindowTokens: biz.MinGenerationContextTokens, MaxOutputTokens: 8192,
		Status: biz.ModelEnabled, VerificationSource: "admin-console", VerifiedAt: &now,
	}
	repo.EXPECT().GetProvider(gomock.Any(), model.ProviderID).Return(biz.Provider{Status: biz.ProviderEnabled}, nil)
	repo.EXPECT().CreateModel(gomock.Any(), admin.UserID, gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _ uuid.UUID, created biz.Model, _ biz.OutboxEvent) (biz.Model, error) {
			if created.RemoteModelName != "generation" || created.ID == uuid.Nil {
				t.Fatalf("model was not normalized: %#v", created)
			}
			return created, nil
		},
	)
	created, err := service.Create(context.Background(), admin, model)
	if err != nil {
		t.Fatal(err)
	}

	lowContext := created
	lowContext.ID = uuid.Nil
	lowContext.ContextWindowTokens = 999_999
	lowContext.Status = biz.ModelDisabled
	if _, err := service.Create(context.Background(), admin, lowContext); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("low context error = %v", err)
	}
	unverified := created
	unverified.VerifiedAt = nil
	if _, err := service.Update(context.Background(), admin, unverified); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("unverified model error = %v", err)
	}

	repo.EXPECT().GetProvider(gomock.Any(), created.ProviderID).Return(biz.Provider{Status: biz.ProviderEnabled}, nil)
	repo.EXPECT().UpdateModel(gomock.Any(), admin.UserID, gomock.Any(), gomock.Any()).Return(created, nil)
	if _, err := service.Update(context.Background(), admin, created); err != nil {
		t.Fatal(err)
	}
	repo.EXPECT().GetModel(gomock.Any(), created.ID).Return(created, nil)
	repo.EXPECT().ListModels(gomock.Any(), gomock.Nil()).Return([]biz.Model{created}, nil)
	repo.EXPECT().DeleteModel(gomock.Any(), admin.UserID, created.ID, gomock.Any()).Return(nil)
	if _, err := service.Get(context.Background(), admin, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.List(context.Background(), admin, nil); err != nil {
		t.Fatal(err)
	}
	if err := service.Delete(context.Background(), admin, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Create(context.Background(), biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}, created); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("non-admin error = %v", err)
	}
	if _, err := biz.NewModelService(nil); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("nil dependency error = %v", err)
	}
}

// TestPolicyPublishRejectsDataRegionMismatch 验证数据区域约束在发布时与模型元数据硬校验。
func TestPolicyPublishRejectsDataRegionMismatch(t *testing.T) {
	ctrl := gomock.NewController(t)
	repo := mocks.NewMockPolicyRepository(ctrl)
	admin := biz.Principal{UserID: uuid.New(), Role: biz.RoleAdmin}
	service, err := biz.NewPolicyService(repo)
	if err != nil {
		t.Fatal(err)
	}
	modelID := uuid.New()
	now := time.Now().UTC()
	candidate := biz.PolicyCandidate{
		ModelID: modelID, Priority: 1, Constraints: map[string]any{"data_region": "cn"},
	}
	policy := biz.ModelPolicy{Name: "compliance", Candidates: []biz.PolicyCandidate{candidate}}
	repo.EXPECT().CreatePolicy(gomock.Any(), admin.UserID, gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _ uuid.UUID, created biz.ModelPolicy, _ biz.OutboxEvent) (biz.ModelPolicy, error) {
			return created, nil
		},
	)
	created, err := service.Create(context.Background(), admin, policy)
	if err != nil {
		t.Fatal(err)
	}
	repo.EXPECT().GetPolicy(gomock.Any(), created.ID).Return(created, nil)
	repo.EXPECT().ListModelsByIDs(gomock.Any(), []uuid.UUID{modelID}).Return([]biz.Model{{
		ID: modelID, Kind: biz.ModelKindChat, Status: biz.ModelEnabled, ContextWindowTokens: biz.MinGenerationContextTokens,
		VerificationSource: "admin-console", VerifiedAt: &now, DataRegion: "us",
	}}, nil)
	if _, err := service.Publish(context.Background(), admin, created.ID); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("data region mismatch error = %v", err)
	}

	// 非字符串约束同样视为非法配置。
	invalidCandidate := candidate
	invalidCandidate.Constraints = map[string]any{"data_region": 42}
	created.Candidates = []biz.PolicyCandidate{invalidCandidate}
	repo.EXPECT().GetPolicy(gomock.Any(), created.ID).Return(created, nil)
	repo.EXPECT().ListModelsByIDs(gomock.Any(), []uuid.UUID{modelID}).Return([]biz.Model{{
		ID: modelID, Kind: biz.ModelKindChat, Status: biz.ModelEnabled, ContextWindowTokens: biz.MinGenerationContextTokens,
		VerificationSource: "admin-console", VerifiedAt: &now, DataRegion: "cn",
	}}, nil)
	if _, err := service.Publish(context.Background(), admin, created.ID); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("invalid constraint type error = %v", err)
	}
}

func TestPolicyServicePublishesOnlyEligibleCandidates(t *testing.T) {
	ctrl := gomock.NewController(t)
	repo := mocks.NewMockPolicyRepository(ctrl)
	admin := biz.Principal{UserID: uuid.New(), Role: biz.RoleAdmin}
	service, err := biz.NewPolicyService(repo)
	if err != nil {
		t.Fatal(err)
	}
	modelID := uuid.New()
	now := time.Now().UTC()
	candidate := biz.PolicyCandidate{ModelID: modelID, Priority: 1}
	policy := biz.ModelPolicy{Name: " production ", Candidates: []biz.PolicyCandidate{candidate}}

	repo.EXPECT().CreatePolicy(gomock.Any(), admin.UserID, gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _ uuid.UUID, created biz.ModelPolicy, _ biz.OutboxEvent) (biz.ModelPolicy, error) {
			if created.Name != "production" || created.Version != 1 || created.Status != biz.PolicyDraft || created.MinContextTokens != biz.MinGenerationContextTokens {
				t.Fatalf("invalid policy defaults: %#v", created)
			}
			return created, nil
		},
	)
	created, err := service.Create(context.Background(), admin, policy)
	if err != nil {
		t.Fatal(err)
	}

	repo.EXPECT().GetPolicy(gomock.Any(), created.ID).Return(created, nil).Times(2)
	repo.EXPECT().ListModelsByIDs(gomock.Any(), []uuid.UUID{modelID}).Return([]biz.Model{{
		ID: modelID, Kind: biz.ModelKindChat, Status: biz.ModelEnabled, ContextWindowTokens: biz.MinGenerationContextTokens,
		VerificationSource: "admin-console", VerifiedAt: &now,
	}}, nil)
	repo.EXPECT().UpdatePolicy(gomock.Any(), admin.UserID, gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _ uuid.UUID, updated biz.ModelPolicy, _ biz.OutboxEvent) (biz.ModelPolicy, error) {
			return updated, nil
		},
	).Times(2)
	if _, err := service.Update(context.Background(), admin, created); err != nil {
		t.Fatal(err)
	}
	published, err := service.Publish(context.Background(), admin, created.ID)
	if err != nil || published.Status != biz.PolicyPublished {
		t.Fatalf("Publish() = %#v, %v", published, err)
	}

	repo.EXPECT().GetPolicy(gomock.Any(), created.ID).Return(created, nil)
	repo.EXPECT().NextPolicyVersion(gomock.Any(), created.Name).Return(2, nil)
	repo.EXPECT().CreatePolicy(gomock.Any(), admin.UserID, gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _ uuid.UUID, next biz.ModelPolicy, _ biz.OutboxEvent) (biz.ModelPolicy, error) {
			if next.Version != 2 || next.ID == created.ID || next.Status != biz.PolicyDraft {
				t.Fatalf("invalid next version: %#v", next)
			}
			return next, nil
		},
	)
	if _, err := service.CreateVersion(context.Background(), admin, created.ID); err != nil {
		t.Fatal(err)
	}

	duplicate := created
	duplicate.Candidates = []biz.PolicyCandidate{candidate, candidate}
	if _, err := service.Create(context.Background(), admin, duplicate); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("duplicate candidate error = %v", err)
	}
	repo.EXPECT().GetPolicy(gomock.Any(), created.ID).Return(published, nil)
	if _, err := service.Update(context.Background(), admin, published); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("published update error = %v", err)
	}
	repo.EXPECT().GetPolicy(gomock.Any(), created.ID).Return(created, nil)
	repo.EXPECT().ListPolicies(gomock.Any()).Return([]biz.ModelPolicy{created}, nil)
	repo.EXPECT().DeletePolicy(gomock.Any(), admin.UserID, created.ID, gomock.Any()).Return(nil)
	if _, err := service.Get(context.Background(), admin, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.List(context.Background(), admin); err != nil {
		t.Fatal(err)
	}
	if err := service.Delete(context.Background(), admin, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.List(context.Background(), biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("non-admin error = %v", err)
	}
	if _, err := biz.NewPolicyService(nil); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("nil dependency error = %v", err)
	}
}

// TestProviderListRemoteModels 验证管理员读取供应商站点模型时的密钥解密与错误边界。
func TestProviderListRemoteModels(t *testing.T) {
	ctrl := gomock.NewController(t)
	repo := mocks.NewMockProviderRepository(ctrl)
	cipher := mocks.NewMockCredentialCipher(ctrl)
	credentials := mocks.NewMockProviderCredentialRepository(ctrl)
	opener := mocks.NewMockCredentialOpener(ctrl)
	lister := mocks.NewMockRemoteModelLister(ctrl)
	service, err := biz.NewProviderService(repo, cipher)
	if err != nil {
		t.Fatal(err)
	}
	service.WithRemoteModelCatalog(credentials, opener, lister)

	admin := biz.Principal{UserID: uuid.New(), Role: biz.RoleAdmin}
	providerID := uuid.New()
	provider := biz.Provider{ID: providerID, Name: "primary", BaseURL: "https://models.example.com", Status: biz.ProviderEnabled}
	credentials.EXPECT().GetProviderCredential(gomock.Any(), providerID).Return(provider, biz.ProviderCredential{Encrypted: []byte("ciphertext"), KeyVersion: "v1"}, nil)
	opener.EXPECT().Open("v1", []byte("ciphertext")).Return([]byte("secret"), nil)
	lister.EXPECT().ListModels(gomock.Any(), provider.BaseURL, "secret").Return([]biz.RemoteModel{{ID: "deepseek-chat", OwnedBy: "deepseek"}}, nil)
	items, err := service.ListRemoteModels(context.Background(), admin, providerID)
	if err != nil || len(items) != 1 || items[0].ID != "deepseek-chat" {
		t.Fatalf("ListRemoteModels() = %#v, %v", items, err)
	}

	if _, err := service.ListRemoteModels(context.Background(), biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}, providerID); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("non-admin remote models error = %v", err)
	}
	plain, err := biz.NewProviderService(repo, cipher)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plain.ListRemoteModels(context.Background(), admin, providerID); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("unconfigured remote models error = %v", err)
	}
}
