package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	harnessv1 "github.com/Mark-Panda/harness-agent/api/harness/v1"
	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/service"
	"github.com/Mark-Panda/harness-agent/internal/service/mocks"
	"github.com/google/uuid"
	"go.uber.org/mock/gomock"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// errNotImplemented 标记适配层测试中不应被触达的仓储方法。
var errNotImplemented = errors.New("not implemented")

// fakeUserSkillSynthesisStore 同时实现用户技能仓储与提炼设置仓储，
// 用于在 HTTP/gRPC 适配层测试中脱离真实数据库验证身份隔离与设置语义。
type fakeUserSkillSynthesisStore struct {
	settings  map[uuid.UUID]biz.UserSkillSynthesisSettings
	upserted  []biz.UserSkillSynthesisSettings
	getErr    error
	upsertErr error
}

// CreateUserSkill 未被设置接口使用。
func (f *fakeUserSkillSynthesisStore) CreateUserSkill(context.Context, biz.UserSkill, biz.OutboxEvent) (biz.UserSkill, error) {
	return biz.UserSkill{}, errNotImplemented
}

// UpdateUserSkill 未被设置接口使用。
func (f *fakeUserSkillSynthesisStore) UpdateUserSkill(context.Context, biz.UserSkill, biz.OutboxEvent) (biz.UserSkill, error) {
	return biz.UserSkill{}, errNotImplemented
}

// GetUserSkill 未被设置接口使用。
func (f *fakeUserSkillSynthesisStore) GetUserSkill(context.Context, uuid.UUID, uuid.UUID) (biz.UserSkill, error) {
	return biz.UserSkill{}, errNotImplemented
}

// ListUserSkills 未被设置接口使用。
func (f *fakeUserSkillSynthesisStore) ListUserSkills(context.Context, uuid.UUID, int) ([]biz.UserSkill, error) {
	return nil, errNotImplemented
}

// ListUserSkillsByIDs 未被设置接口使用。
func (f *fakeUserSkillSynthesisStore) ListUserSkillsByIDs(context.Context, uuid.UUID, []uuid.UUID) ([]biz.UserSkill, error) {
	return nil, errNotImplemented
}

// PublishUserSkill 未被设置接口使用。
func (f *fakeUserSkillSynthesisStore) PublishUserSkill(context.Context, uuid.UUID, uuid.UUID, biz.OutboxEvent) (biz.UserSkill, error) {
	return biz.UserSkill{}, errNotImplemented
}

// DisableUserSkill 未被设置接口使用。
func (f *fakeUserSkillSynthesisStore) DisableUserSkill(context.Context, uuid.UUID, uuid.UUID, biz.OutboxEvent) (biz.UserSkill, error) {
	return biz.UserSkill{}, errNotImplemented
}

// DeleteUserSkill 未被设置接口使用。
func (f *fakeUserSkillSynthesisStore) DeleteUserSkill(context.Context, uuid.UUID, uuid.UUID, biz.OutboxEvent) error {
	return errNotImplemented
}

// GetUserSkillSynthesisSettings 模拟“无记录即关闭”的默认语义。
func (f *fakeUserSkillSynthesisStore) GetUserSkillSynthesisSettings(_ context.Context, userID uuid.UUID) (biz.UserSkillSynthesisSettings, error) {
	if f.getErr != nil {
		return biz.UserSkillSynthesisSettings{}, f.getErr
	}
	if settings, ok := f.settings[userID]; ok {
		return settings, nil
	}
	return biz.UserSkillSynthesisSettings{UserID: userID}, nil
}

// UpsertUserSkillSynthesisSettings 按用户覆盖保存并记录调用，便于断言身份来源。
func (f *fakeUserSkillSynthesisStore) UpsertUserSkillSynthesisSettings(_ context.Context, settings biz.UserSkillSynthesisSettings, _ biz.OutboxEvent) (biz.UserSkillSynthesisSettings, error) {
	if f.upsertErr != nil {
		return biz.UserSkillSynthesisSettings{}, f.upsertErr
	}
	if f.settings == nil {
		f.settings = make(map[uuid.UUID]biz.UserSkillSynthesisSettings)
	}
	now := time.Now().UTC()
	if previous, ok := f.settings[settings.UserID]; ok {
		settings.CreatedAt = previous.CreatedAt
	} else {
		settings.CreatedAt = now
	}
	settings.UpdatedAt = now
	f.settings[settings.UserID] = settings
	f.upserted = append(f.upserted, settings)
	return settings, nil
}

