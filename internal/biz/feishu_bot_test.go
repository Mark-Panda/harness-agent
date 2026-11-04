package biz_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
)

// fixedCipher 返回固定密钥版本的密文，便于断言加密路径。
type fixedCipher struct{}

// Seal 生成带前缀的测试密文。
func (fixedCipher) Seal(plaintext []byte) ([]byte, string, error) {
	return []byte("sealed:" + string(plaintext)), "v1", nil
}

// jsonOpener 解析测试密文并返回原始 JSON。
type jsonOpener struct{}

// Open 去掉测试前缀后返回明文。
func (jsonOpener) Open(_ string, ciphertext []byte) ([]byte, error) {
	if !strings.HasPrefix(string(ciphertext), "sealed:") {
		return nil, errors.New("invalid test ciphertext")
	}
	return []byte(strings.TrimPrefix(string(ciphertext), "sealed:")), nil
}

// fakeFeishuBotRepository 是机器人配置与绑定的内存仓库。
type fakeFeishuBotRepository struct {
	bots        map[uuid.UUID]biz.FeishuBot
	credentials map[uuid.UUID]biz.ProviderCredential
	bindings    map[uuid.UUID]biz.FeishuBotBinding
	lastEvent   biz.OutboxEvent
}

func newFakeFeishuBotRepository() *fakeFeishuBotRepository {
	return &fakeFeishuBotRepository{
		bots: map[uuid.UUID]biz.FeishuBot{}, credentials: map[uuid.UUID]biz.ProviderCredential{},
		bindings: map[uuid.UUID]biz.FeishuBotBinding{},
	}
}

func (r *fakeFeishuBotRepository) CreateFeishuBot(_ context.Context, _ uuid.UUID, bot biz.FeishuBot, credential biz.ProviderCredential, event biz.OutboxEvent) (biz.FeishuBot, error) {
	r.bots[bot.ID] = bot
	r.credentials[bot.ID] = credential
	r.lastEvent = event
	return bot, nil
}

func (r *fakeFeishuBotRepository) UpdateFeishuBot(_ context.Context, _ uuid.UUID, bot biz.FeishuBot, credential *biz.ProviderCredential, event biz.OutboxEvent) (biz.FeishuBot, error) {
	r.bots[bot.ID] = bot
	if credential != nil {
		r.credentials[bot.ID] = *credential
	}
	r.lastEvent = event
	return bot, nil
}

func (r *fakeFeishuBotRepository) DeleteFeishuBot(_ context.Context, _, botID uuid.UUID, event biz.OutboxEvent) error {
	delete(r.bots, botID)
	delete(r.credentials, botID)
	delete(r.bindings, botID)
	r.lastEvent = event
	return nil
}

func (r *fakeFeishuBotRepository) GetFeishuBot(_ context.Context, botID uuid.UUID) (biz.FeishuBot, error) {
	bot, exists := r.bots[botID]
	if !exists {
		return biz.FeishuBot{}, biz.ErrResourceNotFound
	}
	return bot, nil
}

func (r *fakeFeishuBotRepository) ListFeishuBots(context.Context) ([]biz.FeishuBot, error) {
	result := make([]biz.FeishuBot, 0, len(r.bots))
	for _, bot := range r.bots {
		if binding, bound := r.bindings[bot.ID]; bound {
			bot.BoundUserID = &binding.UserID
			bot.BoundAgentID = &binding.AgentID
			bot.BoundAgentName = binding.AgentName
			bot.BoundAgentVersion = &binding.AgentVersion
		}
		result = append(result, bot)
	}
	return result, nil
}

func (r *fakeFeishuBotRepository) GetFeishuBotCredential(ctx context.Context, botID uuid.UUID) (biz.FeishuBot, biz.ProviderCredential, error) {
	bot, err := r.GetFeishuBot(ctx, botID)
	if err != nil {
		return biz.FeishuBot{}, biz.ProviderCredential{}, err
	}
	return bot, r.credentials[botID], nil
}

