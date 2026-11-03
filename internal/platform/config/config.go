package config

import (
	"bytes"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const MinGenerationContextTokens int64 = 1_000_000

type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("duration must be a scalar")
	}
	value, err := time.ParseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("parse duration %q: %w", node.Value, err)
	}
	d.Duration = value
	return nil
}

type Config struct {
	Environment   string              `yaml:"environment"`
	Server        ServerConfig        `yaml:"server"`
	Data          DataConfig          `yaml:"data"`
	Agent         AgentConfig         `yaml:"agent"`
	Observability ObservabilityConfig `yaml:"observability"`
	Logging       LoggingConfig       `yaml:"logging"`
	Retention     RetentionConfig     `yaml:"retention"`
	Auth          AuthConfig          `yaml:"auth"`
	Credentials   CredentialsConfig   `yaml:"credentials"`
	Quotas        QuotasConfig        `yaml:"quotas"`
	Audit         AuditConfig         `yaml:"audit"`
	Feishu        FeishuConfig        `yaml:"feishu"`
}

// QuotasConfig 是用户级资源配额上限。
type QuotasConfig struct {
	// MaxAgentsPerUser 单用户 Agent 版本总数上限。
	MaxAgentsPerUser int `yaml:"max_agents_per_user"`
	// MaxSessionsPerUser 单用户会话总数上限。
	MaxSessionsPerUser int `yaml:"max_sessions_per_user"`
}

type ServerConfig struct {
	HTTP            HTTPServerConfig `yaml:"http"`
	GRPC            GRPCServerConfig `yaml:"grpc"`
	ShutdownTimeout Duration         `yaml:"shutdown_timeout"`
	// CookieSecure 控制刷新令牌与 CSRF Cookie 是否带 Secure 标记；生产环境必须为 true。
	CookieSecure bool `yaml:"cookie_secure"`
	// SSE 控制流式连接的容量与慢消费者写入超时。
	SSE SSEConfig `yaml:"sse"`
	// APIRateLimit 控制全局、用户与 IP 三个维度的 API 限流。
	APIRateLimit APIRateLimitConfig `yaml:"api_rate_limit"`
	// TrustedProxies 是可信反向代理的 CIDR 列表；为空时只使用直连地址，忽略转发头。
	TrustedProxies []string `yaml:"trusted_proxies"`
}

// APIRateLimitConfig 是 API 多维限流配置，0 表示关闭该维度。
type APIRateLimitConfig struct {
	// GlobalPerMinute 全实例每分钟请求上限。
	GlobalPerMinute int `yaml:"global_per_minute"`
	// PerUserPerMinute 单用户每分钟请求上限。
	PerUserPerMinute int `yaml:"per_user_per_minute"`
	// PerIPPerMinute 单 IP 每分钟请求上限。
	PerIPPerMinute int `yaml:"per_ip_per_minute"`
}

// SSEConfig 是 SSE 连接保护配置。
type SSEConfig struct {
	// MaxConnections 单实例 SSE 连接上限，0 表示使用默认值。
	MaxConnections int `yaml:"max_connections"`
	// WriteTimeout 单次 SSE 写入超时，0 表示使用默认值。
	WriteTimeout Duration `yaml:"write_timeout"`
}

// GRPCServerConfig 是 gRPC 监听配置。
type GRPCServerConfig struct {
	// Address gRPC 监听地址，如 0.0.0.0:9090。
	Address string `yaml:"addr"`
}

type HTTPServerConfig struct {
	Address string   `yaml:"addr"`
	Timeout Duration `yaml:"timeout"`
	// TLS 控制应用层 HTTPS；生产可在入口终止 TLS，但端到端链路必须加密。
	TLS HTTPTLSConfig `yaml:"tls"`
}

// HTTPTLSConfig 是应用层 TLS 配置；启用时证书与私钥必须同时提供。
type HTTPTLSConfig struct {
	// Enabled 是否直接由应用监听 HTTPS。
	Enabled bool `yaml:"enabled"`
	// CertFile 证书链文件。
	CertFile string `yaml:"cert_file"`
	// KeyFile 私钥文件。
	KeyFile string `yaml:"key_file"`
	// MinVersion 最低 TLS 版本，支持 1.2 或 1.3，默认 1.3。
	MinVersion string `yaml:"min_version"`
}

type DataConfig struct {
	Postgres PostgresConfig `yaml:"postgres"`
	Redis    RedisConfig    `yaml:"redis"`
}

type PostgresConfig struct {
	DSN          string `yaml:"dsn"`
	AuthDSN      string `yaml:"auth_dsn"`
	MaxOpenConns int    `yaml:"max_open_conns"`
}

type RedisConfig struct {
	Address  string `yaml:"addr"`
	Password string `yaml:"password"`
}

type AgentConfig struct {
	ModelRouting     ModelRoutingConfig     `yaml:"model_routing"`
	ExecutionRecords ExecutionRecordsConfig `yaml:"execution_records"`
	Worker           WorkerConfig           `yaml:"worker"`
	Budgets          BudgetsConfig          `yaml:"budgets"`
	Memory           MemoryConfig           `yaml:"memory"`
	Sandbox          SandboxConfig          `yaml:"sandbox"`
}

// SandboxConfig 配置高风险工具 Sandbox Runner。
type SandboxConfig struct {
	// RequireForHighRisk 高风险工具必须在独立 Sandbox Runner 中执行。
	RequireForHighRisk bool `yaml:"require_for_high_risk"`
	// Endpoint Sandbox Runner HTTP 端点。
	Endpoint string `yaml:"endpoint"`
	// Token 出口认证令牌。
	Token string `yaml:"token"`
	// AllowInsecure 是否允许明文 HTTP，仅限内网调试。
	AllowInsecure bool `yaml:"allow_insecure"`
	// Timeout 单次调用超时。
	Timeout Duration `yaml:"timeout"`
	// MaxResponseBytes 响应最大字节数。
	MaxResponseBytes int64 `yaml:"max_response_bytes"`
}

