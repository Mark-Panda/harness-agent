package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/service"
	"github.com/Mark-Panda/harness-agent/internal/service/mocks"
	"github.com/google/uuid"
	"go.uber.org/mock/gomock"
)

type platformCredentialOpenerFunc func(keyVersion string, ciphertext []byte) ([]byte, error)

func (f platformCredentialOpenerFunc) Open(keyVersion string, ciphertext []byte) ([]byte, error) {
	return f(keyVersion, ciphertext)
}

type platformCredentialAudit struct {
	events []biz.OutboxEvent
}

func (a *platformCredentialAudit) AppendOutboxEvent(_ context.Context, event biz.OutboxEvent) error {
	a.events = append(a.events, event)
	return nil
}

type platformCredentialFeishuRepository struct {
	biz.FeishuBotRepository
	bot        biz.FeishuBot
	credential biz.ProviderCredential
}

func (r *platformCredentialFeishuRepository) GetFeishuBot(_ context.Context, botID uuid.UUID) (biz.FeishuBot, error) {
	if botID != r.bot.ID {
		return biz.FeishuBot{}, biz.ErrResourceNotFound
	}
	return r.bot, nil
}

func (r *platformCredentialFeishuRepository) ListFeishuBots(context.Context) ([]biz.FeishuBot, error) {
	return []biz.FeishuBot{r.bot}, nil
}

func (r *platformCredentialFeishuRepository) ListFeishuBotsForUser(_ context.Context, userID uuid.UUID) ([]biz.FeishuBot, error) {
	if userID != r.bot.OwnerUserID {
		return []biz.FeishuBot{}, nil
	}
	return []biz.FeishuBot{r.bot}, nil
}

func (r *platformCredentialFeishuRepository) GetFeishuBotCredential(_ context.Context, botID uuid.UUID) (biz.FeishuBot, biz.ProviderCredential, error) {
	if botID != r.bot.ID {
		return biz.FeishuBot{}, biz.ProviderCredential{}, biz.ErrResourceNotFound
	}
	return r.bot, r.credential, nil
}

type platformCredentialFixture struct {
	handler             http.Handler
	auth                *fakeAuthApplication
	admin               biz.Principal
	owner               biz.Principal
	providerID          uuid.UUID
	mcpID               uuid.UUID
	botID               uuid.UUID
	providers           *mocks.MockProviderRepository
	providerCredentials *mocks.MockProviderCredentialRepository
	mcps                *mocks.MockMCPRepository
	mcpCredentials      *mocks.MockMCPCredentialRepository
	audit               *platformCredentialAudit
}