// ListEnabledUserIDs 仅由后台维护循环使用，适配层测试不应触达。
func (f *fakeUserSkillSynthesisStore) ListEnabledUserIDs(context.Context, []uuid.UUID) ([]uuid.UUID, error) {
	return nil, errNotImplemented
}

// userSkillSynthesisFixture 装配真实 biz/service 与桩仓储，覆盖 HTTP 与 gRPC 两种入口。
type userSkillSynthesisFixture struct {
	handler    http.Handler
	userSkills *biz.UserSkillService
	platform   *service.PlatformService
	principal  biz.Principal
	store      *fakeUserSkillSynthesisStore
	policies   *mocks.MockPolicyRepository
}

// newUserSkillSynthesisFixture 构造当前用户已认证的 HTTP 适配层。
func newUserSkillSynthesisFixture(t *testing.T) *userSkillSynthesisFixture {
	t.Helper()
	ctrl := gomock.NewController(t)
	store := &fakeUserSkillSynthesisStore{}
	policies := mocks.NewMockPolicyRepository(ctrl)
	userSkills, err := biz.NewUserSkillService(store)
	if err != nil {
		t.Fatal(err)
	}
	userSkills.WithSynthesisSettings(store, policies)
	platform, err := service.NewPlatformService(
		&biz.ProviderService{}, &biz.ModelService{}, &biz.PolicyService{},
		&biz.SkillService{}, &biz.MCPService{}, &biz.AgentService{},
		&biz.SessionService{}, &biz.RunService{}, &biz.IdempotencyService{},
	)
	if err != nil {
		t.Fatal(err)
	}
	platform.UserSkills = userSkills
	principal := biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}
	handler := NewHandler(fstest.MapFS{"index.html": {Data: []byte("Harness")}},
		&fakeAuthApplication{principal: principal}, nil, platform)
	return &userSkillSynthesisFixture{
		handler: handler, userSkills: userSkills, platform: platform,
		principal: principal, store: store, policies: policies,
	}
}

// do 发起带 Bearer Token 的请求。
func (f *userSkillSynthesisFixture) do(method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer access")
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)
	return recorder
}