// MemoryConfig 是会话记忆缓存与摘要配置。
type MemoryConfig struct {
	// HotWindowMessages Redis 热窗口保留的最大消息数，零值使用默认 100。
	HotWindowMessages int `yaml:"hot_window_messages"`
	// HotWindowTTL 热窗口过期时间，零值使用默认 24 小时。
	HotWindowTTL Duration `yaml:"hot_window_ttl"`
	// Summary 异步滚动摘要配置。
	Summary SummaryConfig `yaml:"summary"`
	// Vector 长期记忆向量索引配置。
	Vector VectorConfig `yaml:"vector"`
	// Facts 结构化事实提取配置。
	Facts FactConfig `yaml:"facts"`
}

// FactConfig 是结构化事实提取与冲突策略配置。
type FactConfig struct {
	// Enabled 是否启用结构化事实提取。
	Enabled bool `yaml:"enabled"`
	// MinConfidence 候选写入的最低置信度。
	MinConfidence float64 `yaml:"min_confidence"`
	// CorrectionGuard 用户显式纠正后的覆盖保护阈值。
	CorrectionGuard float64 `yaml:"correction_guard"`
	// MaxFactsPerUser 单用户保留的生效事实上限。
	MaxFactsPerUser int `yaml:"max_facts_per_user"`
	// RecentMessages 触发时最多检查的最近消息数。
	RecentMessages int `yaml:"recent_messages"`
	// MinMessages 触发提取所需的最少消息数。
	MinMessages int `yaml:"min_messages"`
	// MaxFacts 单次提取的最大事实数。
	MaxFacts int `yaml:"max_facts"`
	// MaxMessageBytes 单条消息进入提示的最大字节数。
	MaxMessageBytes int `yaml:"max_message_bytes"`
	// MaxOutputTokens 事实 JSON 输出上限。
	MaxOutputTokens int `yaml:"max_output_tokens"`
	// Temperature 采样温度。
	Temperature float64 `yaml:"temperature"`
	// RequestTimeout 单次事实提取调用超时。
	RequestTimeout Duration `yaml:"request_timeout"`
	// Concurrency 单实例并发事实提取任务数。
	Concurrency int `yaml:"concurrency"`
	// PollInterval 无事实任务时的轮询间隔。
	PollInterval Duration `yaml:"poll_interval"`
	// Lease 事实任务租约时长。
	Lease Duration `yaml:"lease"`
	// Heartbeat 事实任务租约续约间隔。
	Heartbeat Duration `yaml:"heartbeat"`
}

// VectorConfig 是长期记忆 embedding 与索引维护配置。
type VectorConfig struct {
	// Enabled 是否启用长期记忆向量索引与检索。
	Enabled bool `yaml:"enabled"`
	// RetrieverTopK 向量检索候选数。
	RetrieverTopK int `yaml:"retriever_top_k"`
	// MMRTop MMR 最终返回数量。
	MMRTop int `yaml:"mmr_top"`
	// MMRLambda MMR 相关性权重。
	MMRLambda float64 `yaml:"mmr_lambda"`
	// MinSimilarity 最低余弦相似度。
	MinSimilarity float64 `yaml:"min_similarity"`
	// EmbeddingTimeout 单次 embedding 调用超时。
	EmbeddingTimeout Duration `yaml:"embedding_timeout"`
	// RecentMessages 消息写入触发时最多检查的最近消息数。
	RecentMessages int `yaml:"recent_messages"`
	// BatchSize 单次 embedding 请求的最大文本数。
	BatchSize int `yaml:"batch_size"`
	// Concurrency 单实例并发向量化任务数。
	Concurrency int `yaml:"concurrency"`
	// PollInterval 无向量化任务时的轮询间隔。
	PollInterval Duration `yaml:"poll_interval"`
	// Lease 向量化任务租约时长。
	Lease Duration `yaml:"lease"`
	// Heartbeat 向量化任务租约续约间隔。
	Heartbeat Duration `yaml:"heartbeat"`
	// MaintenanceEnabled 是否启用后台索引维护与回填。
	MaintenanceEnabled bool `yaml:"maintenance_enabled"`
	// MaintenanceInterval 后台维护轮询间隔。
	MaintenanceInterval Duration `yaml:"maintenance_interval"`
	// CoverageThreshold staging 索引切换为 active 的最低覆盖比例。
	CoverageThreshold float64 `yaml:"coverage_threshold"`
	// BackfillBatch 单次回填扫描的最大消息数。
	BackfillBatch int `yaml:"backfill_batch"`
}

// SummaryConfig 是异步滚动摘要任务配置。
type SummaryConfig struct {
	// Enabled 是否启用异步摘要任务与摘要规划。
	Enabled bool `yaml:"enabled"`
	// TriggerMessages 会话消息总数达到该值后触发摘要。
	TriggerMessages int `yaml:"trigger_messages"`
	// KeepRecent 始终保留在最近窗口、不进入摘要的消息条数。
	KeepRecent int `yaml:"keep_recent"`
	// MaxSourceMessages 单个摘要任务最多包含的新增消息数。
	MaxSourceMessages int `yaml:"max_source_messages"`
	// MaxInputBytes 单次摘要模型调用输入分块上限，超过后自动分块归并。
	MaxInputBytes int `yaml:"max_input_bytes"`
	// MaxMessageBytes 单条消息纳入摘要的最大字节数。
	MaxMessageBytes int `yaml:"max_message_bytes"`
	// MaxOutputTokens 摘要输出 Token 上限。
	MaxOutputTokens int `yaml:"max_output_tokens"`
	// MaxSummaryBytes 单份摘要落库最大字节数。
	MaxSummaryBytes int `yaml:"max_summary_bytes"`
	// Temperature 摘要采样温度。
	Temperature float64 `yaml:"temperature"`
	// RequestTimeout 单次摘要模型调用超时。
	RequestTimeout Duration `yaml:"request_timeout"`
	// Concurrency 单实例并发摘要任务数。
	Concurrency int `yaml:"concurrency"`
	// PollInterval 无摘要任务时的轮询间隔。
	PollInterval Duration `yaml:"poll_interval"`
	// Lease 摘要任务租约时长。
	Lease Duration `yaml:"lease"`
	// Heartbeat 摘要任务租约续约间隔。
	Heartbeat Duration `yaml:"heartbeat"`
}

