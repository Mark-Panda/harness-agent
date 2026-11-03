package biz_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/service/mocks"
	"github.com/google/uuid"
	"go.uber.org/mock/gomock"
)

// TestSkillServiceLifecycleAndImmutability 验证 Skill 草稿管理、发布后不可变更与权限隔离。
func TestSkillServiceLifecycleAndImmutability(t *testing.T) {
	ctrl := gomock.NewController(t)
	repo := mocks.NewMockSkillRepository(ctrl)
	admin := biz.Principal{UserID: uuid.New(), Role: biz.RoleAdmin}
	service, err := biz.NewSkillService(repo)
	if err != nil {
		t.Fatal(err)
	}
	repo.EXPECT().CreateSkill(gomock.Any(), admin.UserID, gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _ uuid.UUID, skill biz.SkillTemplate, _ biz.OutboxEvent) (biz.SkillTemplate, error) {
			if skill.Status != biz.SkillDraft || skill.Version != 1 || skill.ID == uuid.Nil {
				t.Fatalf("invalid skill draft: %#v", skill)
			}
			return skill, nil
		},
	)
	created, err := service.Create(context.Background(), admin, biz.SkillTemplate{Name: " summarize ", PromptTemplate: "Summarize {{input}}"})
	if err != nil {
		t.Fatal(err)
	}
	if created.Name != "summarize" {
		t.Fatalf("skill name was not normalized: %q", created.Name)
	}
	published := created
	published.Status = biz.SkillPublished
	repo.EXPECT().GetSkill(gomock.Any(), created.ID).Return(published, nil)
	if _, err := service.Update(context.Background(), admin, published); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("published skill update error = %v", err)
	}

	repo.EXPECT().GetSkill(gomock.Any(), created.ID).Return(created, nil)
	repo.EXPECT().UpdateSkill(gomock.Any(), admin.UserID, gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _ uuid.UUID, skill biz.SkillTemplate, _ biz.OutboxEvent) (biz.SkillTemplate, error) {
			if skill.Version != created.Version || skill.Name != created.Name {
				t.Fatalf("skill identity was not preserved: %#v", skill)
			}
			return skill, nil
		},
	)
	if _, err := service.Update(context.Background(), admin, biz.SkillTemplate{ID: created.ID, PromptTemplate: "Updated {{input}}"}); err != nil {
		t.Fatal(err)
	}
	repo.EXPECT().GetSkill(gomock.Any(), created.ID).Return(created, nil)
	repo.EXPECT().ListSkills(gomock.Any()).Return([]biz.SkillTemplate{created}, nil)
	repo.EXPECT().DeleteSkill(gomock.Any(), admin.UserID, created.ID, gomock.Any()).Return(nil)
	if _, err := service.Get(context.Background(), admin, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.List(context.Background(), admin); err != nil {
		t.Fatal(err)
	}
	if err := service.Delete(context.Background(), admin, created.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := service.Create(context.Background(), biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}, biz.SkillTemplate{Name: "denied", PromptTemplate: "x"}); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("non-admin create error = %v", err)
	}
	if _, err := service.Create(context.Background(), admin, biz.SkillTemplate{Name: "empty"}); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("empty template error = %v", err)
	}
	if _, err := biz.NewSkillService(nil); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("nil dependency error = %v", err)
	}
}