func (r *fakeFeishuBotRepository) ListFeishuBotsForUser(_ context.Context, userID uuid.UUID) ([]biz.FeishuBot, error) {
	result := make([]biz.FeishuBot, 0, len(r.bots))
	for _, bot := range r.bots {
		if bot.Status != biz.FeishuBotEnabled {
			continue
		}
		binding, bound := r.bindings[bot.ID]
		if bound {
			bot.BoundUserID = &binding.UserID
			if binding.UserID == userID {
				bot.BoundAgentID = &binding.AgentID
				bot.BoundAgentName = binding.AgentName
			}
		}
		result = append(result, bot)
	}
	return result, nil
}

func (r *fakeFeishuBotRepository) BindFeishuBot(_ context.Context, binding biz.FeishuBotBinding, event biz.OutboxEvent) error {
	if _, exists := r.bindings[binding.BotID]; exists {
		return biz.ErrResourceConflict
	}
	for _, existing := range r.bindings {
		if existing.AgentID == binding.AgentID {
			return biz.ErrResourceConflict
		}
	}
	r.bindings[binding.BotID] = binding
	r.lastEvent = event
	return nil
}

func (r *fakeFeishuBotRepository) UnbindFeishuBot(_ context.Context, userID, agentID uuid.UUID, event biz.OutboxEvent) (int, error) {
	for botID, binding := range r.bindings {
		if binding.AgentID == agentID && binding.UserID == userID {
			delete(r.bindings, botID)
			r.lastEvent = event
			return 1, nil
		}
	}
	return 0, nil
}

func (r *fakeFeishuBotRepository) LookupFeishuBot(_ context.Context, appID string) (biz.FeishuBot, bool, error) {
	for _, bot := range r.bots {
		if bot.AppID == appID {
			return bot, true, nil
		}
	}
	return biz.FeishuBot{}, false, nil
}

func (r *fakeFeishuBotRepository) FindFeishuBotBinding(_ context.Context, botID uuid.UUID) (biz.FeishuBotBinding, bool, error) {
	binding, exists := r.bindings[botID]
	if !exists {
		return biz.FeishuBotBinding{}, false, nil
	}
	bot := r.bots[botID]
	binding.BotName = bot.Name
	binding.BotOpenID = bot.BotOpenID
	return binding, true, nil
}

// fakeFeishuBotTester 记录测试调用并返回预设错误。
type fakeFeishuBotTester struct {
	appID     string
	appSecret string
	err       error
}

func (f *fakeFeishuBotTester) TestBot(_ context.Context, appID, appSecret string) error {
	f.appID, f.appSecret = appID, appSecret
	return f.err
}

// fakeAgentLookup 按用户返回助手，模拟用户隔离查询。
type fakeAgentLookup struct {
	agents map[uuid.UUID]biz.Agent
}

func (f *fakeAgentLookup) GetAgent(_ context.Context, userID, agentID uuid.UUID) (biz.Agent, error) {
	agent, exists := f.agents[agentID]
	if !exists || agent.UserID != userID {
		return biz.Agent{}, biz.ErrResourceNotFound
	}
	return agent, nil
}