func newPlatformCredentialFixture(t *testing.T) *platformCredentialFixture {
	t.Helper()
	ctrl := gomock.NewController(t)
	fixture := &platformCredentialFixture{
		admin:               biz.Principal{UserID: uuid.New(), Role: biz.RoleAdmin},
		owner:               biz.Principal{UserID: uuid.New(), Role: biz.RoleUser},
		providerID:          uuid.New(),
		mcpID:               uuid.New(),
		botID:               uuid.New(),
		providers:           mocks.NewMockProviderRepository(ctrl),
		providerCredentials: mocks.NewMockProviderCredentialRepository(ctrl),
		mcps:                mocks.NewMockMCPRepository(ctrl),
		mcpCredentials:      mocks.NewMockMCPCredentialRepository(ctrl),
		audit:               &platformCredentialAudit{},
	}
	providerCipher := mocks.NewMockCredentialCipher(ctrl)
	providers, err := biz.NewProviderService(fixture.providers, providerCipher)
	if err != nil {
		t.Fatal(err)
	}
	providers.WithRemoteModelCatalog(
		fixture.providerCredentials,
		platformCredentialOpenerFunc(func(keyVersion string, ciphertext []byte) ([]byte, error) {
			if keyVersion != "v1" || string(ciphertext) != "provider-ciphertext" {
				t.Fatalf("unexpected provider credential: %q %q", keyVersion, ciphertext)
			}
			return []byte("provider-secret"), nil
		}),
		mocks.NewMockRemoteModelLister(ctrl),
	).WithRevealAudit(fixture.audit)

	mcpCipher := mocks.NewMockCredentialCipher(ctrl)
	mcps, err := biz.NewMCPService(fixture.mcps, mcpCipher)
	if err != nil {
		t.Fatal(err)
	}
	mcps.WithProber(
		fixture.mcpCredentials,
		platformCredentialOpenerFunc(func(keyVersion string, ciphertext []byte) ([]byte, error) {
			if keyVersion != "v1" || string(ciphertext) != "mcp-ciphertext" {
				t.Fatalf("unexpected MCP credential: %q %q", keyVersion, ciphertext)
			}
			return []byte("mcp-secret"), nil
		}),
		nil,
	).WithRevealAudit(fixture.audit)

	feishuRepository := &platformCredentialFeishuRepository{
		bot: biz.FeishuBot{
			ID: fixture.botID, Name: "credential-bot", AppID: "cli_credential", OwnerUserID: fixture.owner.UserID,
			KeyVersion: "v1", Status: biz.FeishuBotEnabled,
		},
		credential: biz.ProviderCredential{Encrypted: []byte("feishu-ciphertext"), KeyVersion: "v1"},
	}
	feishuBots, err := biz.NewFeishuBotService(
		feishuRepository,
		mocks.NewMockCredentialCipher(ctrl),
		platformCredentialOpenerFunc(func(keyVersion string, ciphertext []byte) ([]byte, error) {
			if keyVersion != "v1" || string(ciphertext) != "feishu-ciphertext" {
				t.Fatalf("unexpected feishu credential: %q %q", keyVersion, ciphertext)
			}
			return []byte(`{"app_secret":"app-secret","verification_token":"verification-secret","encrypt_key":"encrypt-secret"}`), nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	feishuBots.WithRevealAudit(fixture.audit)
	idempotency, err := biz.NewIdempotencyService(newMemoryIdempotencyRepository())
	if err != nil {
		t.Fatal(err)
	}
	platform := &service.PlatformService{
		Providers: providers, MCPs: mcps, Idempotency: idempotency,
		FeishuBots: feishuBots, FeishuBotBindings: &biz.FeishuBotBindingService{},
	}
	fixture.auth = &fakeAuthApplication{principal: fixture.admin}
	fixture.handler = NewHandler(fstest.MapFS{"index.html": {Data: []byte("Harness")}}, fixture.auth, nil, platform)
	return fixture
}

func (f *platformCredentialFixture) request(method, path, body string, authenticated bool, headers map[string]string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if authenticated {
		request.Header.Set("Authorization", "Bearer access")
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)
	return recorder
}

func (f *platformCredentialFixture) do(method, path, body string) *httptest.ResponseRecorder {
	return f.request(method, path, body, true, nil)
}

func assertPlatformCredentialNoStore(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store; response: %d %s", got, recorder.Code, recorder.Body.String())
	}
}

func assertPlatformCredentialError(t *testing.T, recorder *httptest.ResponseRecorder, wantStatus int) {
	t.Helper()
	if recorder.Code != wantStatus {
		t.Fatalf("status = %d, want %d; body: %s", recorder.Code, wantStatus, recorder.Body.String())
	}
	assertPlatformCredentialNoStore(t, recorder)
}

func assertPlatformCredentialResponse(t *testing.T, recorder *httptest.ResponseRecorder, want biz.RevealedCredential) {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", recorder.Code, recorder.Body.String())
	}
	assertPlatformCredentialNoStore(t, recorder)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &fields); err != nil {
		t.Fatalf("decode reveal response: %v; body: %s", err, recorder.Body.String())
	}
	wantFieldCount := 1
	if want.Value != "" {
		wantFieldCount++
	}
	if len(fields) != wantFieldCount {
		t.Fatalf("reveal response fields = %v, want only configured and optional value", fields)
	}
	if _, ok := fields["configured"]; !ok {
		t.Fatalf("reveal response omitted configured: %s", recorder.Body.String())
	}
	if _, ok := fields["value"]; ok != (want.Value != "") {
		t.Fatalf("reveal response value presence is incorrect: %s", recorder.Body.String())
	}
	var got biz.RevealedCredential
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("reveal response = %#v, want %#v", got, want)
	}
}

func TestPlatformCredentialRevealProvider(t *testing.T) {
	fixture := newPlatformCredentialFixture(t)
	fixture.providerCredentials.EXPECT().GetProviderCredential(gomock.Any(), fixture.providerID).Return(
		biz.Provider{ID: fixture.providerID},
		biz.ProviderCredential{Encrypted: []byte("provider-ciphertext"), KeyVersion: "v1"},
		nil,
	)
	recorder := fixture.do(http.MethodPost, "/api/v1/providers/"+fixture.providerID.String()+"/reveal-credential", "")
	assertPlatformCredentialResponse(t, recorder, biz.RevealedCredential{Configured: true, Value: "provider-secret"})
}

