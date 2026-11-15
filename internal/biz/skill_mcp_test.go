package biz_test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
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

// fakePlatformSkillBundles 是平台技能包仓储的内存实现。
type fakePlatformSkillBundles struct {
	info  biz.SkillBundleInfo
	files []biz.SkillBundleFile
}

func (f *fakePlatformSkillBundles) ReplaceSkillBundle(_ context.Context, _, _ uuid.UUID, info biz.SkillBundleInfo, files []biz.SkillBundleFile, _ biz.OutboxEvent) error {
	f.info, f.files = info, append([]biz.SkillBundleFile(nil), files...)
	return nil
}

func (f *fakePlatformSkillBundles) ListSkillFileMeta(context.Context, uuid.UUID) ([]biz.SkillBundleFile, error) {
	return append([]biz.SkillBundleFile(nil), f.files...), nil
}

func (f *fakePlatformSkillBundles) GetSkillFile(_ context.Context, _ uuid.UUID, path string) (biz.SkillBundleFile, error) {
	for _, file := range f.files {
		if file.Path == path {
			return file, nil
		}
	}
	return biz.SkillBundleFile{}, biz.ErrSkillBundleNotFound
}

func (f *fakePlatformSkillBundles) SetSkillBundleExecution(_ context.Context, _, _ uuid.UUID, enabled bool, _ biz.OutboxEvent) error {
	f.info.ExecutionEnabled = enabled
	return nil
}

func newPlatformSkillTestArchive(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	entries := []struct{ name, content string }{
		{name: "SKILL.md", content: "---\nname: 平台技能\ndescription: 验证平台技能包\n---\n\n# 平台技能\n\n按步骤执行。\n"},
		{name: "scripts/hello.py", content: "print('hello')\n"},
		{name: "references/guide.md", content: "# 平台指南\n"},
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

func TestSkillServiceBundleLifecycle(t *testing.T) {
	ctrl := gomock.NewController(t)
	repo := mocks.NewMockSkillRepository(ctrl)
	bundles := &fakePlatformSkillBundles{}
	admin := biz.Principal{UserID: uuid.New(), Role: biz.RoleAdmin}
	service, err := biz.NewSkillService(repo)
	if err != nil {
		t.Fatal(err)
	}
	service.WithBundles(bundles)
	var created biz.SkillTemplate
	repo.EXPECT().CreateSkill(gomock.Any(), admin.UserID, gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _ uuid.UUID, skill biz.SkillTemplate, _ biz.OutboxEvent) (biz.SkillTemplate, error) {
			created = skill
			return skill, nil
		},
	)
	repo.EXPECT().GetSkill(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _ uuid.UUID) (biz.SkillTemplate, error) {
			current := created
			current.Bundle = bundles.info
			return current, nil
		},
	).Times(5)

	imported, err := service.ImportBundle(context.Background(), admin, newPlatformSkillTestArchive(t))
	if err != nil || imported.Bundle.FileCount != 3 || !imported.Bundle.HasScripts {
		t.Fatalf("ImportBundle() = %#v, %v", imported, err)
	}
	files, err := service.GetBundleFiles(context.Background(), admin, imported.ID)
	if err != nil || len(files) != 3 || files[0].Owner != biz.SkillFileOwnerPlatform {
		t.Fatalf("GetBundleFiles() = %#v, %v", files, err)
	}
	file, err := service.ReadBundleFile(context.Background(), admin, imported.ID, "references/guide.md")
	if err != nil || string(file.Content) != "# 平台指南\n" {
		t.Fatalf("ReadBundleFile() = %#v, %v", file, err)
	}
	enabled, err := service.SetBundleExecution(context.Background(), admin, imported.ID, true)
	if err != nil || !enabled.Bundle.ExecutionEnabled {
		t.Fatalf("SetBundleExecution() = %#v, %v", enabled, err)
	}
}

// fakeMCPProber 记录探测参数并返回预设工具或错误。
type fakeMCPProber struct {
	tools    []biz.MCPTool
	err      error
	endpoint string
	authType string
	secret   string
}

func (f *fakeMCPProber) ListTools(_ context.Context, endpoint, authType, secret string) ([]biz.MCPTool, error) {
	f.endpoint, f.authType, f.secret = endpoint, authType, secret
	return f.tools, f.err
}

