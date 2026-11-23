package bootstrap

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/data"
	"github.com/Mark-Panda/harness-agent/internal/platform/audit"
	"github.com/Mark-Panda/harness-agent/internal/platform/auth"
	"github.com/Mark-Panda/harness-agent/internal/platform/config"
	platformcrypto "github.com/Mark-Panda/harness-agent/internal/platform/crypto"
	"github.com/Mark-Panda/harness-agent/internal/platform/ratelimit"
	"github.com/Mark-Panda/harness-agent/internal/server"
	"github.com/Mark-Panda/harness-agent/internal/server/static"
	"github.com/Mark-Panda/harness-agent/internal/service"
	"github.com/Mark-Panda/harness-agent/pkg/agent/model"
	"github.com/Mark-Panda/harness-agent/pkg/feishu"
	"github.com/Mark-Panda/harness-agent/pkg/logging"
	"github.com/Mark-Panda/harness-agent/pkg/mcp"
	"github.com/Mark-Panda/harness-agent/pkg/sandbox"
	"github.com/go-kratos/kratos/v3"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// 进程角色。api 只提供接入与查询，worker 只消费持久化任务，all 兼容单进程部署。
const (
	RoleAPI    = "api"
	RoleWorker = "worker"
	RoleAll    = "all"
)

// normalizeRole 归一化进程角色；空值保持单进程兼容。
func normalizeRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case RoleAPI:
		return RoleAPI
	case RoleWorker:
		return RoleWorker
	default:
		return RoleAll
	}
}

// remoteModelLister 使用 OpenAI-compatible /models 接口读取供应商可用模型。
type remoteModelLister struct{}

// ListModels 调用供应商模型目录；只返回模型名与归属方，不记录密钥。
func (remoteModelLister) ListModels(ctx context.Context, baseURL, apiKey string) ([]biz.RemoteModel, error) {
	client, err := model.NewClient(baseURL, apiKey, 20*time.Second)
	if err != nil {
		return nil, err
	}
	remote, err := client.Models(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]biz.RemoteModel, 0, len(remote))
	for _, item := range remote {
		items = append(items, biz.RemoteModel{ID: item.ID, OwnedBy: item.OwnedBy})
	}
	return items, nil
}

// feishuBotSenderResolver 校验登记状态并复用同一凭证版本的客户端。
type feishuBotSenderResolver struct {
	credentials biz.FeishuBotCredentialReader
	opener      biz.CredentialOpener
	mu          sync.Mutex
	senders     map[uuid.UUID]feishuBotSender
}

type feishuBotSender struct {
	appID      string
	keyVersion string
	digest     [sha256.Size]byte
	client     *feishu.Client
}

// feishuNoticeSender 发送与 Run 无关的系统提示，复用回复 Worker 的机器人路由与凭据。
type feishuNoticeSender struct {
	defaultAppID  string
	defaultSender biz.FeishuReplySender
	senders       *feishuBotSenderResolver
}

// SendNotice 按事件来源选择默认应用或已登记机器人发送纯文本提示。
func (s *feishuNoticeSender) SendNotice(ctx context.Context, route biz.FeishuNoticeRoute, text string) error {
	var sender biz.FeishuReplySender
	switch {
	case route.DefaultRoute && route.BotID == uuid.Nil:
		if route.AppID != "" && route.AppID != s.defaultAppID {
			return fmt.Errorf("feishu notice app %q does not match default app", route.AppID)
		}
		sender = s.defaultSender
	case route.BotID != uuid.Nil && s.senders != nil:
		resolved, err := s.senders.ResolveSender(ctx, route.BotID, route.AppID)
		if err != nil {
			return err
		}
		sender = resolved
	default:
		return fmt.Errorf("feishu notice route is unavailable")
	}
	if sender == nil {
		return fmt.Errorf("feishu notice sender is not configured")
	}
	content, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return err
	}
	inThread := route.ThreadID != "" && route.ThreadID != "p2p"
	_, err = sender.Reply(ctx, route.ReplyToMessageID, feishu.ReplyRequest{
		Type: "text", Content: string(content), InThread: inThread,
	})
	// 话题能力未开通时退化为普通回复，与回复 Worker 的降级策略保持一致。
	if err != nil && inThread && feishu.IsThreadReplyUnsupported(err) {
		_, err = sender.Reply(ctx, route.ReplyToMessageID, feishu.ReplyRequest{Type: "text", Content: string(content)})
	}
	return err
}

// ResolveSender 使用收到入站事件的机器人 UUID 读取当前身份、状态与凭据。
func (r *feishuBotSenderResolver) ResolveSender(ctx context.Context, botID uuid.UUID, appID string) (biz.FeishuReplySender, error) {
	bot, credential, err := r.credentials.GetFeishuBotCredential(ctx, botID)
	if err != nil {
		return nil, err
	}
	if bot.ID != botID || botID == uuid.Nil || bot.AppID != appID || bot.Status != biz.FeishuBotEnabled {
		return nil, fmt.Errorf("feishu outbound bot route is unavailable: %s", botID)
	}
	digest := sha256.Sum256(credential.Encrypted)
	r.mu.Lock()
	defer r.mu.Unlock()
	cached := r.senders[botID]
	if cached.client != nil && cached.appID == appID && cached.keyVersion == credential.KeyVersion && cached.digest == digest {
		return cached.client, nil
	}
	plaintext, err := r.opener.Open(credential.KeyVersion, credential.Encrypted)
	if err != nil {
		return nil, err
	}
	var secret biz.FeishuBotSecret
	if err := json.Unmarshal(plaintext, &secret); err != nil {
		return nil, fmt.Errorf("decode feishu bot credentials: %w", err)
	}
	client, err := feishu.NewClient(feishu.ClientConfig{AppID: appID, AppSecret: secret.AppSecret})
	if err != nil {
		return nil, err
	}
	if r.senders == nil {
		r.senders = make(map[uuid.UUID]feishuBotSender)
	}
	r.senders[botID] = feishuBotSender{appID: appID, keyVersion: credential.KeyVersion, digest: digest, client: client}
	return client, nil
}

// loadFeishuLongConnectionBots 读取启用机器人的应用凭据，用于建立长连接。
func loadFeishuLongConnectionBots(ctx context.Context, repo biz.FeishuBotRepository, opener biz.CredentialOpener, logger *slog.Logger) ([]feishu.LongConnectionBot, error) {
	if logger == nil {
		logger = slog.Default()
	}
	bots, err := repo.ListFeishuBots(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]feishu.LongConnectionBot, 0, len(bots))
	for _, bot := range bots {
		if bot.Status != biz.FeishuBotEnabled {
			continue
		}
		_, credential, credentialErr := repo.GetFeishuBotCredential(ctx, bot.ID)
		if credentialErr != nil {
			logger.Warn("skip feishu long connection bot: read credentials failed", "bot_id", bot.ID, "error", credentialErr)
			continue
		}
		plaintext, openErr := opener.Open(credential.KeyVersion, credential.Encrypted)
		if openErr != nil {
			logger.Warn("skip feishu long connection bot: decrypt credentials failed", "bot_id", bot.ID, "error", openErr)
			continue
		}
		var secret biz.FeishuBotSecret
		if unmarshalErr := json.Unmarshal(plaintext, &secret); unmarshalErr != nil {
			logger.Warn("skip feishu long connection bot: invalid credentials", "bot_id", bot.ID, "error", unmarshalErr)
			continue
		}
		if strings.TrimSpace(secret.AppSecret) == "" {
			logger.Warn("skip feishu long connection bot: app secret is empty", "bot_id", bot.ID)
			continue
		}
		result = append(result, feishu.LongConnectionBot{ID: bot.ID.String(), AppID: bot.AppID, AppSecret: secret.AppSecret})
	}
	return result, nil
}

// feishuBotTester 使用应用凭据换取 Tenant Access Token，验证连通性。
type feishuBotTester struct{}