// TestFeishuBotServiceAdminLifecycleAndCredentialRetention 验证管理员 CRUD、密钥加密与更新保留。
func TestFeishuBotServiceAdminLifecycleAndCredentialRetention(t *testing.T) {
	repo := newFakeFeishuBotRepository()
	service, err := biz.NewFeishuBotService(repo, fixedCipher{}, jsonOpener{})
	if err != nil {
		t.Fatal(err)
	}
	admin := biz.Principal{UserID: uuid.New(), Role: biz.RoleAdmin}
	input := biz.FeishuBotInput{
		Name: "团队机器人", AppID: "cli_team", AppSecret: "secret", VerificationToken: "verify", EncryptKey: "encrypt",
		BotOpenID: "ou_bot", Status: biz.FeishuBotEnabled,
	}
	bot, err := service.Create(context.Background(), admin, input)
	if err != nil {
		t.Fatal(err)
	}
	credential := repo.credentials[bot.ID]
	var secret biz.FeishuBotSecret
	if err := json.Unmarshal([]byte(strings.TrimPrefix(string(credential.Encrypted), "sealed:")), &secret); err != nil {
		t.Fatal(err)
	}
	if secret.AppSecret != "secret" || secret.VerificationToken != "verify" || secret.EncryptKey != "encrypt" || credential.KeyVersion != "v1" {
		t.Fatalf("stored credential = %#v", secret)
	}
	if _, err := service.Create(context.Background(), biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}, input); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("non-admin create error = %v", err)
	}
	if _, err := service.Create(context.Background(), admin, biz.FeishuBotInput{Name: "no secret", AppID: "cli_nosecret"}); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("missing secret error = %v", err)
	}

	// 只改名称时保留原密文。
	updated, err := service.Update(context.Background(), admin, bot.ID, biz.FeishuBotInput{Name: "团队机器人 v2", AppID: "cli_team", Status: biz.FeishuBotEnabled})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != "团队机器人 v2" || repo.credentials[bot.ID].Encrypted == nil || repo.credentials[bot.ID].KeyVersion != "v1" {
		t.Fatalf("credential was not retained: %#v", repo.credentials[bot.ID])
	}

	// 填写新密钥时整体重建密文。
	if _, err := service.Update(context.Background(), admin, bot.ID, biz.FeishuBotInput{
		Name: "团队机器人 v2", AppID: "cli_team", AppSecret: "new-secret", Status: biz.FeishuBotEnabled,
	}); err != nil {
		t.Fatal(err)
	}
	var rotated biz.FeishuBotSecret
	if err := json.Unmarshal([]byte(strings.TrimPrefix(string(repo.credentials[bot.ID].Encrypted), "sealed:")), &rotated); err != nil {
		t.Fatal(err)
	}
	if rotated.AppSecret != "new-secret" || rotated.VerificationToken != "verify" || rotated.EncryptKey != "encrypt" {
		t.Fatalf("rotated credential = %#v", rotated)
	}

	if _, err := service.List(context.Background(), admin); err != nil || len(repo.bots) != 1 {
		t.Fatalf("list admin error = %v", err)
	}
	if _, err := service.Get(context.Background(), admin, bot.ID); err != nil {
		t.Fatal(err)
	}

	// 连通性测试使用解密后的 App ID/Secret，不向前端返回密钥。
	tester := &fakeFeishuBotTester{}
	service.WithTester(tester)
	result, err := service.Test(context.Background(), admin, bot.ID)
	if err != nil || !result.OK || tester.appID != "cli_team" || tester.appSecret != "new-secret" {
		t.Fatalf("test result = %#v, %v, tester=%#v", result, err, tester)
	}
	tester.err = errors.New("feishu token endpoint unavailable")
	failed, err := service.Test(context.Background(), admin, bot.ID)
	if err != nil || failed.OK || !strings.Contains(failed.Message, "连通失败") {
		t.Fatalf("failed test result = %#v, %v", failed, err)
	}
	if _, err := service.Test(context.Background(), biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}, bot.ID); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("non-admin test error = %v", err)
	}
	noTester, _ := biz.NewFeishuBotService(repo, fixedCipher{}, jsonOpener{})
	if _, err := noTester.Test(context.Background(), admin, bot.ID); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("missing tester error = %v", err)
	}

	if err := service.Delete(context.Background(), admin, bot.ID); err != nil || len(repo.bots) != 0 {
		t.Fatalf("delete error = %v", err)
	}
}