// BudgetsConfig 是用户、Agent 与供应商的每日预算上限；零值表示该维度不限制。
type BudgetsConfig struct {
	// UserDailyTokens 单用户每日 Token 上限。
	UserDailyTokens int64 `yaml:"user_daily_tokens"`
	// UserDailyCost 单用户每日费用上限。
	UserDailyCost float64 `yaml:"user_daily_cost"`
	// AgentDailyTokens 单 Agent 每日 Token 上限。
	AgentDailyTokens int64 `yaml:"agent_daily_tokens"`
	// AgentDailyCost 单 Agent 每日费用上限。
	AgentDailyCost float64 `yaml:"agent_daily_cost"`
	// ProviderDailyTokens 单供应商每日 Token 上限。
	ProviderDailyTokens int64 `yaml:"provider_daily_tokens"`
	// ProviderDailyCost 单供应商每日费用上限。
	ProviderDailyCost float64 `yaml:"provider_daily_cost"`
	// AlertThreshold 告警阈值占上限比例，取值 (0,1]。
	AlertThreshold float64 `yaml:"alert_threshold"`
}

// WorkerConfig 是 Run 执行 Worker 的进程配置。
type WorkerConfig struct {
	// Enabled 是否在当前进程启动执行 Worker。
	Enabled bool `yaml:"enabled"`
	// WorkerID 实例标识；为空时启动时自动生成，多副本部署必须唯一。
	WorkerID string `yaml:"worker_id"`
	// Concurrency 单实例并发执行 Run 数。
	Concurrency int `yaml:"concurrency"`
	// PollInterval 无任务时的轮询间隔。
	PollInterval Duration `yaml:"poll_interval"`
	// Lease 任务与 Run 的执行租约时长。
	Lease Duration `yaml:"lease"`
	// Heartbeat 租约续约间隔，必须小于 Lease。
	Heartbeat Duration `yaml:"heartbeat"`
	// CancelPoll 取消状态轮询间隔。
	CancelPoll Duration `yaml:"cancel_poll"`
	// RequestTimeout 单次模型 HTTP 调用超时。
	RequestTimeout Duration `yaml:"request_timeout"`
	// MaxOutputTokens 单次调用输出上限，零表示使用模型上限。
	MaxOutputTokens int `yaml:"max_output_tokens"`
	// BackoffBase 可重试错误的基础退避时长。
	BackoffBase Duration `yaml:"backoff_base"`
	// MaxBackoff 指数退避上限。
	MaxBackoff Duration `yaml:"max_backoff"`
	// MaxRetryAfter 可接受的供应商 Retry-After 上限。
	MaxRetryAfter Duration `yaml:"max_retry_after"`
	// Temperature 采样温度。
	Temperature float64 `yaml:"temperature"`
	// MCPTimeout 单次 MCP 工具发现或调用的超时。
	MCPTimeout Duration `yaml:"mcp_timeout"`
	// ApprovalPoll 等待人工审批时的任务轮询间隔。
	ApprovalPoll Duration `yaml:"approval_poll"`
	// GlobalConcurrency 跨副本全局 Run 并发上限。
	GlobalConcurrency int `yaml:"global_concurrency"`
	// PerUserConcurrency 单用户 Run 并发上限。
	PerUserConcurrency int `yaml:"per_user_concurrency"`
	// PerProviderConcurrency 单供应商模型调用并发上限。
	PerProviderConcurrency int `yaml:"per_provider_concurrency"`
	// ConcurrencyLease 并发槽位租约，超时后自动回收崩溃实例占用的槽位。
	ConcurrencyLease Duration `yaml:"concurrency_lease"`
}

type ModelRoutingConfig struct {
	MinContextTokens    int64                `yaml:"min_context_tokens"`
	MaxPromptTokens     int64                `yaml:"max_prompt_tokens"`
	ReserveOutputTokens int64                `yaml:"reserve_output_tokens"`
	SafetyMarginTokens  int64                `yaml:"safety_margin_tokens"`
	MaxAttemptsPerModel int                  `yaml:"max_attempts_per_model"`
	MaxAttemptsPerRun   int                  `yaml:"max_attempts_per_run"`
	MaxSwitchesPerRun   int                  `yaml:"max_switches_per_run"`
	CircuitBreaker      CircuitBreakerConfig `yaml:"circuit_breaker"`
}

type CircuitBreakerConfig struct {
	FailureThreshold int      `yaml:"failure_threshold"`
	Window           Duration `yaml:"window"`
	OpenFor          Duration `yaml:"open_for"`
}

type ExecutionRecordsConfig struct {
	DefaultCaptureMode    string `yaml:"default_capture_mode"`
	MaxInlinePayloadBytes int64  `yaml:"max_inline_payload_bytes"`
	RetainPartialResponse bool   `yaml:"retain_partial_response"`
}

type ObservabilityConfig struct {
	IncludeExecutionPayloads bool          `yaml:"include_execution_payloads"`
	Tracing                  TracingConfig `yaml:"tracing"`
}

// LoggingConfig 配置结构化日志级别与输出格式；保留期由部署侧日志采集与轮转统一管理。
type LoggingConfig struct {
	// Level 日志级别：debug/info/warn/error，空值默认 info。
	Level string `yaml:"level"`
	// Format 输出格式：json/text，空值默认 json。
	Format string `yaml:"format"`
}