// TestUserSkillSynthesisSettingsHTTPDefaultsAndIdentity 验证默认关闭语义与 Token 身份优先。
func TestUserSkillSynthesisSettingsHTTPDefaultsAndIdentity(t *testing.T) {
	fixture := newUserSkillSynthesisFixture(t)

	// 从未保存过设置时返回默认关闭，且不带模型策略。
	recorder := fixture.do(http.MethodGet, "/api/v1/user-skill-synthesis/settings", "")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"enabled":false`) ||
		!strings.Contains(recorder.Body.String(), `"model_policy_id":null`) {
		t.Fatalf("default settings returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 请求体携带 user_id 必须被拒绝，客户端无法指定归属用户。
	recorder = fixture.do(http.MethodPut, "/api/v1/user-skill-synthesis/settings",
		`{"enabled":true,"user_id":"`+uuid.NewString()+`"}`)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "invalid_json") {
		t.Fatalf("spoofed user_id returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 合法请求落库身份只能来自 Token。
	policyID := uuid.New()
	fixture.policies.EXPECT().GetPolicy(gomock.Any(), policyID).Return(biz.ModelPolicy{
		ID: policyID, Version: 1, Status: biz.PolicyPublished,
	}, nil)
	otherUser := uuid.New()
	recorder = fixture.do(http.MethodPut, "/api/v1/user-skill-synthesis/settings",
		`{"enabled":true,"model_policy_id":"`+policyID.String()+`","model_policy_version":1}`)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"enabled":true`) ||
		!strings.Contains(recorder.Body.String(), `"model_policy_id":"`+policyID.String()+`"`) {
		t.Fatalf("update settings returned %d %s", recorder.Code, recorder.Body.String())
	}
	if len(fixture.store.upserted) != 1 || fixture.store.upserted[0].UserID != fixture.principal.UserID {
		t.Fatalf("settings saved with untrusted identity: %#v", fixture.store.upserted)
	}

	// 再次读取应只看到当前用户自己的设置，其他用户仍为默认关闭。
	recorder = fixture.do(http.MethodGet, "/api/v1/user-skill-synthesis/settings", "")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"enabled":true`) {
		t.Fatalf("get settings returned %d %s", recorder.Code, recorder.Body.String())
	}
	otherSettings, err := fixture.userSkills.GetSynthesisSettings(context.Background(), biz.Principal{UserID: otherUser, Role: biz.RoleUser})
	if err != nil {
		t.Fatal(err)
	}
	if otherSettings.Enabled || otherSettings.ModelPolicyID != nil {
		t.Fatalf("other user inherited settings: %#v", otherSettings)
	}
}

// TestUserSkillSynthesisSettingsHTTPCompleteReplacement 验证完整替换语义与策略清空。
func TestUserSkillSynthesisSettingsHTTPCompleteReplacement(t *testing.T) {
	fixture := newUserSkillSynthesisFixture(t)
	policyID := uuid.New()
	fixture.policies.EXPECT().GetPolicy(gomock.Any(), policyID).Return(biz.ModelPolicy{
		ID: policyID, Version: 2, Status: biz.PolicyPublished,
	}, nil)
	recorder := fixture.do(http.MethodPut, "/api/v1/user-skill-synthesis/settings",
		`{"enabled":true,"model_policy_id":"`+policyID.String()+`","model_policy_version":2}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("enable settings returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 只提交 enabled=false 即完整替换，策略引用必须一并清空。
	recorder = fixture.do(http.MethodPut, "/api/v1/user-skill-synthesis/settings", `{"enabled":false}`)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"enabled":false`) ||
		!strings.Contains(recorder.Body.String(), `"model_policy_id":null`) ||
		!strings.Contains(recorder.Body.String(), `"model_policy_version":null`) {
		t.Fatalf("clear settings returned %d %s", recorder.Code, recorder.Body.String())
	}
	if len(fixture.store.upserted) != 2 || fixture.store.upserted[1].ModelPolicyID != nil || fixture.store.upserted[1].ModelPolicyVersion != nil {
		t.Fatalf("policy reference was not cleared: %#v", fixture.store.upserted)
	}
}

// TestUserSkillSynthesisSettingsHTTPRejectsInvalidPolicyReference 验证策略引用校验的 HTTP 映射。
func TestUserSkillSynthesisSettingsHTTPRejectsInvalidPolicyReference(t *testing.T) {
	policyID := uuid.New()
	cases := []struct {
		name       string
		body       string
		wantStatus int
		wantCode   string
		stubPolicy func(*mocks.MockPolicyRepository)
	}{
		{
			name: "id without version", body: `{"enabled":true,"model_policy_id":"` + policyID.String() + `"}`,
			wantStatus: http.StatusBadRequest, wantCode: "invalid_configuration",
		},
		{
			name: "version without id", body: `{"enabled":true,"model_policy_version":1}`,
			wantStatus: http.StatusBadRequest, wantCode: "invalid_configuration",
		},
		{
			name: "nil id", body: `{"enabled":true,"model_policy_id":"` + uuid.Nil.String() + `","model_policy_version":1}`,
			wantStatus: http.StatusBadRequest, wantCode: "invalid_configuration",
		},
		{
			name: "policy not found", body: `{"enabled":true,"model_policy_id":"` + policyID.String() + `","model_policy_version":1}`,
			wantStatus: http.StatusNotFound, wantCode: "resource_not_found",
			stubPolicy: func(repo *mocks.MockPolicyRepository) {
				repo.EXPECT().GetPolicy(gomock.Any(), policyID).Return(biz.ModelPolicy{}, biz.ErrResourceNotFound)
			},
		},
		{
			name: "draft policy", body: `{"enabled":true,"model_policy_id":"` + policyID.String() + `","model_policy_version":1}`,
			wantStatus: http.StatusBadRequest, wantCode: "invalid_configuration",
			stubPolicy: func(repo *mocks.MockPolicyRepository) {
				repo.EXPECT().GetPolicy(gomock.Any(), policyID).Return(biz.ModelPolicy{
					ID: policyID, Version: 1, Status: biz.PolicyDraft,
				}, nil)
			},
		},
		{
			name: "version mismatch", body: `{"enabled":true,"model_policy_id":"` + policyID.String() + `","model_policy_version":1}`,
			wantStatus: http.StatusBadRequest, wantCode: "invalid_configuration",
			stubPolicy: func(repo *mocks.MockPolicyRepository) {
				repo.EXPECT().GetPolicy(gomock.Any(), policyID).Return(biz.ModelPolicy{
					ID: policyID, Version: 2, Status: biz.PolicyPublished,
				}, nil)
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newUserSkillSynthesisFixture(t)
			if testCase.stubPolicy != nil {
				testCase.stubPolicy(fixture.policies)
			}
			fixture.store.settings = map[uuid.UUID]biz.UserSkillSynthesisSettings{
				fixture.principal.UserID: {UserID: fixture.principal.UserID, Enabled: false},
			}
			recorder := fixture.do(http.MethodPut, "/api/v1/user-skill-synthesis/settings", testCase.body)
			if recorder.Code != testCase.wantStatus || !strings.Contains(recorder.Body.String(), testCase.wantCode) {
				t.Fatalf("returned %d %s, want %d %s", recorder.Code, recorder.Body.String(), testCase.wantStatus, testCase.wantCode)
			}
			if len(fixture.store.upserted) != 0 {
				t.Fatal("invalid settings must not be persisted")
			}
		})
	}
}

// TestListUserSkillSynthesisPoliciesHTTP 验证普通用户只看到已发布策略。
func TestListUserSkillSynthesisPoliciesHTTP(t *testing.T) {
	fixture := newUserSkillSynthesisFixture(t)
	publishedID, draftID := uuid.New(), uuid.New()
	fixture.policies.EXPECT().ListPolicies(gomock.Any()).Return([]biz.ModelPolicy{
		{ID: publishedID, Name: "默认策略", Version: 3, Status: biz.PolicyPublished},
		{ID: draftID, Name: "草稿策略", Version: 1, Status: biz.PolicyDraft},
	}, nil)

	recorder := fixture.do(http.MethodGet, "/api/v1/user-skill-synthesis/policies", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("list policies returned %d %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, publishedID.String()) || !strings.Contains(body, `"version":3`) {
		t.Fatalf("published policy missing: %s", body)
	}
	if strings.Contains(body, draftID.String()) {
		t.Fatalf("draft policy leaked to user: %s", body)
	}
}

// TestListUserSkillSynthesisPoliciesGRPC 验证 gRPC 可选策略列表与鉴权。
func TestListUserSkillSynthesisPoliciesGRPC(t *testing.T) {
	fixture := newUserSkillSynthesisFixture(t)
	server := &platformGRPCServer{platform: fixture.platform}
	if _, err := server.ListUserSkillSynthesisPolicies(context.Background(), &harnessv1.ListUserSkillSynthesisPoliciesRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("unauthenticated list error = %v", err)
	}
	publishedID := uuid.New()
	fixture.policies.EXPECT().ListPolicies(gomock.Any()).Return([]biz.ModelPolicy{
		{ID: publishedID, Name: "默认策略", Version: 2, Status: biz.PolicyPublished},
	}, nil)
	response, err := server.ListUserSkillSynthesisPolicies(
		biz.WithPrincipal(context.Background(), fixture.principal), &harnessv1.ListUserSkillSynthesisPoliciesRequest{})
	if err != nil {
		t.Fatalf("list policies error = %v", err)
	}
	items := response.GetItems()
	if len(items) != 1 || items[0].GetId() != publishedID.String() || items[0].GetName() != "默认策略" || items[0].GetVersion() != 2 {
		t.Fatalf("unexpected policy options: %#v", items)
	}
}

// TestUserSkillSynthesisSettingsGRPC 验证 gRPC 转换、鉴权与策略校验。
func TestUserSkillSynthesisSettingsGRPC(t *testing.T) {
	ctx := func(principal biz.Principal) context.Context {
		return biz.WithPrincipal(context.Background(), principal)
	}

	t.Run("unauthenticated", func(t *testing.T) {
		fixture := newUserSkillSynthesisFixture(t)
		server := &platformGRPCServer{platform: fixture.platform}
		if _, err := server.GetUserSkillSynthesisSettings(context.Background(), &harnessv1.GetUserSkillSynthesisSettingsRequest{}); status.Code(err) != codes.Unauthenticated {
			t.Fatalf("unauthenticated get error = %v", err)
		}
		if _, err := server.UpdateUserSkillSynthesisSettings(context.Background(), &harnessv1.UpdateUserSkillSynthesisSettingsRequest{Enabled: true}); status.Code(err) != codes.Unauthenticated {
			t.Fatalf("unauthenticated update error = %v", err)
		}
	})

	t.Run("default and update", func(t *testing.T) {
		fixture := newUserSkillSynthesisFixture(t)
		server := &platformGRPCServer{platform: fixture.platform}
		principal := fixture.principal

		response, err := server.GetUserSkillSynthesisSettings(ctx(principal), &harnessv1.GetUserSkillSynthesisSettingsRequest{})
		if err != nil {
			t.Fatalf("get settings error = %v", err)
		}
		if response.GetSettings() == nil || response.GetSettings().GetEnabled() ||
			response.GetSettings().ModelPolicyId != nil || response.GetSettings().GetCreatedAt() != nil {
			t.Fatalf("unexpected default settings: %#v", response.GetSettings())
		}

		policyID := uuid.New()
		fixture.policies.EXPECT().GetPolicy(gomock.Any(), policyID).Return(biz.ModelPolicy{
			ID: policyID, Version: 3, Status: biz.PolicyPublished,
		}, nil)
		policyIDText := policyID.String()
		version := int32(3)
		updated, err := server.UpdateUserSkillSynthesisSettings(ctx(principal), &harnessv1.UpdateUserSkillSynthesisSettingsRequest{
			Enabled: true, ModelPolicyId: &policyIDText, ModelPolicyVersion: &version,
		})
		if err != nil {
			t.Fatalf("update settings error = %v", err)
		}
		settings := updated.GetSettings()
		if settings.GetEnabled() != true || settings.GetModelPolicyId() != policyIDText ||
			settings.GetModelPolicyVersion() != version || settings.GetCreatedAt() == nil {
			t.Fatalf("unexpected updated settings: %#v", settings)
		}
		if len(fixture.store.upserted) != 1 || fixture.store.upserted[0].UserID != principal.UserID {
			t.Fatalf("gRPC settings used untrusted identity: %#v", fixture.store.upserted)
		}

		// 省略可选字段即完整替换并清空策略引用。
		cleared, err := server.UpdateUserSkillSynthesisSettings(ctx(principal), &harnessv1.UpdateUserSkillSynthesisSettingsRequest{})
		if err != nil {
			t.Fatalf("clear settings error = %v", err)
		}
		if cleared.GetSettings().GetEnabled() || cleared.GetSettings().ModelPolicyId != nil ||
			cleared.GetSettings().ModelPolicyVersion != nil {
			t.Fatalf("expected cleared settings, got %#v", cleared.GetSettings())
		}
	})

	t.Run("invalid policy reference", func(t *testing.T) {
		fixture := newUserSkillSynthesisFixture(t)
		server := &platformGRPCServer{platform: fixture.platform}
		principal := fixture.principal

		badID := "not-a-uuid"
		if _, err := server.UpdateUserSkillSynthesisSettings(ctx(principal), &harnessv1.UpdateUserSkillSynthesisSettingsRequest{
			ModelPolicyId: &badID,
		}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("invalid policy id error = %v", err)
		}

		version := int32(1)
		if _, err := server.UpdateUserSkillSynthesisSettings(ctx(principal), &harnessv1.UpdateUserSkillSynthesisSettingsRequest{
			ModelPolicyVersion: &version,
		}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("version without id error = %v", err)
		}

		draftID := uuid.New()
		fixture.policies.EXPECT().GetPolicy(gomock.Any(), draftID).Return(biz.ModelPolicy{
			ID: draftID, Version: 1, Status: biz.PolicyDraft,
		}, nil)
		draftIDText := draftID.String()
		one := int32(1)
		if _, err := server.UpdateUserSkillSynthesisSettings(ctx(principal), &harnessv1.UpdateUserSkillSynthesisSettingsRequest{
			Enabled: true, ModelPolicyId: &draftIDText, ModelPolicyVersion: &one,
		}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("draft policy error = %v", err)
		}
		if len(fixture.store.upserted) != 0 {
			t.Fatal("invalid gRPC settings must not be persisted")
		}
	})
}