func TestPlatformCredentialRevealMCP(t *testing.T) {
	fixture := newPlatformCredentialFixture(t)
	fixture.mcpCredentials.EXPECT().GetMCPCredential(gomock.Any(), fixture.mcpID).Return(
		biz.MCP{ID: fixture.mcpID, AuthType: "bearer"},
		biz.ProviderCredential{Encrypted: []byte("mcp-ciphertext"), KeyVersion: "v1"},
		nil,
	)
	recorder := fixture.do(http.MethodPost, "/api/v1/mcps/"+fixture.mcpID.String()+"/reveal-credential", "")
	assertPlatformCredentialResponse(t, recorder, biz.RevealedCredential{Configured: true, Value: "mcp-secret"})
}

func TestPlatformCredentialRevealFeishuRoutesAndFields(t *testing.T) {
	fixture := newPlatformCredentialFixture(t)
	tests := []struct {
		name      string
		path      string
		principal biz.Principal
		field     string
		value     string
	}{
		{name: "admin app secret", path: "/api/v1/admin/feishu-bots/", principal: fixture.admin, field: "app_secret", value: "app-secret"},
		{name: "owner verification token", path: "/api/v1/feishu/bots/", principal: fixture.owner, field: "verification_token", value: "verification-secret"},
		{name: "admin encrypt key", path: "/api/v1/admin/feishu-bots/", principal: fixture.admin, field: "encrypt_key", value: "encrypt-secret"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture.auth.principal = test.principal
			body := `{"field":"` + test.field + `"}`
			recorder := fixture.do(http.MethodPost, test.path+fixture.botID.String()+"/reveal-credential", body)
			assertPlatformCredentialResponse(t, recorder, biz.RevealedCredential{Configured: true, Value: test.value})
			for _, otherSecret := range []string{"provider-secret", "mcp-secret", "app-secret", "verification-secret", "encrypt-secret"} {
				if otherSecret != test.value && strings.Contains(recorder.Body.String(), otherSecret) {
					t.Fatalf("response disclosed another credential %q: %s", otherSecret, recorder.Body.String())
				}
			}
		})
	}
}

func TestPlatformCredentialRevealAuthenticationErrorsAreNotCacheable(t *testing.T) {
	fixture := newPlatformCredentialFixture(t)
	tests := []struct {
		path string
		body string
	}{
		{path: "/api/v1/providers/" + fixture.providerID.String() + "/reveal-credential"},
		{path: "/api/v1/mcps/" + fixture.mcpID.String() + "/reveal-credential"},
		{path: "/api/v1/admin/feishu-bots/" + fixture.botID.String() + "/reveal-credential", body: `{"field":"app_secret"}`},
		{path: "/api/v1/feishu/bots/" + fixture.botID.String() + "/reveal-credential", body: `{"field":"app_secret"}`},
	}
	for _, test := range tests {
		recorder := fixture.request(http.MethodPost, test.path, test.body, false, nil)
		assertPlatformCredentialError(t, recorder, http.StatusUnauthorized)
	}
}

func TestPlatformCredentialRevealValidationAndAuthorization(t *testing.T) {
	t.Run("provider forbidden", func(t *testing.T) {
		fixture := newPlatformCredentialFixture(t)
		fixture.auth.principal = biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}
		recorder := fixture.do(http.MethodPost, "/api/v1/providers/"+fixture.providerID.String()+"/reveal-credential", "")
		assertPlatformCredentialError(t, recorder, http.StatusForbidden)
	})
	t.Run("MCP forbidden", func(t *testing.T) {
		fixture := newPlatformCredentialFixture(t)
		fixture.auth.principal = biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}
		recorder := fixture.do(http.MethodPost, "/api/v1/mcps/"+fixture.mcpID.String()+"/reveal-credential", "")
		assertPlatformCredentialError(t, recorder, http.StatusForbidden)
	})
	t.Run("feishu cross tenant forbidden", func(t *testing.T) {
		fixture := newPlatformCredentialFixture(t)
		fixture.auth.principal = biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}
		recorder := fixture.do(http.MethodPost, "/api/v1/feishu/bots/"+fixture.botID.String()+"/reveal-credential", `{"field":"app_secret"}`)
		assertPlatformCredentialError(t, recorder, http.StatusForbidden)
	})
	t.Run("feishu invalid field", func(t *testing.T) {
		fixture := newPlatformCredentialFixture(t)
		recorder := fixture.do(http.MethodPost, "/api/v1/admin/feishu-bots/"+fixture.botID.String()+"/reveal-credential", `{"field":"unknown"}`)
		assertPlatformCredentialError(t, recorder, http.StatusBadRequest)
		if !strings.Contains(recorder.Body.String(), "invalid_configuration") || strings.Contains(recorder.Body.String(), "app-secret") {
			t.Fatalf("invalid field response leaked data or used wrong error: %s", recorder.Body.String())
		}
	})
	t.Run("feishu invalid JSON", func(t *testing.T) {
		fixture := newPlatformCredentialFixture(t)
		recorder := fixture.do(http.MethodPost, "/api/v1/admin/feishu-bots/"+fixture.botID.String()+"/reveal-credential", `{`)
		assertPlatformCredentialError(t, recorder, http.StatusBadRequest)
	})
	t.Run("provider invalid ID", func(t *testing.T) {
		fixture := newPlatformCredentialFixture(t)
		recorder := fixture.do(http.MethodPost, "/api/v1/providers/not-a-uuid/reveal-credential", "")
		assertPlatformCredentialError(t, recorder, http.StatusBadRequest)
	})
	t.Run("provider business error", func(t *testing.T) {
		fixture := newPlatformCredentialFixture(t)
		fixture.providerCredentials.EXPECT().GetProviderCredential(gomock.Any(), fixture.providerID).Return(
			biz.Provider{}, biz.ProviderCredential{}, biz.ErrResourceNotFound,
		)
		recorder := fixture.do(http.MethodPost, "/api/v1/providers/"+fixture.providerID.String()+"/reveal-credential", "")
		assertPlatformCredentialError(t, recorder, http.StatusNotFound)
	})
}