// fakeMCPOpener 返回固定明文凭证。
type fakeMCPOpener struct {
	plain []byte
	err   error
}

func (f fakeMCPOpener) Open(string, []byte) ([]byte, error) { return f.plain, f.err }

// TestMCPServiceInspectConnectivityAndTools 验证连通性测试、工具列表与脱敏错误展示。
func TestMCPServiceInspectConnectivityAndTools(t *testing.T) {
	ctrl := gomock.NewController(t)
	repo := mocks.NewMockMCPRepository(ctrl)
	cipher := mocks.NewMockCredentialCipher(ctrl)
	admin := biz.Principal{UserID: uuid.New(), Role: biz.RoleAdmin}
	service, err := biz.NewMCPService(repo, cipher)
	if err != nil {
		t.Fatal(err)
	}
	mcpID := uuid.New()
	live := biz.MCP{ID: mcpID, Name: "contextforge", Endpoint: "https://tools.example.com/mcp", AuthType: "header", Status: string(biz.MCPEnabled)}
	prober := &fakeMCPProber{tools: []biz.MCPTool{{Name: "query", Description: "查询"}}}
	credentials := mocks.NewMockMCPCredentialRepository(ctrl)
	service.WithProber(credentials, fakeMCPOpener{plain: []byte(`{"Authorization":"Bearer token"}`)}, prober)
	credentials.EXPECT().GetMCPCredential(gomock.Any(), mcpID).Return(live, biz.ProviderCredential{Encrypted: []byte("ciphertext"), KeyVersion: "v1"}, nil)
	result, err := service.Inspect(context.Background(), admin, mcpID)
	if err != nil || !result.OK || len(result.Tools) != 1 || prober.authType != "header" || prober.secret != `{"Authorization":"Bearer token"}` {
		t.Fatalf("inspect = %#v, %v, prober=%#v", result, err, prober)
	}
	if !strings.Contains(result.Message, "1") {
		t.Fatalf("message = %q", result.Message)
	}

	prober.err = errors.New("mcp handshake unavailable")
	credentials.EXPECT().GetMCPCredential(gomock.Any(), mcpID).Return(live, biz.ProviderCredential{Encrypted: []byte("ciphertext"), KeyVersion: "v1"}, nil)
	failed, err := service.Inspect(context.Background(), admin, mcpID)
	if err != nil || failed.OK || !strings.Contains(failed.Message, "连通失败") {
		t.Fatalf("failed inspect = %#v, %v", failed, err)
	}
	if _, err := service.Inspect(context.Background(), biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}, mcpID); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("non-admin inspect error = %v", err)
	}
	noProber, _ := biz.NewMCPService(repo, cipher)
	if _, err := noProber.Inspect(context.Background(), admin, mcpID); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("missing prober error = %v", err)
	}
}

