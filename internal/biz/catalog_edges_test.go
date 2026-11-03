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

func TestProviderUpdateRotationAndErrors(t *testing.T) {
	ctrl := gomock.NewController(t)
	repo := mocks.NewMockProviderRepository(ctrl)
	cipher := mocks.NewMockCredentialCipher(ctrl)
	service, _ := biz.NewProviderService(repo, cipher)
	admin := biz.Principal{UserID: uuid.New(), Role: biz.RoleAdmin}
	input := biz.ProviderInput{Name: "provider", BaseURL: "https://example.com", APIKey: "new-secret", Status: biz.ProviderDisabled}
	cipher.EXPECT().Seal([]byte("new-secret")).Return([]byte("sealed"), "v2", nil)
	repo.EXPECT().UpdateProvider(gomock.Any(), admin.UserID, gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _ uuid.UUID, provider biz.Provider, credential *biz.ProviderCredential, _ biz.OutboxEvent) (biz.Provider, error) {
			if credential == nil || provider.KeyVersion != "v2" {
				t.Fatal("provider key rotation was not forwarded")
			}
			return provider, nil
		},
	)
	if _, err := service.Update(context.Background(), admin, uuid.New(), input); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Update(context.Background(), biz.Principal{}, uuid.New(), input); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("anonymous update error = %v", err)
	}
	if _, err := service.Get(context.Background(), biz.Principal{}, uuid.New()); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("anonymous get error = %v", err)
	}
	if _, err := service.List(context.Background(), biz.Principal{}); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("anonymous list error = %v", err)
	}
	input.Name = ""
	if _, err := service.Update(context.Background(), admin, uuid.New(), input); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("invalid update error = %v", err)
	}
	input.Name = "provider"
	cipher.EXPECT().Seal([]byte("new-secret")).Return(nil, "", errors.New("seal failed"))
	if _, err := service.Update(context.Background(), admin, uuid.New(), input); err == nil || errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("cipher error = %v", err)
	}
}

func TestModelValidationEdges(t *testing.T) {
	admin := biz.Principal{UserID: uuid.New(), Role: biz.RoleAdmin}
	now := time.Now().UTC()
	valid := biz.Model{
		ID: uuid.New(), ProviderID: uuid.New(), RemoteModelName: "model", Kind: biz.ModelKindChat,
		ContextWindowTokens: biz.MinGenerationContextTokens, MaxOutputTokens: 4096, Status: biz.ModelDisabled,
	}
	tests := map[string]func(*biz.Model){
		"missing id":       func(m *biz.Model) { m.ID = uuid.Nil },
		"missing provider": func(m *biz.Model) { m.ProviderID = uuid.Nil },
		"missing name":     func(m *biz.Model) { m.RemoteModelName = " " },
		"invalid context":  func(m *biz.Model) { m.ContextWindowTokens = 0 },
		"invalid output":   func(m *biz.Model) { m.MaxOutputTokens = 0 },
		"invalid kind":     func(m *biz.Model) { m.Kind = "unknown" },
		"invalid status":   func(m *biz.Model) { m.Status = "unknown" },
		"negative price":   func(m *biz.Model) { m.InputPricePerMillion = -1 },
		"future verification": func(m *biz.Model) {
			m.Status = biz.ModelEnabled
			m.VerificationSource = "admin"
			future := now.Add(time.Hour)
			m.VerifiedAt = &future
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			service, _ := biz.NewModelService(mocks.NewMockModelRepository(ctrl))
			model := valid
			mutate(&model)
			if _, err := service.Update(context.Background(), admin, model); !errors.Is(err, biz.ErrInvalidConfig) {
				t.Fatalf("validation error = %v", err)
			}
		})
	}

	ctrl := gomock.NewController(t)
	repo := mocks.NewMockModelRepository(ctrl)
	service, _ := biz.NewModelService(repo)
	disabledProvider := valid
	disabledProvider.Status = biz.ModelEnabled
	disabledProvider.VerificationSource = "admin"
	disabledProvider.VerifiedAt = &now
	repo.EXPECT().GetProvider(gomock.Any(), valid.ProviderID).Return(biz.Provider{Status: biz.ProviderDisabled}, nil)
	if _, err := service.Update(context.Background(), admin, disabledProvider); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("disabled provider error = %v", err)
	}
	if err := service.Delete(context.Background(), admin, uuid.Nil); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("nil model id error = %v", err)
	}
	if _, err := service.Get(context.Background(), biz.Principal{}, valid.ID); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("anonymous get error = %v", err)
	}
	if _, err := service.List(context.Background(), biz.Principal{}, nil); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("anonymous list error = %v", err)
	}
	if err := service.Delete(context.Background(), biz.Principal{}, valid.ID); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("anonymous delete error = %v", err)
	}
}