// TestBot 调用飞书开放平台获取访问令牌；错误由上层截断后展示。
func (feishuBotTester) TestBot(ctx context.Context, appID, appSecret string) error {
	client, err := feishu.NewClient(feishu.ClientConfig{AppID: appID, AppSecret: appSecret})
	if err != nil {
		return err
	}
	_, err = client.TenantAccessToken(ctx)
	return err
}

// mcpProber 使用安全 HTTPS 客户端初始化 MCP 会话并读取工具列表。
type mcpProber struct {
	// allowPrivate 是否允许连接内网 MCP 端点。
	allowPrivate bool
	// timeout 单次探测超时。
	timeout time.Duration
}

// ListTools 返回 MCP 服务暴露的工具摘要；调用方负责展示脱敏后的错误。
func (p mcpProber) ListTools(ctx context.Context, endpoint, authType, secret string) ([]biz.MCPTool, error) {
	headers := mcp.AuthHeaders(authType, secret)
	client, err := mcp.NewClientWithHeaders(endpoint, headers, p.timeout, mcp.WithAllowPrivateEndpoints(p.allowPrivate))
	if err != nil {
		return nil, err
	}
	if err := client.Initialize(ctx); err != nil {
		return nil, err
	}
	tools, err := client.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]biz.MCPTool, 0, len(tools))
	for _, tool := range tools {
		result = append(result, biz.MCPTool{Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema})
	}
	return result, nil
}