func TestMCPServiceRevealCredential(t *testing.T) {
	t.Run("admin reveals configured credential with safe audit", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		repo := mocks.NewMockMCPRepository(ctrl)
		cipher := mocks.NewMockCredentialCipher(ctrl)
		credentials := mocks.NewMockMCPCredentialRepository(ctrl)
		audit := &fakeOutboxAppender{}
		service, _ := biz.NewMCPService(repo, cipher)
		service.WithProber(credentials, fakeMCPOpener{plain: []byte("mcp-secret")}, nil).WithRevealAudit(audit)
		admin := biz.Principal{UserID: uuid.New(), Role: biz.RoleAdmin}
		mcpID := uuid.New()
		credentials.EXPECT().GetMCPCredential(gomock.Any(), mcpID).Return(
			biz.MCP{ID: mcpID, AuthType: "header"},
			biz.ProviderCredential{Encrypted: []byte("ciphertext"), KeyVersion: "v1"}, nil,
		)
		revealed, err := service.RevealCredential(context.Background(), admin, mcpID)
		if err != nil || !revealed.Configured || revealed.Value != "mcp-secret" {
			t.Fatalf("RevealCredential() = %#v, %v", revealed, err)
		}
		if len(audit.events) != 1 || audit.events[0].EventType != "mcp.credential.revealed" || audit.events[0].Payload["auth_type"] != "header" {
			t.Fatalf("reveal audit = %#v", audit.events)
		}
		if serialized := fmt.Sprintf("%#v", audit.events[0]); strings.Contains(serialized, "mcp-secret") || strings.Contains(serialized, "ciphertext") {
			t.Fatalf("audit contains credential material: %s", serialized)
		}
	})

	t.Run("none auth is unconfigured without decrypting", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		credentials := mocks.NewMockMCPCredentialRepository(ctrl)
		audit := &fakeOutboxAppender{}
		service, _ := biz.NewMCPService(mocks.NewMockMCPRepository(ctrl), mocks.NewMockCredentialCipher(ctrl))
		service.WithProber(credentials, fakeMCPOpener{err: errors.New("must not decrypt")}, nil).WithRevealAudit(audit)
		mcpID := uuid.New()
		credentials.EXPECT().GetMCPCredential(gomock.Any(), mcpID).Return(biz.MCP{ID: mcpID, AuthType: "none"}, biz.ProviderCredential{}, nil)
		revealed, err := service.RevealCredential(context.Background(), biz.Principal{UserID: uuid.New(), Role: biz.RoleAdmin}, mcpID)
		if err != nil || revealed.Configured || revealed.Value != "" || len(audit.events) != 1 {
			t.Fatalf("unconfigured reveal = %#v, %v, audit=%#v", revealed, err, audit.events)
		}
	})

	t.Run("rejects non admin", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		service, _ := biz.NewMCPService(mocks.NewMockMCPRepository(ctrl), mocks.NewMockCredentialCipher(ctrl))
		if _, err := service.RevealCredential(context.Background(), biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}, uuid.New()); !errors.Is(err, biz.ErrForbidden) {
			t.Fatalf("non-admin reveal error = %v", err)
		}
	})

	t.Run("decrypt failure is sanitized and not audited as success", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		credentials := mocks.NewMockMCPCredentialRepository(ctrl)
		audit := &fakeOutboxAppender{}
		service, _ := biz.NewMCPService(mocks.NewMockMCPRepository(ctrl), mocks.NewMockCredentialCipher(ctrl))
		service.WithProber(credentials, fakeMCPOpener{err: errors.New("decrypt failed")}, nil).WithRevealAudit(audit)
		mcpID := uuid.New()
		credentials.EXPECT().GetMCPCredential(gomock.Any(), mcpID).Return(biz.MCP{ID: mcpID, AuthType: "bearer"}, biz.ProviderCredential{Encrypted: []byte("ciphertext"), KeyVersion: "v1"}, nil)
		if _, err := service.RevealCredential(context.Background(), biz.Principal{UserID: uuid.New(), Role: biz.RoleAdmin}, mcpID); !errors.Is(err, biz.ErrInvalidConfig) || len(audit.events) != 0 {
			t.Fatalf("decrypt failure = %v, audit=%#v", err, audit.events)
		}
	})

	t.Run("audit failure returns no credential", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		credentials := mocks.NewMockMCPCredentialRepository(ctrl)
		auditErr := errors.New("audit unavailable")
		audit := &fakeOutboxAppender{err: auditErr}
		service, _ := biz.NewMCPService(mocks.NewMockMCPRepository(ctrl), mocks.NewMockCredentialCipher(ctrl))
		service.WithProber(credentials, fakeMCPOpener{plain: []byte("mcp-secret")}, nil).WithRevealAudit(audit)
		mcpID := uuid.New()
		credentials.EXPECT().GetMCPCredential(gomock.Any(), mcpID).Return(biz.MCP{ID: mcpID, AuthType: "bearer"}, biz.ProviderCredential{Encrypted: []byte("ciphertext"), KeyVersion: "v1"}, nil)
		revealed, err := service.RevealCredential(context.Background(), biz.Principal{UserID: uuid.New(), Role: biz.RoleAdmin}, mcpID)
		if !errors.Is(err, auditErr) || revealed.Configured || revealed.Value != "" {
			t.Fatalf("audit failure = %#v, %v", revealed, err)
		}
	})

	t.Run("missing reveal dependencies", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		service, _ := biz.NewMCPService(mocks.NewMockMCPRepository(ctrl), mocks.NewMockCredentialCipher(ctrl))
		if _, err := service.RevealCredential(context.Background(), biz.Principal{UserID: uuid.New(), Role: biz.RoleAdmin}, uuid.New()); !errors.Is(err, biz.ErrInvalidConfig) {
			t.Fatalf("missing reveal dependencies error = %v", err)
		}
	})
}

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