// RetentionConfig 配置执行载荷与审计数据的保留期；清理由受控保留任务执行。
type RetentionConfig struct {
	// ExecutionRecords 执行载荷保留期，超过后删除脱敏载荷但保留 Run/步骤元数据。
	ExecutionRecords Duration `yaml:"execution_records"`
	// AuditLogs 审计日志与检查点保留期，删除需要数据库保留期特权。
	AuditLogs Duration `yaml:"audit_logs"`
	// FeishuEvents 飞书原始事件回执保留期，用于限制重放载荷中的正文驻留时间。
	FeishuEvents Duration `yaml:"feishu_events"`
}

// TracingConfig 配置 OpenTelemetry OTLP HTTP 导出、采样与出口认证。
// 约束：Headers 只允许承载导出凭证，禁止写入用户或会话标识；
// ResourceAttributes 为全局低基数资源属性，Trace/日志正文默认不采集 Prompt、Completion 与工具原文。
type TracingConfig struct {
	// Enabled 是否启用 Trace 导出。
	Enabled bool `yaml:"enabled"`
	// ServiceName 服务名，写入 service.name。
	ServiceName string `yaml:"service_name"`
	// Endpoint OTLP HTTP 端点，支持 host:port 或完整 URL。
	Endpoint string `yaml:"endpoint"`
	// Insecure host:port 形式下允许明文 HTTP，仅限内网或本地调试。
	Insecure bool `yaml:"insecure"`
	// SampleRatio 采样比例，必须位于 (0, 1]。
	SampleRatio float64 `yaml:"sample_ratio"`
	// BatchTimeout 批量导出间隔。
	BatchTimeout Duration `yaml:"batch_timeout"`
	// ExportTimeout 单次导出超时。
	ExportTimeout Duration `yaml:"export_timeout"`
	// Headers 导出请求头，用于网关或 Collector 出口认证。
	Headers map[string]string `yaml:"headers"`
	// ResourceAttributes 附加资源属性。
	ResourceAttributes map[string]string `yaml:"resource_attributes"`
}

type AuthConfig struct {
	JWTIssuer           string               `yaml:"jwt_issuer"`
	JWTSigningKeyID     string               `yaml:"jwt_signing_key_id"`
	JWTSigningKeySecret string               `yaml:"jwt_signing_key_secret"`
	JWTVerificationKeys []JWTVerificationKey `yaml:"jwt_verification_keys"`
	RefreshTokenPepper  string               `yaml:"refresh_token_pepper"`
	// RegistrationEnabled 是否允许开放注册；关闭时由管理员邀请创建用户。
	RegistrationEnabled bool            `yaml:"registration_enabled"`
	LoginRateLimit      RateLimitConfig `yaml:"login_rate_limit"`
}

type CredentialsConfig struct {
	ActiveKeyID     string          `yaml:"active_key_id"`
	ActiveKeySecret string          `yaml:"active_key_secret"`
	PreviousKeys    []CredentialKey `yaml:"previous_keys"`
}

type CredentialKey struct {
	ID     string `yaml:"id"`
	Secret string `yaml:"secret"`
}

type RateLimitConfig struct {
	MaxAttempts int      `yaml:"max_attempts"`
	Window      Duration `yaml:"window"`
}

type JWTVerificationKey struct {
	ID     string `yaml:"id"`
	Secret string `yaml:"secret"`
}

type AuditConfig struct {
	SynchronousCriticalWrites bool   `yaml:"synchronous_critical_writes"`
	HashChainEnabled          bool   `yaml:"hash_chain_enabled"`
	PseudonymKey              string `yaml:"pseudonym_key"`
}

// FeishuConfig 是飞书集成配置；未启用时仅校验群聊策略，不影响本地开发。
type FeishuConfig struct {
	// Enabled 是否启用飞书事件接入与 Worker。
	Enabled bool `yaml:"enabled"`
	// AppID 飞书应用 App ID。
	AppID string `yaml:"app_id"`
	// AppSecret 飞书应用 App Secret，必须由环境变量注入。
	AppSecret string `yaml:"app_secret"`
	// VerificationToken 事件订阅 Verification Token。
	VerificationToken string `yaml:"verification_token"`
	// EncryptKeys 事件加密密钥列表，第一个为当前密钥，其余为轮换期历史密钥。
	EncryptKeys []FeishuEncryptKey `yaml:"encrypt_keys"`
	// EventWindow 允许的事件时间戳重放窗口。
	EventWindow Duration `yaml:"event_window"`
	// MaxBodyBytes 单次回调请求体上限。
	MaxBodyBytes int64 `yaml:"max_body_bytes"`
	// BotOpenID 机器人 open_id，用于精确判断群聊是否被 @。
	BotOpenID string `yaml:"bot_open_id"`
	// BotName 机器人显示名，open_id 未配置时的后备 @ 判断。
	BotName string `yaml:"bot_name"`
	// DefaultAgentID 飞书事件默认使用的已发布 Agent。
	DefaultAgentID string `yaml:"default_agent_id"`
	// DefaultAgentVersion 默认 Agent 版本号。
	DefaultAgentVersion int `yaml:"default_agent_version"`
	// Mode 默认编排模式：react/plan/super。
	Mode string `yaml:"mode"`
	// Worker 事件消费 Worker 配置。
	Worker FeishuWorkerConfig `yaml:"worker"`
	// Reply 外发回复 Worker 配置。
	Reply FeishuReplyConfig `yaml:"reply"`
	// Group 群聊话题与回复策略。
	Group FeishuGroupConfig `yaml:"group"`
}

// FeishuEncryptKey 是飞书事件加密密钥版本。
type FeishuEncryptKey struct {
	// ID 密钥版本标识，用于轮换与排查。
	ID string `yaml:"id"`
	// Secret 控制台配置的 Encrypt Key 原文。
	Secret string `yaml:"secret"`
}