// TestFeishuBotBindingEnforcesPublishedOwnershipAndExclusivity 验证绑定仅限已发布助手且机器人全局唯一。
func TestFeishuBotBindingEnforcesPublishedOwnershipAndExclusivity(t *testing.T) {
	repo := newFakeFeishuBotRepository()
	owner := biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}
	other := biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}
	publishedID, secondPublishedID, draftID, otherID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	agents := &fakeAgentLookup{agents: map[uuid.UUID]biz.Agent{
		publishedID:       {ID: publishedID, UserID: owner.UserID, Name: "已发布助手", Version: 2, Status: biz.AgentPublished, Mode: biz.AgentModeReact},
		secondPublishedID: {ID: secondPublishedID, UserID: owner.UserID, Name: "另一个已发布助手", Version: 1, Status: biz.AgentPublished, Mode: biz.AgentModeReact},
		draftID:           {ID: draftID, UserID: owner.UserID, Name: "草稿助手", Version: 1, Status: biz.AgentDraft, Mode: biz.AgentModeReact},
		otherID:           {ID: otherID, UserID: other.UserID, Name: "他人助手", Version: 1, Status: biz.AgentPublished, Mode: biz.AgentModeReact},
	}}
	botID, disabledID := uuid.New(), uuid.New()
	repo.bots[botID] = biz.FeishuBot{ID: botID, Name: "共享机器人", AppID: "cli_shared", Status: biz.FeishuBotEnabled}
	repo.bots[disabledID] = biz.FeishuBot{ID: disabledID, Name: "停用机器人", AppID: "cli_disabled", Status: biz.FeishuBotDisabled}
	service, err := biz.NewFeishuBotBindingService(repo, repo, agents)
	if err != nil {
		t.Fatal(err)
	}

	if err := service.Bind(context.Background(), owner, draftID, botID); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("draft bind error = %v", err)
	}
	if err := service.Bind(context.Background(), owner, publishedID, disabledID); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("disabled bot bind error = %v", err)
	}
	if err := service.Bind(context.Background(), other, publishedID, botID); !errors.Is(err, biz.ErrResourceNotFound) {
		t.Fatalf("cross-user bind error = %v", err)
	}
	if err := service.Bind(context.Background(), owner, publishedID, botID); err != nil {
		t.Fatalf("bind error = %v", err)
	}

	// 机器人已被占用时，另一个助手（包括其他用户）都不能再次绑定。
	if err := service.Bind(context.Background(), owner, secondPublishedID, botID); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("duplicate bot bind error = %v", err)
	}
	if err := service.Bind(context.Background(), other, otherID, botID); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("other user duplicate bind error = %v", err)
	}

	// 所有者看到自己的绑定，其他用户只看到机器人不可用。
	ownerOptions, err := service.List(context.Background(), owner)
	if err != nil || len(ownerOptions) != 1 || ownerOptions[0].Available || ownerOptions[0].BoundAgentID == nil || *ownerOptions[0].BoundAgentID != publishedID {
		t.Fatalf("owner options = %#v, %v", ownerOptions, err)
	}
	otherOptions, err := service.List(context.Background(), other)
	if err != nil || len(otherOptions) != 1 || otherOptions[0].Available || otherOptions[0].BoundAgentID != nil {
		t.Fatalf("other options = %#v, %v", otherOptions, err)
	}

	// 解绑后机器人重新可用。
	if err := service.Unbind(context.Background(), owner, publishedID); err != nil {
		t.Fatalf("unbind error = %v", err)
	}
	options, err := service.List(context.Background(), owner)
	if err != nil || len(options) != 1 || !options[0].Available {
		t.Fatalf("options after unbind = %#v, %v", options, err)
	}
	// 重复解绑幂等。
	if err := service.Unbind(context.Background(), owner, publishedID); err != nil {
		t.Fatalf("idempotent unbind error = %v", err)
	}
	if err := service.Unbind(context.Background(), biz.Principal{}, publishedID); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("anonymous unbind error = %v", err)
	}
}