func TestPlatformCredentialRevealDoesNotReplayIdempotencyKey(t *testing.T) {
	fixture := newPlatformCredentialFixture(t)
	fixture.providerCredentials.EXPECT().GetProviderCredential(gomock.Any(), fixture.providerID).Return(
		biz.Provider{ID: fixture.providerID},
		biz.ProviderCredential{Encrypted: []byte("provider-ciphertext"), KeyVersion: "v1"},
		nil,
	).Times(2)
	for attempt := 0; attempt < 2; attempt++ {
		recorder := fixture.request(
			http.MethodPost,
			"/api/v1/providers/"+fixture.providerID.String()+"/reveal-credential",
			"",
			true,
			map[string]string{"Idempotency-Key": "reveal-provider-once"},
		)
		assertPlatformCredentialResponse(t, recorder, biz.RevealedCredential{Configured: true, Value: "provider-secret"})
		if recorder.Header().Get("Idempotency-Replayed") != "" {
			t.Fatal("credential reveal must never be replayed from idempotency storage")
		}
	}
	if len(fixture.audit.events) != 2 {
		t.Fatalf("reveal audit count = %d, want 2 explicit reveals", len(fixture.audit.events))
	}
}

func TestPlatformCredentialOrdinaryQueriesRemainRedacted(t *testing.T) {
	fixture := newPlatformCredentialFixture(t)
	fixture.providers.EXPECT().GetProvider(gomock.Any(), fixture.providerID).Return(
		biz.Provider{ID: fixture.providerID, Name: "provider", KeyVersion: "v1"}, nil,
	)
	fixture.providers.EXPECT().ListProviders(gomock.Any()).Return(
		[]biz.Provider{{ID: fixture.providerID, Name: "provider", KeyVersion: "v1"}}, nil,
	)
	fixture.mcps.EXPECT().GetMCP(gomock.Any(), fixture.mcpID).Return(
		biz.MCP{ID: fixture.mcpID, Name: "mcp", AuthType: "bearer", KeyVersion: "v1"}, nil,
	)
	fixture.mcps.EXPECT().ListMCPs(gomock.Any()).Return(
		[]biz.MCP{{ID: fixture.mcpID, Name: "mcp", AuthType: "bearer", KeyVersion: "v1"}}, nil,
	)
	paths := []string{
		"/api/v1/providers/" + fixture.providerID.String(),
		"/api/v1/providers",
		"/api/v1/mcps/" + fixture.mcpID.String(),
		"/api/v1/mcps",
		"/api/v1/admin/feishu-bots/" + fixture.botID.String(),
		"/api/v1/admin/feishu-bots",
	}
	for _, path := range paths {
		recorder := fixture.do(http.MethodGet, path, "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET %s returned %d %s", path, recorder.Code, recorder.Body.String())
		}
		body := recorder.Body.String()
		for _, forbidden := range []string{
			"provider-secret", "mcp-secret", "app-secret", "verification-secret", "encrypt-secret",
			`"api_key"`, `"credentials"`, `"app_secret"`, `"verification_token"`, `"encrypt_key"`, `"value"`,
		} {
			if strings.Contains(body, forbidden) {
				t.Fatalf("GET %s disclosed %q: %s", path, forbidden, body)
			}
		}
	}
}