// Run 启动 API、Worker 或两者；role 为空时读取配置中的 agent.worker.role，再回退 all。
func Run(role string) error {
	cfg, err := config.LoadEnvironment(os.Getenv("APP_ENV"))
	if err != nil {
		return err
	}
	if strings.TrimSpace(role) == "" {
		role = cfg.Agent.Worker.Role
	}
	role = normalizeRole(role)
	runAPI := role == RoleAll || role == RoleAPI
	runWorkers := role == RoleAll || role == RoleWorker
	// 结构化日志先于其他组件初始化：后续构造的 Worker 与 Service 共享统一级别、JSON 格式与关联 ID 注入。
	logger, err := logging.Setup(logging.Options{
		Level: cfg.Logging.Level, Format: cfg.Logging.Format,
		Service: "harness-agent", Environment: cfg.Environment,
	})
	if err != nil {
		return fmt.Errorf("configure logging: %w", err)
	}
	slog.SetDefault(logger)
	previousCredentialKeys := make([]platformcrypto.Key, 0, len(cfg.Credentials.PreviousKeys))
	for _, key := range cfg.Credentials.PreviousKeys {
		previousCredentialKeys = append(previousCredentialKeys, platformcrypto.Key{ID: key.ID, Secret: []byte(key.Secret)})
	}
	credentialKeys, err := platformcrypto.NewKeyring(platformcrypto.Key{ID: cfg.Credentials.ActiveKeyID, Secret: []byte(cfg.Credentials.ActiveKeySecret)}, previousCredentialKeys)
	if err != nil {
		return fmt.Errorf("configure credential encryption: %w", err)
	}
	appDB, err := data.OpenPostgres(cfg.Data.Postgres.DSN, cfg.Data.Postgres.MaxOpenConns)
	if err != nil {
		return err
	}
	defer closeDB(appDB)
	authDB, err := data.OpenPostgres(cfg.Data.Postgres.AuthDSN, cfg.Data.Postgres.MaxOpenConns)
	if err != nil {
		return err
	}
	defer closeDB(authDB)
	appSQL, err := appDB.DB()
	if err != nil {
		return fmt.Errorf("get app postgres pool: %w", err)
	}
	authSQL, err := authDB.DB()
	if err != nil {
		return fmt.Errorf("get auth postgres pool: %w", err)
	}
	verificationKeys := make([]auth.SigningKey, 0, len(cfg.Auth.JWTVerificationKeys))
	for _, key := range cfg.Auth.JWTVerificationKeys {
		verificationKeys = append(verificationKeys, auth.SigningKey{ID: key.ID, Secret: []byte(key.Secret)})
	}
	jwtManager, err := auth.NewJWTManager(
		auth.SigningKey{ID: cfg.Auth.JWTSigningKeyID, Secret: []byte(cfg.Auth.JWTSigningKeySecret)},
		verificationKeys,
		cfg.Auth.JWTIssuer,
		nil,
	)
	if err != nil {
		return err
	}
	redisClient := redis.NewClient(&redis.Options{Addr: cfg.Data.Redis.Address, Password: cfg.Data.Redis.Password})
	defer redisClient.Close()
	pingContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := redisClient.Ping(pingContext).Err(); err != nil {
		return fmt.Errorf("ping redis: %w", err)
	}
	// 依赖探针把故障状态暴露给负载均衡：
	// PostgreSQL 是关键依赖，故障时实例退出流量；Redis 仅缓存与会话热数据，故障时仍可降级服务。
	dependencyProbes := []server.DependencyProbe{
		{Name: "postgres", Critical: true, Check: func(ctx context.Context) error {
			if err := appSQL.PingContext(ctx); err != nil {
				return err
			}
			return authSQL.PingContext(ctx)
		}},
		{Name: "redis", Critical: false, Check: func(ctx context.Context) error {
			return redisClient.Ping(ctx).Err()
		}},
	}
	server.SetDependencyProbes(dependencyProbes...)
	// 刷新并发重放依赖 Redis 短时缓存，键只包含令牌哈希的二次摘要。
	refreshReplay, err := data.NewRefreshReplayStore(redisClient)
	if err != nil {
		return err
	}
	// 模型熔断状态跨副本共享，管理页面与 Worker 使用同一份 Redis 状态。
	routeHealth, err := data.NewRouteHealthStore(
		redisClient, cfg.Agent.ModelRouting.CircuitBreaker.FailureThreshold,
		cfg.Agent.ModelRouting.CircuitBreaker.Window.Duration, cfg.Agent.ModelRouting.CircuitBreaker.OpenFor.Duration,
	)
	if err != nil {
		return err
	}
	authOptions := []service.AuthServiceOption{
		service.WithRefreshReplay(refreshReplay, service.DefaultRefreshReplayTTL),
		service.WithPasswordReset(data.NewPasswordResetRepository(authDB)),
		service.WithRegistrationEnabled(cfg.Auth.RegistrationEnabled),
	}
	// 网页飞书登录与事件接入共用应用凭据；未配置回调地址时不开放登录入口。
	if cfg.Feishu.Enabled && strings.TrimSpace(cfg.Feishu.LoginRedirectURL) != "" {
		oauthClient, oauthErr := feishu.NewOAuthClient(feishu.OAuthConfig{
			AppID: cfg.Feishu.AppID, AppSecret: cfg.Feishu.AppSecret,
		})
		if oauthErr != nil {
			return fmt.Errorf("configure feishu oauth client: %w", oauthErr)
		}
		authOptions = append(authOptions, service.WithFeishuLogin(oauthClient, data.NewFeishuIdentityRepository(authDB), cfg.Feishu.LoginRedirectURL))
	}
	userRepository := data.NewUserRepository(appDB, authDB)
	authService, err := service.NewAuthService(
		userRepository,
		data.NewRefreshTokenRepository(authDB),
		jwtManager,
		[]byte(cfg.Auth.RefreshTokenPepper),
		nil,
		authOptions...,
	)
	if err != nil {
		return err
	}
	limiter, err := ratelimit.NewRedisLimiter(redisClient, "auth", cfg.Auth.LoginRateLimit.MaxAttempts, cfg.Auth.LoginRateLimit.Window.Duration)
	if err != nil {
		return err
	}
	catalogRepository := data.NewCatalogRepository(appDB)
	// 技能包文件同时服务平台与用户技能；用户数据在仓库内部强制按 user_id 隔离。
	skillBundleRepository := data.NewSkillBundleRepository(appDB)
	catalogContext, cancelCatalogCheck := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelCatalogCheck()
	if err := catalogRepository.ValidateCatalog(catalogContext); err != nil {
		return fmt.Errorf("validate model catalog: %w", err)
	}
	providerService, err := biz.NewProviderService(catalogRepository, credentialKeys)
	if err != nil {
		return err
	}
	providerService.WithRemoteModelCatalog(catalogRepository, credentialKeys, remoteModelLister{}).WithRevealAudit(catalogRepository)
	modelService, err := biz.NewModelService(catalogRepository)
	if err != nil {
		return err
	}
	policyService, err := biz.NewPolicyService(catalogRepository)
	if err != nil {
		return err
	}
	skillService, err := biz.NewSkillService(catalogRepository)
	if err != nil {
		return err
	}
	skillService.WithBundles(skillBundleRepository)
	mcpService, err := biz.NewMCPService(catalogRepository, credentialKeys)
	if err != nil {
		return err
	}
	mcpService.WithProber(catalogRepository, credentialKeys, mcpProber{
		allowPrivate: cfg.Agent.Worker.AllowPrivateMCPEndpoints,
		timeout:      cfg.Agent.Worker.MCPTimeout.Duration,
	}).WithRevealAudit(catalogRepository)
	quotaLimits := biz.QuotaLimits{MaxAgentsPerUser: cfg.Quotas.MaxAgentsPerUser, MaxSessionsPerUser: cfg.Quotas.MaxSessionsPerUser}
	agentRepository := data.NewAgentRepository(appDB)
	agentService, err := biz.NewAgentService(agentRepository, quotaLimits)
	if err != nil {
		return err
	}
	// 飞书机器人注册与助手绑定：机器人与绑定关系均按归属用户隔离。
	feishuBotRepository := data.NewFeishuBotRepository(appDB)
	feishuBotService, err := biz.NewFeishuBotService(feishuBotRepository, credentialKeys, credentialKeys)
	if err != nil {
		return err
	}
	feishuBotService.WithTester(feishuBotTester{}).WithRevealAudit(feishuBotRepository)
	if cfg.Feishu.NormalizedEventMode() == config.FeishuEventModeLongConnection {
		feishuBotService.WithLongConnection()
	}
	feishuBotBindingService, err := biz.NewFeishuBotBindingService(feishuBotRepository, feishuBotRepository, agentRepository)
	if err != nil {
		return err
	}
	sessionRepository := data.NewSessionRepository(appDB)
	hotMessages, err := data.NewHotMessageStore(redisClient, cfg.Agent.Memory.HotWindowMessages, cfg.Agent.Memory.HotWindowTTL.Duration)
	if err != nil {
		return err
	}
	summaryRepository := data.NewSummaryRepository(appDB)
	jobRepository := data.NewJobRepository(appDB)
	memoryRepository := data.NewMemoryRepository(appDB)
	factRepository := data.NewFactRepository(appDB)
	userSkillRepository := data.NewUserSkillRepository(appDB)
	var summaryPlanner *biz.SummaryPlanner
	var memoryIndexer *biz.MemoryIndexer
	var memoryRetriever *biz.MemoryRetriever
	var memoryReranker *biz.LLMReranker
	var embeddingGenerator *biz.LLMEmbeddingGenerator
	var factService *biz.FactService
	var factIndexer *biz.FactIndexer
	var factExtractor *biz.FactLLMExtractor
	if cfg.Agent.Memory.Summary.Enabled {
		summaryPlanner, err = biz.NewSummaryPlanner(sessionRepository, summaryRepository, jobRepository, biz.SummaryPlanConfig{
			TriggerMessages: cfg.Agent.Memory.Summary.TriggerMessages, KeepRecent: cfg.Agent.Memory.Summary.KeepRecent,
			MaxSourceMessages: cfg.Agent.Memory.Summary.MaxSourceMessages,
		}, slog.Default(), nil)
		if err != nil {
			return err
		}
	}
	if cfg.Agent.Memory.Vector.Enabled {
		embeddingGenerator, err = biz.NewLLMEmbeddingGenerator(catalogRepository, credentialKeys, cfg.Agent.Memory.Vector.EmbeddingTimeout.Duration)
		if err != nil {
			return err
		}
		memoryIndexer, err = biz.NewMemoryIndexer(sessionRepository, memoryRepository, jobRepository, biz.MemoryIndexerConfig{
			RecentMessages: cfg.Agent.Memory.Vector.RecentMessages,
		}, slog.Default(), nil, memoryRepository)
		if err != nil {
			return err
		}
		var reranker biz.Reranker
		if cfg.Agent.Memory.Vector.RerankEnabled {
			memoryReranker, err = biz.NewLLMReranker(catalogRepository, memoryRepository, credentialKeys, cfg.Agent.Memory.Vector.EmbeddingTimeout.Duration)
			if err != nil {
				return err
			}
			reranker = memoryReranker
		}
		memoryRetriever, err = biz.NewMemoryRetriever(memoryRepository, memoryRepository, embeddingGenerator, reranker, biz.MemoryRetrieverConfig{
			TopK: cfg.Agent.Memory.Vector.RetrieverTopK, MMRTop: cfg.Agent.Memory.Vector.MMRTop,
			Lambda: cfg.Agent.Memory.Vector.MMRLambda, MinSimilarity: cfg.Agent.Memory.Vector.MinSimilarity,
			RerankEnabled: cfg.Agent.Memory.Vector.RerankEnabled,
		}, slog.Default())
		if err != nil {
			return err
		}
	}
	sessionOptions := []biz.SessionServiceOption{biz.WithHotMessages(hotMessages)}
	if summaryPlanner != nil {
		sessionOptions = append(sessionOptions, biz.WithSummaryTrigger(summaryPlanner))
	}
	if memoryIndexer != nil {
		sessionOptions = append(sessionOptions, biz.WithMemoryTrigger(memoryIndexer))
	}
	if cfg.Agent.Memory.Facts.Enabled {
		factService, err = biz.NewFactService(factRepository, biz.FactPolicy{
			MinConfidence: cfg.Agent.Memory.Facts.MinConfidence, CorrectionGuard: cfg.Agent.Memory.Facts.CorrectionGuard,
			MaxFactsPerUser: cfg.Agent.Memory.Facts.MaxFactsPerUser,
		}, nil)
		if err != nil {
			return err
		}
		factIndexer, err = biz.NewFactIndexer(sessionRepository, jobRepository, biz.FactIndexerConfig{
			RecentMessages: cfg.Agent.Memory.Facts.RecentMessages, MinMessages: cfg.Agent.Memory.Facts.MinMessages,
		}, slog.Default(), nil)
		if err != nil {
			return err
		}
		sessionOptions = append(sessionOptions, biz.WithFactTrigger(factIndexer))
	}
	// 飞书启用时阻止群聊话题被改为私有记忆，避免个人画像与长期记忆注入共享上下文。
	if cfg.Feishu.Enabled {
		sessionOptions = append(sessionOptions, biz.WithSharedSessionDetector(data.NewFeishuConversationRepository(appDB)))
	}
	sessionService, err := biz.NewSessionService(sessionRepository, quotaLimits, sessionOptions...)
	if err != nil {
		return err
	}
	attachmentRepository := data.NewAttachmentRepository(appDB)
	attachmentService, err := biz.NewAttachmentService(sessionRepository, attachmentRepository)
	if err != nil {
		return err
	}
	idempotencyService, err := biz.NewIdempotencyService(data.NewIdempotencyRepository(appDB))
	if err != nil {
		return err
	}
	// Run 事件通知总线：事件写入后即时唤醒 SSE；Redis 异常时由 SSE 兜底轮询覆盖。
	runEventBus := data.NewRunEventBus(redisClient)
	defer runEventBus.Close()
	server.SetRunEventStream(runEventBus)
	runRepository := data.NewRunRepository(appDB, data.WithRunEventNotifier(runEventBus))
	// MCP 客户端与工具清单缓存：跨 Run 复用连接与发现结果，避免每次执行重复握手。
	mcpClientCache := mcp.NewClientCache(0)
	stepRepository := data.NewExecutionStepRepository(appDB)
	approvalRepository := data.NewToolApprovalRepository(appDB)
	budgetRepository := data.NewBudgetRepository(appDB)
	budgetService, err := biz.NewBudgetService(budgetRepository, biz.BudgetPolicy{
		UserDailyTokens: cfg.Agent.Budgets.UserDailyTokens, UserDailyCost: cfg.Agent.Budgets.UserDailyCost,
		AgentDailyTokens: cfg.Agent.Budgets.AgentDailyTokens, AgentDailyCost: cfg.Agent.Budgets.AgentDailyCost,
		ProviderDailyTokens: cfg.Agent.Budgets.ProviderDailyTokens, ProviderDailyCost: cfg.Agent.Budgets.ProviderDailyCost,
		AlertThreshold: cfg.Agent.Budgets.AlertThreshold,
	}, nil)
	if err != nil {
		return err
	}
	if memoryRetriever != nil {
		memoryRetriever.WithUsageRecorder(budgetRepository).WithBudgetChecker(budgetService)
	}
	if memoryReranker != nil {
		memoryReranker.WithBudgetChecker(budgetService)
	}
	runService, err := biz.NewRunService(runRepository, sessionRepository,
		biz.WithRunSnapshots(data.NewRunSnapshotRepository(agentRepository, catalogRepository)),
		biz.WithRunEnqueuer(jobRepository),
		biz.WithRunCreationTransaction(data.NewRunCreationTransaction(appDB)),
		biz.WithExecutionSteps(stepRepository),
		biz.WithToolApprovals(approvalRepository),
		biz.WithBudgets(budgetService),
		biz.WithModelCalls(data.NewModelCallRepository(appDB)),
		biz.WithSupervisorTasks(data.NewSupervisorTaskRepository(appDB)),
		biz.WithPlanResume(data.NewPlanRepository(appDB)),
		biz.WithRunDeadline(cfg.Agent.Worker.RunDeadline.Duration),
	)
	if err != nil {
		return err
	}
	platformService, err := service.NewPlatformService(providerService, modelService, policyService, skillService, mcpService, agentService, sessionService, runService, idempotencyService)
	if err != nil {
		return err
	}
	userSkillService, err := biz.NewUserSkillService(userSkillRepository)
	if err != nil {
		return err
	}
	userSkillService.WithSynthesisSettings(userSkillRepository, catalogRepository)
	userSkillService.WithSynthesisRuns(userSkillRepository)
	userSkillService.WithSynthesisTrigger(jobRepository)
	userSkillService.WithBundles(skillBundleRepository)
	platformService.UserSkills = userSkillService
	platformService.WithFeishuBots(feishuBotService, feishuBotBindingService)
	// 路由健康聚合仅管理员可读，供 Web 展示熔断状态。
	routingHealth, err := biz.NewRoutingHealthService(catalogRepository, routeHealth)
	if err != nil {
		return err
	}
	platformService.WithRoutingHealth(routingHealth)
	// 向量模型状态只读查询：供前端提示是否已配置并生效的 embedding 模型。
	vectorStatusService, err := biz.NewVectorStatusService(memoryRepository, memoryRepository, catalogRepository, cfg.Agent.Memory.Vector.Enabled)
	if err != nil {
		return err
	}
	platformService.WithVectorStatus(vectorStatusService)
	platformService.WithAttachments(attachmentService)
	// Plan 服务复用 Run 服务完成取消与恢复入队。
	planService, err := biz.NewPlanService(data.NewPlanRepository(appDB), runService)
	if err != nil {
		return err
	}
	platformService.WithPlans(planService)
	deletionRepository := data.NewDeletionRepository(appDB)
	deletionService, err := biz.NewDeletionService(deletionRepository, sessionRepository, slog.Default(), nil)
	if err != nil {
		return err
	}
	exportService, err := biz.NewExportService(sessionRepository, summaryRepository, factRepository, memoryRepository, nil, biz.WithExportAgents(agentRepository))
	if err != nil {
		return err
	}
	platformService.WithDeletion(deletionService, exportService)
	// 个人记忆知识图谱复用记忆仓储，按 user_id 聚合只读视图。
	memoryGraphService, err := biz.NewMemoryGraphService(sessionRepository, summaryRepository, factRepository, memoryRepository, nil)
	if err != nil {
		return err
	}
	platformService.WithMemoryGraph(memoryGraphService)
	// Worker 标识在执行 Worker 与飞书 Worker 间共享；为空时启动时生成唯一标识。
	workerID := cfg.Agent.Worker.WorkerID
	if workerID == "" {
		hostname, _ := os.Hostname()
		workerID = fmt.Sprintf("%s-%d-%s", hostname, os.Getpid(), uuid.NewString()[:8])
	}
	// 飞书集成：回调或长连接只做验签幂等入库，身份映射、话题绑定与回复投递全部异步执行。
	var feishuEventWorker *biz.FeishuEventWorker
	var feishuReplyWorker *biz.FeishuReplyWorker
	var feishuLongConnection *feishu.LongConnection
	if cfg.Feishu.Enabled {
		eventMode := cfg.Feishu.NormalizedEventMode()
		encryptKeys := make([]feishu.EncryptionKey, 0, len(cfg.Feishu.EncryptKeys))
		for _, key := range cfg.Feishu.EncryptKeys {
			// 配置模板保留的空占位不参与验签；长连接模式通常没有加密密钥。
			if strings.TrimSpace(key.ID) == "" || strings.TrimSpace(key.Secret) == "" {
				continue
			}
			encryptKeys = append(encryptKeys, feishu.EncryptionKey{ID: key.ID, Secret: key.Secret})
		}
		verifier, err := feishu.NewVerifier(feishu.VerifierConfig{
			VerificationToken: cfg.Feishu.VerificationToken,
			EncryptKeys:       encryptKeys,
			Window:            cfg.Feishu.EventWindow.Duration,
		})
		if err != nil {
			return fmt.Errorf("configure feishu verifier: %w", err)
		}
		eventRepository := data.NewFeishuEventRepository(appDB, credentialKeys)
		ingestService, err := biz.NewFeishuIngestService(verifier, eventRepository, jobRepository, biz.FeishuIngestConfig{
			MaxBodyBytes: cfg.Feishu.MaxBodyBytes, DefaultAppID: cfg.Feishu.AppID,
		})
		if err != nil {
			return err
		}
		ingestService.WithBotVerification(feishuBotRepository, credentialKeys)
		// 只有 webhook 模式才注册公网回调路由；长连接由服务主动连飞书。
		if eventMode == config.FeishuEventModeWebhook {
			platformService.WithFeishuIngest(ingestService)
		}
		// 飞书话题解绑：仅会话所有者可解绑，管理员不跨用户操作他人绑定。
		feishuBindingService, err := biz.NewFeishuBindingService(data.NewFeishuConversationRepository(appDB), sessionRepository)
		if err != nil {
			return err
		}
		platformService.WithFeishuBindings(feishuBindingService)
		// 默认应用仅在使用全局应用凭据时创建；长连接模式可只依赖已登记机器人。
		var feishuClient *feishu.Client
		defaultAppID := strings.TrimSpace(cfg.Feishu.AppID)
		if defaultAppID != "" && strings.TrimSpace(cfg.Feishu.AppSecret) != "" {
			feishuClient, err = feishu.NewClient(feishu.ClientConfig{AppID: defaultAppID, AppSecret: cfg.Feishu.AppSecret})
			if err != nil {
				return fmt.Errorf("configure feishu client: %w", err)
			}
		}
		var defaultFeishuSender biz.FeishuReplySender
		if feishuClient != nil {
			defaultFeishuSender = feishuClient
		}
		// 默认应用机器人仅在配置了默认助手时启用；只使用注册机器人时可留空。
		var defaultAgentID *uuid.UUID
		var defaultVersion *int
		var defaultAgentOwnerID *uuid.UUID
		if raw := strings.TrimSpace(cfg.Feishu.DefaultAgentID); raw != "" {
			parsed, parseErr := uuid.Parse(raw)
			if parseErr != nil {
				return fmt.Errorf("parse feishu default agent id: %w", parseErr)
			}
			version := cfg.Feishu.DefaultAgentVersion
			// 启动期校验默认助手：版本必须匹配且已发布、编排模式必须与 feishu.mode 一致。
			// 否则事件到达后才会在 Run 快照校验处失败，默认应用也无法确定应以谁的身份执行。
			agent, loadErr := agentRepository.GetAgentAdmin(context.Background(), parsed)
			if loadErr != nil {
				return fmt.Errorf("load feishu default agent: %w", loadErr)
			}
			if agent.Version != version {
				return fmt.Errorf("feishu default agent version mismatch: configured %d, found %d", version, agent.Version)
			}
			if agent.Status != biz.AgentPublished {
				return fmt.Errorf("feishu default agent %s is not published", parsed)
			}
			if string(agent.Mode) != cfg.Feishu.Mode {
				return fmt.Errorf("feishu default agent mode %q does not match feishu.mode %q", agent.Mode, cfg.Feishu.Mode)
			}
			// 默认应用沿用“机器人只服务属主”的策略，执行身份固定为默认助手属主。
			owner := agent.UserID
			defaultAgentOwnerID = &owner
			defaultAgentID, defaultVersion = &parsed, &version
		}
		sharedChats := make([]biz.FeishuSharedChat, 0, len(cfg.Feishu.Group.SharedChats))
		for _, chat := range cfg.Feishu.Group.SharedChats {
			policyID, parseErr := uuid.Parse(strings.TrimSpace(chat.PolicyID))
			if parseErr != nil {
				return fmt.Errorf("parse feishu shared chat model policy id: %w", parseErr)
			}
			policy, loadErr := catalogRepository.GetPolicy(context.Background(), policyID)
			if loadErr != nil {
				return fmt.Errorf("load feishu shared chat model policy: %w", loadErr)
			}
			if policy.Status != biz.PolicyPublished {
				return fmt.Errorf("feishu shared chat model policy %s is not published", policyID)
			}
			sharedChats = append(sharedChats, biz.FeishuSharedChat{
				AppID: chat.AppID, TenantKey: chat.TenantKey, ChatID: chat.ChatID, PolicyID: policyID,
			})
		}
		feishuMessages := data.NewFeishuMessageRepository(appDB)
		feishuSenderResolver := &feishuBotSenderResolver{credentials: feishuBotRepository, opener: credentialKeys}
		feishuWorkerDependencies := biz.FeishuEventWorkerDependencies{
			Jobs:          jobRepository,
			Events:        eventRepository,
			Identities:    data.NewFeishuIdentityRepository(authDB),
			Conversations: data.NewFeishuConversationRepository(appDB),
			Sessions:      sessionRepository,
			Messages:      feishuMessages,
			GroupMessages: feishuMessages,
			Runs:          runService,
			Outbound:      data.NewFeishuOutboundRepository(appDB),
			Notices: &feishuNoticeSender{
				defaultAppID: defaultAppID, defaultSender: defaultFeishuSender, senders: feishuSenderResolver,
			},
			HotMessages: hotMessages,
			Bots:        feishuBotRepository,
			Logger:      slog.Default(),
		}
		// 群聊个人事实只写发言人私有记忆；事实功能关闭时保持为空，避免 typed-nil 接口调用。
		if factIndexer != nil {
			feishuWorkerDependencies.SpeakerFacts = factIndexer
		}
		feishuEventWorker, err = biz.NewFeishuEventWorker(feishuWorkerDependencies, biz.FeishuEventWorkerConfig{
			WorkerID: workerID, Concurrency: cfg.Feishu.Worker.Concurrency,
			PollInterval: cfg.Feishu.Worker.PollInterval.Duration, Lease: cfg.Feishu.Worker.Lease.Duration,
			DefaultAgentID: defaultAgentID, DefaultAgentVersion: defaultVersion,
			DefaultAgentOwnerID: defaultAgentOwnerID,
			DefaultAppID:        defaultAppID, SharedChats: sharedChats,
			Mode: biz.AgentMode(cfg.Feishu.Mode), BotOpenID: cfg.Feishu.BotOpenID, BotName: cfg.Feishu.BotName,
			RequireTopic: cfg.Feishu.Group.RequireTopic, AllowRootReplyFallback: cfg.Feishu.Group.AllowRootReplyFallback,
		})
		if err != nil {
			return err
		}
		feishuReplyWorker, err = biz.NewFeishuReplyWorker(biz.FeishuReplyWorkerDependencies{
			Outbound: data.NewFeishuOutboundRepository(appDB),
			Runs:     runRepository,
			Sender:   defaultFeishuSender,
			Senders:  feishuSenderResolver,
			Logger:   slog.Default(),
		}, biz.FeishuReplyWorkerConfig{
			DefaultAppID: defaultAppID,
			WorkerID:     workerID, Concurrency: cfg.Feishu.Reply.Concurrency,
			PollInterval: cfg.Feishu.Reply.PollInterval.Duration, Lease: cfg.Feishu.Reply.Lease.Duration,
			PollRunInterval: cfg.Feishu.Reply.PollRunInterval.Duration, RunWaitTimeout: cfg.Feishu.Reply.RunWaitTimeout.Duration,
			MaxTextBytes:         cfg.Feishu.Reply.MaxTextBytes,
			CardUpdateInterval:   cfg.Feishu.Reply.CardUpdateInterval.Duration,
			MaxConcurrentPerChat: cfg.Feishu.Reply.MaxConcurrentPerChat,
			MaxConcurrentPerUser: cfg.Feishu.Reply.MaxConcurrentPerUser,
		})
		if err != nil {
			return err
		}
		if eventMode == config.FeishuEventModeLongConnection {
			bots, loadErr := loadFeishuLongConnectionBots(context.Background(), feishuBotRepository, credentialKeys, slog.Default())
			if loadErr != nil {
				return fmt.Errorf("load feishu long connection bots: %w", loadErr)
			}
			feishuLongConnection, err = feishu.NewLongConnection(feishu.LongConnectionConfig{
				Bots: bots,
				LoadBots: func(ctx context.Context) ([]feishu.LongConnectionBot, error) {
					return loadFeishuLongConnectionBots(ctx, feishuBotRepository, credentialKeys, slog.Default())
				},
				RefreshInterval: 10 * time.Second,
				Handler: func(ctx context.Context, botID, appID string, body []byte) error {
					botUUID, parseErr := uuid.Parse(botID)
					if parseErr != nil {
						return fmt.Errorf("parse feishu bot id: %w", parseErr)
					}
					_, ingestErr := ingestService.AcceptTrusted(ctx, botUUID, appID, body)
					return ingestErr
				},
				Logger: slog.Default(),
			})
			if err != nil {
				return fmt.Errorf("configure feishu long connection: %w", err)
			}
			slog.Info("feishu long connection configured", "bots", len(bots))
		}
	}
	// 长连接故障只标记降级，不摘除仍可服务 Web 请求的实例；探针不暴露凭据或上游错误详情。
	if feishuLongConnection != nil {
		dependencyProbes = append(dependencyProbes, server.DependencyProbe{
			Name: "feishu_long_connection", Critical: false,
			Check: func(context.Context) error { return feishuLongConnection.Health() },
		})
		server.SetDependencyProbes(dependencyProbes...)
	}
	// API 多维限流：全局、来源 IP 与登录用户三个维度各自独立计数；
	// 任一维度配置为 0 视为关闭该维度，便于按环境逐步启用。
	apiLimits := server.APILimiters{}
	if cfg.Server.APIRateLimit.GlobalPerMinute > 0 {
		apiLimits.Global, err = ratelimit.NewRedisLimiter(redisClient, "api-global", cfg.Server.APIRateLimit.GlobalPerMinute, time.Minute)
		if err != nil {
			return err
		}
	}
	if cfg.Server.APIRateLimit.PerIPPerMinute > 0 {
		apiLimits.IP, err = ratelimit.NewRedisLimiter(redisClient, "api-ip", cfg.Server.APIRateLimit.PerIPPerMinute, time.Minute)
		if err != nil {
			return err
		}
	}
	if cfg.Server.APIRateLimit.PerUserPerMinute > 0 {
		apiLimits.User, err = ratelimit.NewRedisLimiter(redisClient, "api-user", cfg.Server.APIRateLimit.PerUserPerMinute, time.Minute)
		if err != nil {
			return err
		}
	}
	// Worker 生命周期上下文先于 HTTP Server 创建，供停机回调和后台任务共享。
	workerContext, stopWorker := context.WithCancel(context.Background())
	defer stopWorker()
	// Worker-only 进程不监听 HTTP/gRPC；API-only 进程不启动消费 Worker。
	var app *kratos.App
	if runAPI {
		httpServer, err := server.NewHTTPServerWithLimits(cfg.Server, static.Dist(), authService, limiter, apiLimits, platformService)
		if err != nil {
			return err
		}
		grpcServer, err := server.NewGRPCServer(cfg.Server.GRPC.Address, authService, platformService)
		if err != nil {
			return err
		}
		app = kratos.New(
			kratos.Name("harness-agent"),
			kratos.Server(httpServer, grpcServer),
			// 停机顺序：先 readiness 摘流，再等待在途任务收敛，最后取消领取并等待 Worker 退出。
			kratos.BeforeStop(func(ctx context.Context) error {
				server.SetReady(false)
				deadline := time.NewTimer(10 * time.Second)
				defer deadline.Stop()
				ticker := time.NewTicker(100 * time.Millisecond)
				defer ticker.Stop()
				for biz.InflightJobs() > 0 {
					select {
					case <-ctx.Done():
						stopWorker()
						return nil
					case <-deadline.C:
						stopWorker()
						return nil
					case <-ticker.C:
					}
				}
				stopWorker()
				return nil
			}),
			kratos.StopTimeout(cfg.Server.ShutdownTimeout.Duration),
		)
	}
	recorder, err := audit.NewRecorder(authDB, []byte(cfg.Audit.PseudonymKey), nil)
	if err != nil {
		return err
	}
	dispatcher, err := audit.NewDispatcher(authDB, recorder, nil)
	if err != nil {
		return err
	}
	var workers sync.WaitGroup
	// 队列深度指标在 API 与 Worker 角色都采集，供 /metrics 与 HPA 使用。
	workers.Add(1)
	go func() {
		defer workers.Done()
		collectJobQueueMetrics(workerContext, jobRepository, jobQueueMetricsInterval)
	}()
	workers.Add(1)
	go func() {
		defer workers.Done()
		dispatcher.Run(workerContext, time.Second)
	}()
	// Sandbox Runner 仅在高风险工具强制执行或显式配置端点时创建；未配置时高层策略会失败关闭。
	var sandboxRunner sandbox.Runner
	if strings.TrimSpace(cfg.Agent.Sandbox.Endpoint) != "" {
		client, err := sandbox.NewClient(sandbox.Config{
			Endpoint: cfg.Agent.Sandbox.Endpoint, Token: cfg.Agent.Sandbox.Token,
			AllowInsecure: cfg.Agent.Sandbox.AllowInsecure, Timeout: cfg.Agent.Sandbox.Timeout.Duration,
			MaxResponseBytes: cfg.Agent.Sandbox.MaxResponseBytes,
		})
		if err != nil {
			return fmt.Errorf("configure sandbox: %w", err)
		}
		sandboxRunner = client
	}
	if runWorkers && workerComponentEnabled(cfg.Agent.Worker, "execution") && cfg.Agent.Worker.Enabled {
		providerLimiter, err := data.NewRedisConcurrencyLimiter(redisClient, cfg.Agent.Worker.PerProviderConcurrency, cfg.Agent.Worker.ConcurrencyLease.Duration, 100*time.Millisecond)
		if err != nil {
			return err
		}
		baseConfig := biz.WorkerConfig{
			WorkerID:     workerID,
			PollInterval: cfg.Agent.Worker.PollInterval.Duration, Lease: cfg.Agent.Worker.Lease.Duration,
			Heartbeat: cfg.Agent.Worker.Heartbeat.Duration, CancelPoll: cfg.Agent.Worker.CancelPoll.Duration,
			MaxPayloadBytes:           cfg.Agent.ExecutionRecords.MaxInlinePayloadBytes,
			RetainPartialResponse:     cfg.Agent.ExecutionRecords.RetainPartialResponse,
			RequireSandboxForHighRisk: cfg.Agent.Sandbox.RequireForHighRisk,
			SkillExecution: biz.SkillExecutionConfig{
				AllowLocalExecution: cfg.Agent.Skills.EnableLocalExecution,
				Timeout:             cfg.Agent.Skills.Timeout.Duration,
				MaxOutputBytes:      cfg.Agent.Skills.MaxOutputBytes,
			},
			ReactMaxSteps:          cfg.Agent.Worker.ReactMaxSteps,
			RunDeadline:            cfg.Agent.Worker.RunDeadline.Duration,
			ReactMaxTotalTokens:    cfg.Agent.Worker.ReactMaxTotalTokens,
			ReactMaxCost:           cfg.Agent.Worker.ReactMaxCost,
			SupervisorMaxTokens:    cfg.Agent.Worker.SupervisorMaxTokens,
			SupervisorMaxCost:      cfg.Agent.Worker.SupervisorMaxCost,
			MCPTimeout:             cfg.Agent.Worker.MCPTimeout.Duration,
			MCPAllowPrivateNetwork: cfg.Agent.Worker.AllowPrivateMCPEndpoints,
			ApprovalPoll:           cfg.Agent.Worker.ApprovalPoll.Duration,
			PromptTokenBudget:      int(cfg.Agent.ModelRouting.MaxPromptTokens),
			ReserveOutputTokens:    int(cfg.Agent.ModelRouting.ReserveOutputTokens),
			SafetyMarginTokens:     int(cfg.Agent.ModelRouting.SafetyMarginTokens),
			HistoryLimit:           cfg.Agent.Memory.HotWindowMessages,
			Routing: model.RouterConfig{
				MinContextTokens:    cfg.Agent.ModelRouting.MinContextTokens,
				MaxAttemptsPerModel: cfg.Agent.ModelRouting.MaxAttemptsPerModel,
				MaxAttemptsPerRun:   cfg.Agent.ModelRouting.MaxAttemptsPerRun,
				MaxSwitchesPerRun:   cfg.Agent.ModelRouting.MaxSwitchesPerRun,
				MaxOutputTokens:     cfg.Agent.Worker.MaxOutputTokens,
				RequestTimeout:      cfg.Agent.Worker.RequestTimeout.Duration,
				BackoffBase:         cfg.Agent.Worker.BackoffBase.Duration,
				MaxBackoff:          cfg.Agent.Worker.MaxBackoff.Duration,
				MaxRetryAfter:       cfg.Agent.Worker.MaxRetryAfter.Duration,
				Temperature:         cfg.Agent.Worker.Temperature,
				Limiter:             providerLimiter,
			},
		}
		baseDependencies := biz.ExecutionWorkerDependencies{
			Jobs: jobRepository, Runs: runRepository, Sessions: sessionRepository,
			SessionPolicies: sessionRepository,
			Providers:       catalogRepository, Opener: credentialKeys,
			Attempts: data.NewModelCallRepository(appDB), Steps: stepRepository,
			Approvals: approvalRepository, Plans: data.NewPlanRepository(appDB), SupervisorTasks: data.NewSupervisorTaskRepository(appDB), Budgets: budgetService, HotMessages: hotMessages, MCPs: catalogRepository,
			MCPCache:  mcpClientCache,
			Summaries: summaryRepository, SummaryTrigger: summaryPlanner,
			Memory: memoryRetriever, MemoryTrigger: memoryIndexer,
			Facts: factService, FactTrigger: factIndexer,
			Attachments:  attachmentRepository,
			SkillBundles: skillBundleRepository,
			Health:       routeHealth, Sandbox: sandboxRunner,
		}
		for _, pool := range effectiveExecutionPools(cfg.Agent.Worker) {
			poolConcurrency := pool.Concurrency
			if poolConcurrency <= 0 {
				poolConcurrency = cfg.Agent.Worker.Concurrency
			}
			poolGlobalConcurrency := pool.GlobalConcurrency
			if poolGlobalConcurrency <= 0 {
				poolGlobalConcurrency = cfg.Agent.Worker.GlobalConcurrency
			}
			poolLease := pool.Lease.Duration
			if poolLease <= 0 {
				poolLease = cfg.Agent.Worker.Lease.Duration
			}
			poolHeartbeat := pool.Heartbeat.Duration
			if poolHeartbeat <= 0 {
				poolHeartbeat = cfg.Agent.Worker.Heartbeat.Duration
			}
			globalLimiter, err := data.NewRedisConcurrencyLimiter(redisClient, poolGlobalConcurrency, cfg.Agent.Worker.ConcurrencyLease.Duration, 100*time.Millisecond)
			if err != nil {
				return err
			}
			userLimiter, err := data.NewRedisConcurrencyLimiter(redisClient, cfg.Agent.Worker.PerUserConcurrency, cfg.Agent.Worker.ConcurrencyLease.Duration, 100*time.Millisecond)
			if err != nil {
				return err
			}
			dependencies := baseDependencies
			dependencies.GlobalLimiter = globalLimiter
			dependencies.UserLimiter = userLimiter
			dependencies.Logger = slog.Default().With("worker_pool", pool.Name, "worker_id", workerID+"-"+pool.Name)
			workerConfig := baseConfig
			workerConfig.WorkerID = workerID + "-" + pool.Name
			workerConfig.PoolName = pool.Name
			workerConfig.Lanes = append([]string(nil), pool.Lanes...)
			workerConfig.Concurrency = poolConcurrency
			workerConfig.Lease = poolLease
			workerConfig.Heartbeat = poolHeartbeat
			executionWorker, err := biz.NewExecutionWorker(dependencies, workerConfig)
			if err != nil {
				return err
			}
			workers.Add(1)
			go func(name string, worker *biz.ExecutionWorker, poolWorkerID string, lanes []string, concurrency int, lease time.Duration) {
				defer workers.Done()
				slog.Info("execution worker pool started", "pool", name, "worker_id", poolWorkerID,
					"lanes", lanes, "concurrency", concurrency, "lease", lease)
				worker.Start(workerContext)
			}(pool.Name, executionWorker, workerConfig.WorkerID, append([]string(nil), workerConfig.Lanes...),
				workerConfig.Concurrency, workerConfig.Lease)
		}
	}
	if runWorkers && workerComponentEnabled(cfg.Agent.Worker, "background") && cfg.Agent.Memory.Summary.Enabled {
		// 摘要 Worker 复用 Run 的模型候选与凭证解密能力，并受供应商并发舱壁约束。
		summaryLimiter, err := data.NewRedisConcurrencyLimiter(redisClient, cfg.Agent.Worker.PerProviderConcurrency, cfg.Agent.Worker.ConcurrencyLease.Duration, 100*time.Millisecond)
		if err != nil {
			return err
		}
		generator, err := biz.NewSummaryLLMGenerator(runRepository, catalogRepository, credentialKeys, budgetService, summaryLimiter, biz.SummaryLLMConfig{
			MaxInputBytes: cfg.Agent.Memory.Summary.MaxInputBytes, MaxMessageBytes: cfg.Agent.Memory.Summary.MaxMessageBytes,
			MaxOutputTokens: cfg.Agent.Memory.Summary.MaxOutputTokens, Temperature: cfg.Agent.Memory.Summary.Temperature,
			RequestTimeout: cfg.Agent.Memory.Summary.RequestTimeout.Duration,
		})
		if err != nil {
			return err
		}
		generator.WithUsageRecorder(budgetRepository)
		summaryWorker, err := biz.NewSummaryWorker(biz.SummaryWorkerDependencies{
			Jobs: jobRepository, Sessions: sessionRepository, Messages: sessionRepository,
			Summaries: summaryRepository, Generator: generator, Queue: jobRepository,
			Runs: runRepository, Steps: stepRepository, Logger: slog.Default(),
		}, biz.SummaryWorkerConfig{
			WorkerID: workerID, Concurrency: cfg.Agent.Memory.Summary.Concurrency,
			PollInterval: cfg.Agent.Memory.Summary.PollInterval.Duration, Lease: cfg.Agent.Memory.Summary.Lease.Duration,
			Heartbeat: cfg.Agent.Memory.Summary.Heartbeat.Duration, MaxSummaryBytes: cfg.Agent.Memory.Summary.MaxSummaryBytes,
		})
		if err != nil {
			return err
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			summaryWorker.Start(workerContext)
		}()
		// 补建扫描补偿消息写入与 Run 完成路径中丢失的摘要入队，保证摘要最终推进。
		summaryMaintenance, err := biz.NewSummaryMaintenance(sessionRepository, summaryPlanner, biz.SummaryMaintenanceConfig{
			Interval: cfg.Agent.Memory.Summary.MaintenanceInterval.Duration,
		}, slog.Default())
		if err != nil {
			return err
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			summaryMaintenance.Start(workerContext)
		}()
	}
	if runWorkers && workerComponentEnabled(cfg.Agent.Worker, "background") && cfg.Agent.Memory.Vector.Enabled {
		// 索引服务先于 Worker 构造，供 Worker 上报 embedding 不可用信号以暂停回填。
		indexService, err := biz.NewMemoryIndexService(memoryRepository, memoryRepository, jobRepository, biz.MemoryIndexServiceConfig{
			CoverageThreshold: cfg.Agent.Memory.Vector.CoverageThreshold, BackfillBatch: cfg.Agent.Memory.Vector.BackfillBatch,
		}, slog.Default(), nil)
		if err != nil {
			return err
		}
		memoryEmbedWorker, err := biz.NewMemoryEmbedWorker(biz.MemoryEmbedWorkerDependencies{
			Jobs: jobRepository, Messages: memoryRepository, Indexes: memoryRepository,
			Vectors: memoryRepository, Generator: embeddingGenerator, Usage: budgetRepository, Budgets: budgetService,
			Health: indexService, Logger: slog.Default(),
		}, biz.MemoryEmbedWorkerConfig{
			WorkerID: workerID, Concurrency: cfg.Agent.Memory.Vector.Concurrency,
			PollInterval: cfg.Agent.Memory.Vector.PollInterval.Duration, Lease: cfg.Agent.Memory.Vector.Lease.Duration,
			Heartbeat: cfg.Agent.Memory.Vector.Heartbeat.Duration, BatchSize: cfg.Agent.Memory.Vector.BatchSize,
		})
		if err != nil {
			return err
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			memoryEmbedWorker.Start(workerContext)
		}()
		maintenance, err := biz.NewMemoryMaintenance(indexService, biz.MemoryMaintenanceConfig{
			Enabled: cfg.Agent.Memory.Vector.MaintenanceEnabled, Interval: cfg.Agent.Memory.Vector.MaintenanceInterval.Duration,
		}, slog.Default())
		if err != nil {
			return err
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			maintenance.Start(workerContext)
		}()
	}
	if runWorkers && workerComponentEnabled(cfg.Agent.Worker, "background") && cfg.Agent.Memory.Facts.Enabled {
		factExtractor, err = biz.NewFactLLMExtractor(runRepository, catalogRepository, credentialKeys, biz.FactLLMConfig{
			MaxMessageBytes: cfg.Agent.Memory.Facts.MaxMessageBytes, MaxOutputTokens: cfg.Agent.Memory.Facts.MaxOutputTokens,
			Temperature: cfg.Agent.Memory.Facts.Temperature, RequestTimeout: cfg.Agent.Memory.Facts.RequestTimeout.Duration,
		})
		if err != nil {
			return err
		}
		factExtractor.WithUsageRecorder(budgetRepository).WithBudgetChecker(budgetService)
		factWorker, err := biz.NewFactExtractionWorker(biz.FactExtractionWorkerDependencies{
			Jobs: jobRepository, Messages: sessionRepository, Facts: factService, Extractor: factExtractor,
			Logger: slog.Default(),
		}, biz.FactExtractionWorkerConfig{
			WorkerID: workerID, MaxFacts: cfg.Agent.Memory.Facts.MaxFacts,
			Concurrency: cfg.Agent.Memory.Facts.Concurrency, PollInterval: cfg.Agent.Memory.Facts.PollInterval.Duration,
			Lease: cfg.Agent.Memory.Facts.Lease.Duration, Heartbeat: cfg.Agent.Memory.Facts.Heartbeat.Duration,
		})
		if err != nil {
			return err
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			factWorker.Start(workerContext)
		}()
	}
	if runWorkers && workerComponentEnabled(cfg.Agent.Worker, "background") && cfg.Agent.Memory.SkillSynthesis.Enabled {
		var defaultSkillPolicyID *uuid.UUID
		if raw := strings.TrimSpace(cfg.Agent.Memory.SkillSynthesis.DefaultModelPolicyID); raw != "" {
			parsed, parseErr := uuid.Parse(raw)
			if parseErr != nil {
				return parseErr
			}
			defaultSkillPolicyID = &parsed
		}
		skillExtractor, err := biz.NewSkillLLMExtractor(userSkillRepository, catalogRepository, catalogRepository, credentialKeys, biz.UserSkillLLMConfig{
			DefaultModelPolicyID:      defaultSkillPolicyID,
			DefaultModelPolicyVersion: cfg.Agent.Memory.SkillSynthesis.DefaultModelPolicyVersion,
			MaxMessageBytes:           cfg.Agent.Memory.SkillSynthesis.MaxMessageBytes,
			MaxOutputTokens:           cfg.Agent.Memory.SkillSynthesis.MaxOutputTokens,
			Temperature:               cfg.Agent.Memory.SkillSynthesis.Temperature,
			RequestTimeout:            cfg.Agent.Memory.SkillSynthesis.RequestTimeout.Duration,
		})
		if err != nil {
			return err
		}
		skillExtractor.WithUsageRecorder(budgetRepository).WithBudgetChecker(budgetService)
		skillSynthesisWorker, err := biz.NewSkillSynthesisWorker(biz.SkillSynthesisWorkerDependencies{
			Jobs: jobRepository, Settings: userSkillRepository, Sessions: sessionRepository, UserSkills: userSkillService,
			Runs:      userSkillRepository,
			Extractor: skillExtractor, Logger: slog.Default(),
		}, biz.SkillSynthesisConfig{
			Enabled:                cfg.Agent.Memory.SkillSynthesis.Enabled,
			LookbackHours:          cfg.Agent.Memory.SkillSynthesis.LookbackHours,
			MinConfidence:          cfg.Agent.Memory.SkillSynthesis.MinConfidence,
			MaxSkillsPerUserPerDay: cfg.Agent.Memory.SkillSynthesis.MaxSkillsPerUserPerDay,
			MaxSessionsPerUser:     cfg.Agent.Memory.SkillSynthesis.MaxSessionsPerUser,
			MaxMessagesPerSession:  cfg.Agent.Memory.SkillSynthesis.MaxMessagesPerSession,
			MaxMessageBytes:        cfg.Agent.Memory.SkillSynthesis.MaxMessageBytes,
			MaxOutputTokens:        cfg.Agent.Memory.SkillSynthesis.MaxOutputTokens,
			Temperature:            cfg.Agent.Memory.SkillSynthesis.Temperature,
			RequestTimeout:         cfg.Agent.Memory.SkillSynthesis.RequestTimeout.Duration,
			Concurrency:            cfg.Agent.Memory.SkillSynthesis.Concurrency,
			PollInterval:           cfg.Agent.Memory.SkillSynthesis.PollInterval.Duration,
			Lease:                  cfg.Agent.Memory.SkillSynthesis.Lease.Duration,
			Heartbeat:              cfg.Agent.Memory.SkillSynthesis.Heartbeat.Duration,
		})
		if err != nil {
			return err
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			skillSynthesisWorker.Start(workerContext)
		}()
		skillSynthesisMaintenance, err := biz.NewSkillSynthesisMaintenance(userRepository, userSkillRepository, jobRepository, biz.SkillSynthesisConfig{
			Enabled:       cfg.Agent.Memory.SkillSynthesis.Enabled,
			Interval:      cfg.Agent.Memory.SkillSynthesis.Interval.Duration,
			LookbackHours: cfg.Agent.Memory.SkillSynthesis.LookbackHours,
		}, slog.Default(), nil)
		if err != nil {
			return err
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			skillSynthesisMaintenance.Start(workerContext)
		}()
	}
	if runWorkers && workerComponentEnabled(cfg.Agent.Worker, "deletion") && cfg.Agent.Worker.Enabled {
		deletionExecutor := data.NewDeletionExecutor(appDB, hotMessages)
		deletionWorker, err := biz.NewDeletionWorker(biz.DeletionWorkerDependencies{
			Requests: deletionRepository, Executor: deletionExecutor, Logger: slog.Default(),
		}, biz.DeletionWorkerConfig{
			WorkerID: workerID, Concurrency: 1, PollInterval: time.Second,
			Lease: 5 * time.Minute, Heartbeat: 30 * time.Second,
		})
		if err != nil {
			return err
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			deletionWorker.Start(workerContext)
		}()
	}
	// 飞书事件与回复 Worker 复用统一 Worker 生命周期，停机时随上下文取消退出。
	if runWorkers && workerComponentEnabled(cfg.Agent.Worker, "feishu") && feishuEventWorker != nil {
		workers.Add(1)
		go func() {
			defer workers.Done()
			feishuEventWorker.Start(workerContext)
		}()
	}
	if runWorkers && workerComponentEnabled(cfg.Agent.Worker, "feishu") && feishuReplyWorker != nil {
		workers.Add(1)
		go func() {
			defer workers.Done()
			feishuReplyWorker.Start(workerContext)
		}()
	}
	if runWorkers && workerComponentEnabled(cfg.Agent.Worker, "feishu") && feishuLongConnection != nil {
		workers.Add(1)
		go func() {
			defer workers.Done()
			feishuLongConnection.Start(workerContext)
		}()
	}
	var runErr error
	if app != nil {
		runErr = app.Run()
	} else {
		// Worker-only 进程没有 HTTP 服务，等待 SIGTERM/SIGINT 后优雅退出。
		slog.Info("worker-only role started", "role", role, "worker_id", workerID)
		signalContext, stopSignal := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		<-signalContext.Done()
		stopSignal()
	}
	stopWorker()
	workers.Wait()
	return runErr
}

func closeDB(db interface{ DB() (*sql.DB, error) }) {
	sqlDB, err := db.DB()
	if err == nil {
		_ = sqlDB.Close()
	}
}