// FeishuWorkerConfig 是事件消费 Worker 的并发与轮询配置。
type FeishuWorkerConfig struct {
	// Concurrency 单实例并发消费数。
	Concurrency int `yaml:"concurrency"`
	// PollInterval 无任务时的轮询间隔。
	PollInterval Duration `yaml:"poll_interval"`
	// Lease 任务租约时长。
	Lease Duration `yaml:"lease"`
}

// FeishuReplyConfig 是外发回复 Worker 的投递与降级配置。
type FeishuReplyConfig struct {
	// Concurrency 并发投递数。
	Concurrency int `yaml:"concurrency"`
	// PollInterval 无任务时的轮询间隔。
	PollInterval Duration `yaml:"poll_interval"`
	// Lease 外发记录租约时长。
	Lease Duration `yaml:"lease"`
	// PollRunInterval Run 未结束时再次检查的间隔。
	PollRunInterval Duration `yaml:"poll_run_interval"`
	// RunWaitTimeout Run 长时间未完成时的降级阈值。
	RunWaitTimeout Duration `yaml:"run_wait_timeout"`
	// MaxTextBytes 单条回复文本上限，按 UTF-8 边界截断。
	MaxTextBytes int `yaml:"max_text_bytes"`
	// CardUpdateInterval 同一卡片两次增量更新的最小间隔，避免放大飞书限流。
	CardUpdateInterval Duration `yaml:"card_update_interval"`
	// MaxConcurrentPerChat 单个飞书会话的并发投递上限。
	MaxConcurrentPerChat int `yaml:"max_concurrent_per_chat"`
	// MaxConcurrentPerUser 单个用户的并发投递上限。
	MaxConcurrentPerUser int `yaml:"max_concurrent_per_user"`
}

type FeishuGroupConfig struct {
	RequireTopic           bool `yaml:"require_topic"`
	AutoReplyInBoundTopic  bool `yaml:"auto_reply_in_bound_topic"`
	AllowRootReplyFallback bool `yaml:"allow_root_reply_fallback"`
}

func LoadEnvironment(environment string) (*Config, error) {
	if environment == "" {
		environment = "dev"
	}
	basePath := "configs/config.yaml"
	overlayPath := fmt.Sprintf("configs/config.%s.yaml", environment)
	if _, err := os.Stat(overlayPath); err == nil {
		return Load(basePath, overlayPath)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("inspect config overlay: %w", err)
	}
	return Load(basePath)
}

func Load(basePath string, overlayPaths ...string) (*Config, error) {
	var cfg Config
	for _, path := range append([]string{basePath}, overlayPaths...) {
		raw, err := os.ReadFile(path) // #nosec G304 -- 配置文件路径由启动参数与固定 overlay 决定
		if err != nil {
			return nil, fmt.Errorf("read config %s: %w", path, err)
		}
		decoder := yaml.NewDecoder(bytes.NewReader([]byte(os.ExpandEnv(string(raw)))))
		decoder.KnownFields(true)
		if err := decoder.Decode(&cfg); err != nil {
			return nil, fmt.Errorf("decode config %s: %w", path, err)
		}
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}
	return &cfg, nil
}