// TestMCPServiceSSRFAndCredentialHandling 验证端点准入、凭证加密与只写不读语义。
func TestMCPServiceSSRFAndCredentialHandling(t *testing.T) {
	ctrl := gomock.NewController(t)
	repo := mocks.NewMockMCPRepository(ctrl)
	cipher := mocks.NewMockCredentialCipher(ctrl)
	admin := biz.Principal{UserID: uuid.New(), Role: biz.RoleAdmin}
	service, err := biz.NewMCPService(repo, cipher)
	if err != nil {
		t.Fatal(err)
	}
	cipher.EXPECT().Seal([]byte("mcp-token")).Return([]byte("sealed"), "v1", nil)
	repo.EXPECT().CreateMCP(gomock.Any(), admin.UserID, gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _ uuid.UUID, mcp biz.MCP, credential *biz.ProviderCredential, _ biz.OutboxEvent) (biz.MCP, error) {
			if credential == nil || credential.KeyVersion != "v1" || mcp.Endpoint != "https://tools.example.com/mcp" {
				t.Fatalf("invalid MCP create: %#v %#v", mcp, credential)
			}
			return mcp, nil
		},
	)
	created, err := service.Create(context.Background(), admin, biz.MCPInput{
		Name: " tools ", Endpoint: "https://tools.example.com/mcp", AuthType: "bearer", Credentials: "mcp-token", RiskLevel: "medium",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != string(biz.MCPEnabled) || created.Name != "tools" {
		t.Fatalf("invalid MCP defaults: %#v", created)
	}

	// SSRF 与鉴权组合的负向用例：必须全部被领域层拒绝。
	for name, input := range map[string]biz.MCPInput{
		"http endpoint":        {Name: "mcp", Endpoint: "http://tools.example.com", AuthType: "none", RiskLevel: "low"},
		"loopback":             {Name: "mcp", Endpoint: "https://127.0.0.1/mcp", AuthType: "none", RiskLevel: "low"},
		"private network":      {Name: "mcp", Endpoint: "https://10.0.0.8/mcp", AuthType: "none", RiskLevel: "low"},
		"metadata address":     {Name: "mcp", Endpoint: "https://169.254.169.254/latest/meta-data", AuthType: "none", RiskLevel: "low"},
		"localhost suffix":     {Name: "mcp", Endpoint: "https://metadata.local/mcp", AuthType: "none", RiskLevel: "low"},
		"credentials in URL":   {Name: "mcp", Endpoint: "https://user:pass@tools.example.com/mcp", AuthType: "none", RiskLevel: "low"},
		"none with secret":     {Name: "mcp", Endpoint: "https://tools.example.com", AuthType: "none", Credentials: "secret", RiskLevel: "low"},
		"bearer without token": {Name: "mcp", Endpoint: "https://tools.example.com", AuthType: "bearer", RiskLevel: "low"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := service.Create(context.Background(), admin, input); !errors.Is(err, biz.ErrInvalidConfig) {
				t.Fatalf("SSRF/auth validation error = %v", err)
			}
		})
	}

	cipher.EXPECT().Seal([]byte("rotated")).Return([]byte("sealed-2"), "v2", nil)
	repo.EXPECT().GetMCP(gomock.Any(), created.ID).Return(created, nil)
	repo.EXPECT().UpdateMCP(gomock.Any(), admin.UserID, gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _ uuid.UUID, mcp biz.MCP, credential *biz.ProviderCredential, _ biz.OutboxEvent) (biz.MCP, error) {
			if credential == nil || mcp.KeyVersion != "v2" {
				t.Fatal("MCP credential rotation was not forwarded")
			}
			return mcp, nil
		},
	)
	if _, err := service.Update(context.Background(), admin, created.ID, biz.MCPInput{
		Name: "tools", Endpoint: "https://tools.example.com/mcp", AuthType: "bearer", Credentials: "rotated", RiskLevel: "medium",
	}); err != nil {
		t.Fatal(err)
	}
	// 无新凭证时更新必须保留原密钥版本，避免响应丢失 key_version。
	rotated := created
	rotated.KeyVersion = "v2"
	repo.EXPECT().GetMCP(gomock.Any(), created.ID).Return(rotated, nil)
	repo.EXPECT().UpdateMCP(gomock.Any(), admin.UserID, gomock.Any(), gomock.Nil(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _ uuid.UUID, mcp biz.MCP, credential *biz.ProviderCredential, _ biz.OutboxEvent) (biz.MCP, error) {
			if credential != nil || mcp.KeyVersion != "v2" {
				t.Fatalf("key version was not preserved: %#v %#v", mcp, credential)
			}
			return mcp, nil
		},
	)
	if _, err := service.Update(context.Background(), admin, created.ID, biz.MCPInput{
		Name: "tools", Endpoint: "https://tools.example.com/mcp", AuthType: "bearer", RiskLevel: "low",
	}); err != nil {
		t.Fatal(err)
	}
	repo.EXPECT().GetMCP(gomock.Any(), created.ID).Return(created, nil)
	repo.EXPECT().ListMCPs(gomock.Any()).Return([]biz.MCP{created}, nil)
	if _, err := service.Get(context.Background(), admin, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.List(context.Background(), admin); err != nil {
		t.Fatal(err)
	}
	repo.EXPECT().DeleteMCP(gomock.Any(), admin.UserID, created.ID, gomock.Any()).Return(nil)
	if err := service.Delete(context.Background(), admin, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Create(context.Background(), biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}, biz.MCPInput{
		Name: "denied", Endpoint: "https://tools.example.com", AuthType: "none", RiskLevel: "low",
	}); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("non-admin MCP create error = %v", err)
	}
	if _, err := biz.NewMCPService(nil, cipher); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("nil dependency error = %v", err)
	}
}