func TestPolicyPublicationValidationEdges(t *testing.T) {
	admin := biz.Principal{UserID: uuid.New(), Role: biz.RoleAdmin}
	t.Run("empty and oversized candidate sets", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		repo := mocks.NewMockPolicyRepository(ctrl)
		service, _ := biz.NewPolicyService(repo)
		policyID := uuid.New()
		repo.EXPECT().GetPolicy(gomock.Any(), policyID).Return(biz.ModelPolicy{ID: policyID, Name: "policy", Version: 1, Status: biz.PolicyDraft}, nil)
		if _, err := service.Publish(context.Background(), admin, policyID); !errors.Is(err, biz.ErrInvalidConfig) {
			t.Fatalf("empty publication error = %v", err)
		}
		tooMany := biz.ModelPolicy{Name: "policy", Candidates: make([]biz.PolicyCandidate, 17)}
		if _, err := service.Create(context.Background(), admin, tooMany); !errors.Is(err, biz.ErrInvalidConfig) {
			t.Fatalf("oversized candidate error = %v", err)
		}
		lowMinimum := biz.ModelPolicy{Name: "policy", MinContextTokens: 128000}
		if _, err := service.Create(context.Background(), admin, lowMinimum); !errors.Is(err, biz.ErrInvalidConfig) {
			t.Fatalf("low minimum error = %v", err)
		}
	})

	t.Run("candidate identity and priority", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		service, _ := biz.NewPolicyService(mocks.NewMockPolicyRepository(ctrl))
		for _, candidates := range [][]biz.PolicyCandidate{
			{{Priority: 1}}, {{ModelID: uuid.New()}},
			{{ModelID: uuid.New(), Priority: 1}, {ModelID: uuid.New(), Priority: 1}},
		} {
			if _, err := service.Create(context.Background(), admin, biz.ModelPolicy{Name: "policy", Candidates: candidates}); !errors.Is(err, biz.ErrInvalidConfig) {
				t.Fatalf("candidate validation error = %v", err)
			}
		}
	})

	t.Run("ineligible and missing models", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		repo := mocks.NewMockPolicyRepository(ctrl)
		service, _ := biz.NewPolicyService(repo)
		policyID := uuid.New()
		modelID := uuid.New()
		draft := biz.ModelPolicy{ID: policyID, Name: "policy", Version: 1, Status: biz.PolicyDraft, MinContextTokens: biz.MinGenerationContextTokens, Candidates: []biz.PolicyCandidate{{ModelID: modelID, Priority: 1}}}
		repo.EXPECT().GetPolicy(gomock.Any(), policyID).Return(draft, nil).Times(2)
		repo.EXPECT().ListModelsByIDs(gomock.Any(), []uuid.UUID{modelID}).Return(nil, nil)
		if _, err := service.Publish(context.Background(), admin, policyID); !errors.Is(err, biz.ErrResourceNotFound) {
			t.Fatalf("missing model error = %v", err)
		}
		repo.EXPECT().ListModelsByIDs(gomock.Any(), []uuid.UUID{modelID}).Return([]biz.Model{{ID: modelID, Kind: biz.ModelKindChat, Status: biz.ModelDisabled, ContextWindowTokens: biz.MinGenerationContextTokens}}, nil)
		if _, err := service.Publish(context.Background(), admin, policyID); !errors.Is(err, biz.ErrInvalidConfig) {
			t.Fatalf("disabled candidate error = %v", err)
		}
	})

	t.Run("state and anonymous errors", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		repo := mocks.NewMockPolicyRepository(ctrl)
		service, _ := biz.NewPolicyService(repo)
		policyID := uuid.New()
		repo.EXPECT().GetPolicy(gomock.Any(), policyID).Return(biz.ModelPolicy{ID: policyID, Status: biz.PolicyPublished}, nil)
		if _, err := service.Publish(context.Background(), admin, policyID); !errors.Is(err, biz.ErrResourceConflict) {
			t.Fatalf("republication error = %v", err)
		}
		if _, err := service.Create(context.Background(), biz.Principal{}, biz.ModelPolicy{}); !errors.Is(err, biz.ErrForbidden) {
			t.Fatalf("anonymous create error = %v", err)
		}
		if _, err := service.Get(context.Background(), biz.Principal{}, policyID); !errors.Is(err, biz.ErrForbidden) {
			t.Fatalf("anonymous get error = %v", err)
		}
		if err := service.Delete(context.Background(), biz.Principal{}, policyID); !errors.Is(err, biz.ErrForbidden) {
			t.Fatalf("anonymous delete error = %v", err)
		}
	})
}