func (c Config) Validate() error {
	switch c.Environment {
	case "development", "staging", "production":
	default:
		return fmt.Errorf("unsupported environment %q", c.Environment)
	}
	if strings.TrimSpace(c.Server.HTTP.Address) == "" {
		return fmt.Errorf("server.http.addr is required")
	}
	if strings.TrimSpace(c.Server.GRPC.Address) == "" {
		return fmt.Errorf("server.grpc.addr is required")
	}
	if c.Server.HTTP.Timeout.Duration <= 0 || c.Server.ShutdownTimeout.Duration <= 0 {
		return fmt.Errorf("server timeouts must be positive")
	}
	if c.Server.HTTP.TLS.Enabled {
		if strings.TrimSpace(c.Server.HTTP.TLS.CertFile) == "" || strings.TrimSpace(c.Server.HTTP.TLS.KeyFile) == "" {
			return fmt.Errorf("server.http.tls.cert_file and key_file are required when TLS is enabled")
		}
		switch c.Server.HTTP.TLS.MinVersion {
		case "", "1.2", "1.3":
		default:
			return fmt.Errorf("server.http.tls.min_version must be 1.2 or 1.3")
		}
	}
	if c.Data.Postgres.MaxOpenConns <= 0 {
		return fmt.Errorf("data.postgres.max_open_conns must be positive")
	}
	routing := c.Agent.ModelRouting
	if routing.MinContextTokens < MinGenerationContextTokens {
		return fmt.Errorf("agent.model_routing.min_context_tokens must be at least %d", MinGenerationContextTokens)
	}
	if routing.MaxPromptTokens <= 0 || routing.ReserveOutputTokens <= 0 || routing.SafetyMarginTokens <= 0 {
		return fmt.Errorf("model token budgets must be positive")
	}
	if routing.MaxPromptTokens+routing.ReserveOutputTokens+routing.SafetyMarginTokens > routing.MinContextTokens {
		return fmt.Errorf("prompt, output and safety budgets exceed the minimum model context")
	}
	if routing.MaxAttemptsPerModel <= 0 || routing.MaxAttemptsPerRun < routing.MaxAttemptsPerModel {
		return fmt.Errorf("invalid model attempt limits")
	}
	if routing.MaxSwitchesPerRun < 0 || routing.MaxSwitchesPerRun >= routing.MaxAttemptsPerRun {
		return fmt.Errorf("invalid model switch limit")
	}
	if routing.CircuitBreaker.FailureThreshold <= 0 || routing.CircuitBreaker.Window.Duration <= 0 || routing.CircuitBreaker.OpenFor.Duration <= 0 {
		return fmt.Errorf("circuit breaker thresholds and durations must be positive")
	}
	worker := c.Agent.Worker
	if worker.Enabled {
		if worker.Concurrency <= 0 {
			return fmt.Errorf("agent.worker.concurrency must be positive")
		}
		if worker.PollInterval.Duration <= 0 || worker.Lease.Duration <= 0 || worker.CancelPoll.Duration <= 0 || worker.RequestTimeout.Duration <= 0 {
			return fmt.Errorf("agent.worker intervals and request timeout must be positive")
		}
		if worker.Heartbeat.Duration <= 0 || worker.Heartbeat.Duration >= worker.Lease.Duration {
			return fmt.Errorf("agent.worker.heartbeat must be positive and shorter than lease")
		}
		if worker.BackoffBase.Duration <= 0 || worker.MaxBackoff.Duration < worker.BackoffBase.Duration || worker.MaxRetryAfter.Duration <= 0 {
			return fmt.Errorf("agent.worker backoff and retry-after budgets must be positive")
		}
		if worker.MCPTimeout.Duration <= 0 {
			return fmt.Errorf("agent.worker.mcp_timeout must be positive")
		}
		if worker.ApprovalPoll.Duration <= 0 {
			return fmt.Errorf("agent.worker.approval_poll must be positive")
		}
		if worker.MaxOutputTokens < 0 || worker.Temperature < 0 || worker.Temperature > 2 {
			return fmt.Errorf("agent.worker output tokens and temperature are invalid")
		}
		if worker.GlobalConcurrency <= 0 || worker.PerUserConcurrency <= 0 || worker.PerProviderConcurrency <= 0 {
			return fmt.Errorf("agent.worker concurrency limits must be positive")
		}
		if worker.PerUserConcurrency > worker.GlobalConcurrency {
			return fmt.Errorf("agent.worker per-user concurrency cannot exceed global concurrency")
		}
		if worker.ConcurrencyLease.Duration <= 0 {
			return fmt.Errorf("agent.worker.concurrency_lease must be positive")
		}
	}
	budgets := c.Agent.Budgets
	if budgets.UserDailyTokens < 0 || budgets.AgentDailyTokens < 0 || budgets.ProviderDailyTokens < 0 ||
		budgets.UserDailyCost < 0 || budgets.AgentDailyCost < 0 || budgets.ProviderDailyCost < 0 {
		return fmt.Errorf("agent.budgets limits must not be negative")
	}
	// 告警阈值零值表示使用默认 0.8，只有负数或超过 100% 才是非法配置。
	if budgets.AlertThreshold < 0 || budgets.AlertThreshold > 1 {
		return fmt.Errorf("agent.budgets.alert_threshold must be within [0, 1]")
	}
	memory := c.Agent.Memory
	if memory.HotWindowMessages < 0 || memory.HotWindowTTL.Duration < 0 {
		return fmt.Errorf("agent.memory hot window size and ttl must not be negative")
	}
	summary := memory.Summary
	if summary.Enabled {
		if summary.TriggerMessages <= 0 || summary.KeepRecent <= 0 || summary.TriggerMessages <= summary.KeepRecent {
			return fmt.Errorf("agent.memory.summary trigger_messages must be greater than keep_recent")
		}
		if summary.MaxSourceMessages <= 0 || summary.MaxInputBytes <= 0 || summary.MaxMessageBytes <= 0 ||
			summary.MaxOutputTokens <= 0 || summary.MaxSummaryBytes <= 0 {
			return fmt.Errorf("agent.memory.summary size limits must be positive")
		}
		if summary.Temperature < 0 || summary.Temperature > 2 {
			return fmt.Errorf("agent.memory.summary.temperature must be within [0, 2]")
		}
		if summary.RequestTimeout.Duration <= 0 || summary.PollInterval.Duration <= 0 || summary.Lease.Duration <= 0 {
			return fmt.Errorf("agent.memory.summary timeouts and intervals must be positive")
		}
		if summary.Heartbeat.Duration <= 0 || summary.Heartbeat.Duration >= summary.Lease.Duration {
			return fmt.Errorf("agent.memory.summary.heartbeat must be positive and shorter than lease")
		}
		if summary.Concurrency <= 0 {
			return fmt.Errorf("agent.memory.summary.concurrency must be positive")
		}
	}
	vector := memory.Vector
	if vector.Enabled {
		if vector.RetrieverTopK <= 0 || vector.MMRTop <= 0 || vector.MMRTop > vector.RetrieverTopK {
			return fmt.Errorf("agent.memory.vector top_k and mmr_top must be positive and mmr_top <= top_k")
		}
		if vector.MMRLambda < 0 || vector.MMRLambda > 1 {
			return fmt.Errorf("agent.memory.vector.mmr_lambda must be within [0, 1]")
		}
		if vector.MinSimilarity < -1 || vector.MinSimilarity > 1 {
			return fmt.Errorf("agent.memory.vector.min_similarity must be within [-1, 1]")
		}
		if vector.EmbeddingTimeout.Duration <= 0 || vector.PollInterval.Duration <= 0 || vector.Lease.Duration <= 0 {
			return fmt.Errorf("agent.memory.vector timeouts and intervals must be positive")
		}
		if vector.Heartbeat.Duration <= 0 || vector.Heartbeat.Duration >= vector.Lease.Duration {
			return fmt.Errorf("agent.memory.vector.heartbeat must be positive and shorter than lease")
		}
		if vector.RecentMessages <= 0 || vector.BatchSize <= 0 || vector.Concurrency <= 0 {
			return fmt.Errorf("agent.memory.vector message, batch and concurrency limits must be positive")
		}
		if vector.MaintenanceEnabled {
			if vector.MaintenanceInterval.Duration <= 0 || vector.BackfillBatch <= 0 {
				return fmt.Errorf("agent.memory.vector maintenance interval and backfill batch must be positive")
			}
			if vector.CoverageThreshold <= 0 || vector.CoverageThreshold > 1 {
				return fmt.Errorf("agent.memory.vector.coverage_threshold must be within (0, 1]")
			}
		}
	}
	facts := memory.Facts
	if facts.Enabled {
		if facts.MinConfidence <= 0 || facts.MinConfidence > 1 || facts.CorrectionGuard <= 0 || facts.CorrectionGuard > 1 {
			return fmt.Errorf("agent.memory.facts confidence thresholds must be within (0, 1]")
		}
		if facts.MaxFactsPerUser <= 0 || facts.RecentMessages <= 0 || facts.MinMessages <= 0 || facts.MaxFacts <= 0 {
			return fmt.Errorf("agent.memory.facts limits must be positive")
		}
		if facts.MinMessages > facts.RecentMessages {
			return fmt.Errorf("agent.memory.facts min_messages cannot exceed recent_messages")
		}
		if facts.MaxMessageBytes <= 0 || facts.MaxOutputTokens <= 0 {
			return fmt.Errorf("agent.memory.facts message and output limits must be positive")
		}
		if facts.Temperature < 0 || facts.Temperature > 2 {
			return fmt.Errorf("agent.memory.facts.temperature must be within [0, 2]")
		}
		if facts.RequestTimeout.Duration <= 0 || facts.PollInterval.Duration <= 0 || facts.Lease.Duration <= 0 {
			return fmt.Errorf("agent.memory.facts timeouts and intervals must be positive")
		}
		if facts.Heartbeat.Duration <= 0 || facts.Heartbeat.Duration >= facts.Lease.Duration {
			return fmt.Errorf("agent.memory.facts.heartbeat must be positive and shorter than lease")
		}
		if facts.Concurrency <= 0 {
			return fmt.Errorf("agent.memory.facts.concurrency must be positive")
		}
	}
	records := c.Agent.ExecutionRecords
	if records.DefaultCaptureMode != "redacted" && records.DefaultCaptureMode != "metadata_only" {
		return fmt.Errorf("unsupported execution capture mode %q", records.DefaultCaptureMode)
	}
	if records.MaxInlinePayloadBytes <= 0 {
		return fmt.Errorf("agent.execution_records.max_inline_payload_bytes must be positive")
	}
	if c.Observability.IncludeExecutionPayloads {
		return fmt.Errorf("observability.include_execution_payloads must remain false")
	}
	tracing := c.Observability.Tracing
	if tracing.Enabled {
		if strings.TrimSpace(tracing.Endpoint) == "" {
			return fmt.Errorf("observability.tracing.endpoint is required when tracing is enabled")
		}
		if strings.Contains(tracing.Endpoint, "://") {
			parsed, err := url.Parse(tracing.Endpoint)
			if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
				return fmt.Errorf("observability.tracing.endpoint must be a valid http(s) URL")
			}
		}
		if tracing.SampleRatio <= 0 || tracing.SampleRatio > 1 {
			return fmt.Errorf("observability.tracing.sample_ratio must be within (0, 1]")
		}
		if tracing.BatchTimeout.Duration <= 0 || tracing.ExportTimeout.Duration <= 0 {
			return fmt.Errorf("observability.tracing batch and export timeouts must be positive")
		}
	}
	if c.Retention.ExecutionRecords.Duration <= 0 || c.Retention.AuditLogs.Duration <= 0 {
		return fmt.Errorf("retention.execution_records and retention.audit_logs must be positive")
	}
	sandbox := c.Agent.Sandbox
	if sandbox.RequireForHighRisk {
		if strings.TrimSpace(sandbox.Endpoint) == "" || strings.TrimSpace(sandbox.Token) == "" {
			return fmt.Errorf("agent.sandbox endpoint and token are required when high-risk tools are enforced")
		}
		if sandbox.Timeout.Duration <= 0 || sandbox.MaxResponseBytes <= 0 {
			return fmt.Errorf("agent.sandbox timeout and max_response_bytes must be positive")
		}
		parsed, err := url.Parse(sandbox.Endpoint)
		if err != nil || parsed.Host == "" {
			return fmt.Errorf("agent.sandbox endpoint must be a valid URL")
		}
		if parsed.Scheme != "https" && !sandbox.AllowInsecure {
			return fmt.Errorf("agent.sandbox endpoint must use https unless allow_insecure is enabled")
		}
	}
	if c.Auth.JWTIssuer == "" || c.Auth.JWTSigningKeyID == "" || len(c.Auth.JWTSigningKeySecret) < 32 || len(c.Auth.RefreshTokenPepper) < 32 {
		return fmt.Errorf("auth jwt issuer, signing key and refresh token pepper are required")
	}
	if c.Auth.LoginRateLimit.MaxAttempts <= 0 || c.Auth.LoginRateLimit.Window.Duration <= 0 {
		return fmt.Errorf("auth.login_rate_limit must have positive attempts and window")
	}
	// API 限流配额 0 表示关闭该维度，负数视为非法配置。
	if c.Server.APIRateLimit.GlobalPerMinute < 0 || c.Server.APIRateLimit.PerUserPerMinute < 0 || c.Server.APIRateLimit.PerIPPerMinute < 0 {
		return fmt.Errorf("server.api_rate_limit values must not be negative")
	}
	// 可信代理必须是合法 CIDR：错误配置会让来源 IP 推导失准，直接影响限流与审计。
	for _, proxy := range c.Server.TrustedProxies {
		if _, _, err := net.ParseCIDR(strings.TrimSpace(proxy)); err != nil {
			return fmt.Errorf("server.trusted_proxies contains invalid CIDR %q", proxy)
		}
	}
	if len(c.Audit.PseudonymKey) < 32 {
		return fmt.Errorf("audit.pseudonym_key must contain at least 32 bytes")
	}
	if c.Quotas.MaxAgentsPerUser <= 0 || c.Quotas.MaxSessionsPerUser <= 0 {
		return fmt.Errorf("quotas.max_agents_per_user and quotas.max_sessions_per_user must be positive")
	}
	if c.Feishu.Group.RequireTopic && c.Feishu.Group.AllowRootReplyFallback {
		return fmt.Errorf("feishu root reply fallback cannot be enabled when topics are required")
	}
	if err := c.validateFeishu(); err != nil {
		return err
	}
	if c.Environment == "production" {
		if strings.TrimSpace(c.Data.Postgres.DSN) == "" || strings.TrimSpace(c.Data.Postgres.AuthDSN) == "" || strings.TrimSpace(c.Data.Redis.Address) == "" || c.Data.Redis.Password == "" {
			return fmt.Errorf("production PostgreSQL and Redis secrets must be injected")
		}
		// 生产环境必须使用 HTTPS Cookie，防止刷新令牌在明文链路泄露。
		if !c.Server.CookieSecure {
			return fmt.Errorf("server.cookie_secure must be true in production")
		}
		if !c.Audit.SynchronousCriticalWrites || !c.Audit.HashChainEnabled {
			return fmt.Errorf("production synchronous audit writes and hash chain are required")
		}
	}
	if err := c.validateCredentials(); err != nil {
		return err
	}
	return nil
}

// validateFeishu 校验飞书集成配置；未启用时允许全部留空，避免本地开发强依赖飞书。
// 关键步骤：启用时必须具备应用凭据、事件校验密钥与默认 Agent，生产环境额外强制签名与时间窗。
func (c Config) validateFeishu() error {
	feishu := c.Feishu
	if !feishu.Enabled {
		return nil
	}
	if strings.TrimSpace(feishu.AppID) == "" {
		return fmt.Errorf("feishu.app_id is required when feishu is enabled")
	}
	if strings.TrimSpace(feishu.AppSecret) == "" {
		return fmt.Errorf("feishu.app_secret is required when feishu is enabled")
	}
	if strings.TrimSpace(feishu.VerificationToken) == "" {
		return fmt.Errorf("feishu.verification_token is required when feishu is enabled")
	}
	if feishu.EventWindow.Duration <= 0 {
		return fmt.Errorf("feishu.event_window must be positive when feishu is enabled")
	}
	if feishu.MaxBodyBytes <= 0 {
		return fmt.Errorf("feishu.max_body_bytes must be positive when feishu is enabled")
	}
	if len(feishu.EncryptKeys) == 0 {
		return fmt.Errorf("feishu.encrypt_keys is required when feishu is enabled")
	}
	keyIDs := map[string]struct{}{}
	for i, key := range feishu.EncryptKeys {
		if strings.TrimSpace(key.ID) == "" {
			return fmt.Errorf("feishu.encrypt_keys[%d].id is required", i)
		}
		if strings.TrimSpace(key.Secret) == "" {
			return fmt.Errorf("feishu.encrypt_keys[%d].secret is required", i)
		}
		if _, exists := keyIDs[key.ID]; exists {
			return fmt.Errorf("feishu encrypt key id %q is not unique", key.ID)
		}
		keyIDs[key.ID] = struct{}{}
	}
	if strings.TrimSpace(feishu.DefaultAgentID) == "" {
		return fmt.Errorf("feishu.default_agent_id is required when feishu is enabled")
	}
	if feishu.DefaultAgentVersion <= 0 {
		return fmt.Errorf("feishu.default_agent_version must be positive when feishu is enabled")
	}
	switch feishu.Mode {
	case "react", "plan", "super":
	default:
		return fmt.Errorf("unsupported feishu mode %q", feishu.Mode)
	}
	worker := feishu.Worker
	if worker.Concurrency <= 0 {
		return fmt.Errorf("feishu.worker.concurrency must be positive")
	}
	if worker.PollInterval.Duration <= 0 || worker.Lease.Duration <= 0 {
		return fmt.Errorf("feishu.worker intervals and lease must be positive")
	}
	reply := feishu.Reply
	if reply.Concurrency <= 0 {
		return fmt.Errorf("feishu.reply.concurrency must be positive")
	}
	if reply.PollInterval.Duration <= 0 || reply.Lease.Duration <= 0 {
		return fmt.Errorf("feishu.reply intervals and lease must be positive")
	}
	if reply.PollRunInterval.Duration <= 0 || reply.RunWaitTimeout.Duration <= 0 {
		return fmt.Errorf("feishu.reply run polling interval and wait timeout must be positive")
	}
	if reply.MaxTextBytes <= 0 {
		return fmt.Errorf("feishu.reply.max_text_bytes must be positive")
	}
	if reply.CardUpdateInterval.Duration <= 0 {
		return fmt.Errorf("feishu.reply.card_update_interval must be positive")
	}
	if reply.MaxConcurrentPerChat <= 0 || reply.MaxConcurrentPerUser <= 0 {
		return fmt.Errorf("feishu.reply concurrency limits must be positive")
	}
	// 生产环境必须启用事件签名与时间窗校验，禁止明文回调。
	if c.Environment == "production" && (len(feishu.EncryptKeys) == 0 || feishu.EventWindow.Duration <= 0) {
		return fmt.Errorf("production feishu callback signature and replay window are required")
	}
	return nil
}

// validateCredentials 校验凭据加密密钥版本配置。
func (c Config) validateCredentials() error {
	if strings.TrimSpace(c.Credentials.ActiveKeyID) == "" {
		return fmt.Errorf("credentials.active_key_id is required")
	}
	if len(c.Credentials.ActiveKeySecret) != 32 {
		return fmt.Errorf("credentials.active_key_secret must be exactly 32 bytes")
	}
	keyIDs := map[string]struct{}{c.Credentials.ActiveKeyID: {}}
	for i, key := range c.Credentials.PreviousKeys {
		if strings.TrimSpace(key.ID) == "" {
			return fmt.Errorf("credentials.previous_keys[%d].id is required", i)
		}
		if _, exists := keyIDs[key.ID]; exists {
			return fmt.Errorf("credentials key id %q is not unique", key.ID)
		}
		if len(key.Secret) != 32 {
			return fmt.Errorf("credentials.previous_keys[%d].secret must be exactly 32 bytes", i)
		}
		keyIDs[key.ID] = struct{}{}
	}
	return nil
}
