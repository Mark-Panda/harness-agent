package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func dur(v time.Duration) Duration {
	return Duration{Duration: v}
}

// newValidConfig 返回一份通过 Validate 的最小完整配置。
func newValidConfig() *Config {
	return &Config{
		Environment: "development",
		Server: ServerConfig{
			HTTP:            HTTPServerConfig{Address: "0.0.0.0:8080", Timeout: dur(15 * time.Second)},
			GRPC:            GRPCServerConfig{Address: "0.0.0.0:9090"},
			ShutdownTimeout: dur(10 * time.Second),
		},
		Data: DataConfig{
			Postgres: PostgresConfig{MaxOpenConns: 10},
		},
		Agent: AgentConfig{
			ModelRouting: ModelRoutingConfig{
				MinContextTokens:    MinGenerationContextTokens,
				MaxPromptTokens:     300000,
				ReserveOutputTokens: 16384,
				SafetyMarginTokens:  4096,
				MaxAttemptsPerModel: 2,
				MaxAttemptsPerRun:   6,
				MaxSwitchesPerRun:   2,
				CircuitBreaker:      CircuitBreakerConfig{FailureThreshold: 3, Window: dur(30 * time.Second), OpenFor: dur(30 * time.Second)},
			},
			ExecutionRecords: ExecutionRecordsConfig{DefaultCaptureMode: "redacted", MaxInlinePayloadBytes: 262144},
		},
		Retention: RetentionConfig{ExecutionRecords: dur(2160 * time.Hour), AuditLogs: dur(8760 * time.Hour)},
		Auth: AuthConfig{
			JWTIssuer:           "harness",
			JWTSigningKeyID:     "key-1",
			JWTSigningKeySecret: strings.Repeat("a", 32),
			RefreshTokenPepper:  strings.Repeat("b", 32),
			LoginRateLimit:      RateLimitConfig{MaxAttempts: 5, Window: dur(5 * time.Minute)},
		},
		Credentials: CredentialsConfig{ActiveKeyID: "cred-1", ActiveKeySecret: strings.Repeat("c", 32)},
		Audit:       AuditConfig{PseudonymKey: strings.Repeat("p", 32)},
		Quotas:      QuotasConfig{MaxAgentsPerUser: 50, MaxSessionsPerUser: 200},
	}
}

// enableWorker 启用一个合法的内嵌 Worker 配置。
func enableWorker(c *Config) {
	c.Agent.Worker = WorkerConfig{
		Enabled: true, Concurrency: 4,
		PollInterval: dur(1 * time.Second), Lease: dur(60 * time.Second), Heartbeat: dur(10 * time.Second),
		CancelPoll: dur(2 * time.Second), RequestTimeout: dur(30 * time.Second),
		BackoffBase: dur(1 * time.Second), MaxBackoff: dur(30 * time.Second), MaxRetryAfter: dur(60 * time.Second),
		MCPTimeout: dur(10 * time.Second), ReactMaxSteps: 5, RunDeadline: dur(5 * time.Minute),
		ApprovalPoll: dur(5 * time.Second), MaxOutputTokens: 4096, Temperature: 0.7,
		GlobalConcurrency: 100, PerUserConcurrency: 10, PerProviderConcurrency: 20,
		ConcurrencyLease: dur(2 * time.Minute),
	}
}

// enableSummary 启用合法的滚动摘要配置。
func enableSummary(c *Config) {
	c.Agent.Memory.Summary = SummaryConfig{
		Enabled: true, TriggerMessages: 100, KeepRecent: 20, MaxSourceMessages: 50,
		MaxInputBytes: 65536, MaxMessageBytes: 4096, MaxOutputTokens: 1024, MaxSummaryBytes: 8192,
		Temperature: 0.3, RequestTimeout: dur(30 * time.Second), Concurrency: 2,
		PollInterval: dur(5 * time.Second), Lease: dur(60 * time.Second), Heartbeat: dur(10 * time.Second),
	}
}

// enableVector 启用合法的向量记忆配置。
func enableVector(c *Config) {
	c.Agent.Memory.Vector = VectorConfig{
		Enabled: true, RetrieverTopK: 20, MMRTop: 5, MMRLambda: 0.5, MinSimilarity: 0.2,
		EmbeddingTimeout: dur(10 * time.Second), RecentMessages: 20, BatchSize: 16, Concurrency: 2,
		PollInterval: dur(5 * time.Second), Lease: dur(60 * time.Second), Heartbeat: dur(10 * time.Second),
	}
}

// enableFacts 启用合法的结构化事实提取配置。
func enableFacts(c *Config) {
	c.Agent.Memory.Facts = FactConfig{
		Enabled: true, MinConfidence: 0.6, CorrectionGuard: 0.9, MaxFactsPerUser: 100,
		RecentMessages: 20, MinMessages: 4, MaxFacts: 8, MaxMessageBytes: 4096, MaxOutputTokens: 1024,
		Temperature: 0.2, RequestTimeout: dur(30 * time.Second), Concurrency: 2,
		PollInterval: dur(5 * time.Second), Lease: dur(60 * time.Second), Heartbeat: dur(10 * time.Second),
	}
}

// enableFeishu 启用合法的飞书 Webhook 配置。
func enableFeishu(c *Config) {
	c.Feishu = FeishuConfig{
		Enabled: true, EventMode: FeishuEventModeWebhook,
		AppID: "app-id", AppSecret: "app-secret", VerificationToken: "verify-token",
		EncryptKeys:    []FeishuEncryptKey{{ID: "k1", Secret: "s1"}},
		EventWindow:    dur(5 * time.Minute),
		MaxBodyBytes:   1048576,
		DefaultAgentID: "agent-1", DefaultAgentVersion: 1, Mode: "react",
		Worker: FeishuWorkerConfig{Concurrency: 2, PollInterval: dur(1 * time.Second), Lease: dur(30 * time.Second)},
		Reply: FeishuReplyConfig{
			Concurrency: 2, PollInterval: dur(1 * time.Second), Lease: dur(30 * time.Second),
			PollRunInterval: dur(2 * time.Second), RunWaitTimeout: dur(10 * time.Minute),
			MaxTextBytes: 4096, CardUpdateInterval: dur(5 * time.Second),
			MaxConcurrentPerChat: 1, MaxConcurrentPerUser: 2,
		},
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{name: "valid base"},
		{name: "unsupported environment", mutate: func(c *Config) { c.Environment = "bogus" }, wantErr: "unsupported environment"},
		{name: "missing http addr", mutate: func(c *Config) { c.Server.HTTP.Address = " " }, wantErr: "server.http.addr is required"},
		{name: "missing grpc addr", mutate: func(c *Config) { c.Server.GRPC.Address = "" }, wantErr: "server.grpc.addr is required"},
		{name: "zero http timeout", mutate: func(c *Config) { c.Server.HTTP.Timeout = dur(0) }, wantErr: "server timeouts must be positive"},
		{name: "negative shutdown timeout", mutate: func(c *Config) { c.Server.ShutdownTimeout = dur(-time.Second) }, wantErr: "server timeouts must be positive"},
		{name: "tls missing cert and key", mutate: func(c *Config) { c.Server.HTTP.TLS.Enabled = true }, wantErr: "cert_file and key_file are required"},
		{name: "tls invalid min version", mutate: func(c *Config) {
			c.Server.HTTP.TLS = HTTPTLSConfig{Enabled: true, CertFile: "cert.pem", KeyFile: "key.pem", MinVersion: "1.1"}
		}, wantErr: "min_version must be 1.2 or 1.3"},
		{name: "tls valid", mutate: func(c *Config) {
			c.Server.HTTP.TLS = HTTPTLSConfig{Enabled: true, CertFile: "cert.pem", KeyFile: "key.pem", MinVersion: "1.2"}
		}},
		{name: "non positive max open conns", mutate: func(c *Config) { c.Data.Postgres.MaxOpenConns = 0 }, wantErr: "data.postgres.max_open_conns must be positive"},
		{name: "non positive max prompt tokens", mutate: func(c *Config) { c.Agent.ModelRouting.MaxPromptTokens = 0 }, wantErr: "model token budgets must be positive"},
		{name: "budgets exceed min context", mutate: func(c *Config) { c.Agent.ModelRouting.MaxPromptTokens = 990000 }, wantErr: "exceed the minimum model context"},
		{name: "non positive attempts per model", mutate: func(c *Config) { c.Agent.ModelRouting.MaxAttemptsPerModel = 0 }, wantErr: "invalid model attempt limits"},
		{name: "run attempts below model attempts", mutate: func(c *Config) { c.Agent.ModelRouting.MaxAttemptsPerRun = 1 }, wantErr: "invalid model attempt limits"},
		{name: "negative switches", mutate: func(c *Config) { c.Agent.ModelRouting.MaxSwitchesPerRun = -1 }, wantErr: "invalid model switch limit"},
		{name: "switches not below run attempts", mutate: func(c *Config) { c.Agent.ModelRouting.MaxSwitchesPerRun = 6 }, wantErr: "invalid model switch limit"},
		{name: "circuit breaker zero threshold", mutate: func(c *Config) { c.Agent.ModelRouting.CircuitBreaker.FailureThreshold = 0 }, wantErr: "circuit breaker thresholds"},
		{name: "circuit breaker zero window", mutate: func(c *Config) { c.Agent.ModelRouting.CircuitBreaker.Window = dur(0) }, wantErr: "circuit breaker thresholds"},
		{name: "circuit breaker zero open for", mutate: func(c *Config) { c.Agent.ModelRouting.CircuitBreaker.OpenFor = dur(0) }, wantErr: "circuit breaker thresholds"},
		// Worker
		{name: "worker valid", mutate: enableWorker},
		{name: "worker zero concurrency", mutate: func(c *Config) { enableWorker(c); c.Agent.Worker.Concurrency = 0 }, wantErr: "agent.worker.concurrency must be positive"},
		{name: "worker zero poll interval", mutate: func(c *Config) { enableWorker(c); c.Agent.Worker.PollInterval = dur(0) }, wantErr: "intervals and request timeout must be positive"},
		{name: "worker zero lease", mutate: func(c *Config) { enableWorker(c); c.Agent.Worker.Lease = dur(0) }, wantErr: "intervals and request timeout must be positive"},
		{name: "worker zero cancel poll", mutate: func(c *Config) { enableWorker(c); c.Agent.Worker.CancelPoll = dur(0) }, wantErr: "intervals and request timeout must be positive"},
		{name: "worker zero request timeout", mutate: func(c *Config) { enableWorker(c); c.Agent.Worker.RequestTimeout = dur(0) }, wantErr: "intervals and request timeout must be positive"},
		{name: "worker zero heartbeat", mutate: func(c *Config) { enableWorker(c); c.Agent.Worker.Heartbeat = dur(0) }, wantErr: "heartbeat must be positive and shorter than lease"},
		{name: "worker heartbeat not shorter than lease", mutate: func(c *Config) { enableWorker(c); c.Agent.Worker.Heartbeat = c.Agent.Worker.Lease }, wantErr: "heartbeat must be positive and shorter than lease"},
		{name: "worker zero backoff base", mutate: func(c *Config) { enableWorker(c); c.Agent.Worker.BackoffBase = dur(0) }, wantErr: "backoff and retry-after budgets must be positive"},
		{name: "worker max backoff below base", mutate: func(c *Config) { enableWorker(c); c.Agent.Worker.MaxBackoff = dur(500 * time.Millisecond) }, wantErr: "backoff and retry-after budgets must be positive"},
		{name: "worker zero retry after", mutate: func(c *Config) { enableWorker(c); c.Agent.Worker.MaxRetryAfter = dur(0) }, wantErr: "backoff and retry-after budgets must be positive"},
		{name: "worker zero mcp timeout", mutate: func(c *Config) { enableWorker(c); c.Agent.Worker.MCPTimeout = dur(0) }, wantErr: "mcp_timeout must be positive"},
		{name: "worker react steps too large", mutate: func(c *Config) { enableWorker(c); c.Agent.Worker.ReactMaxSteps = 101 }, wantErr: "react_max_steps must be within [0, 100]"},
		{name: "worker react steps negative", mutate: func(c *Config) { enableWorker(c); c.Agent.Worker.ReactMaxSteps = -1 }, wantErr: "react_max_steps must be within [0, 100]"},
		{name: "worker run deadline below one minute", mutate: func(c *Config) { enableWorker(c); c.Agent.Worker.RunDeadline = dur(30 * time.Second) }, wantErr: "run_deadline must be positive and at least 1m"},
		{name: "worker negative react tokens", mutate: func(c *Config) { enableWorker(c); c.Agent.Worker.ReactMaxTotalTokens = -1 }, wantErr: "token and cost budgets must not be negative"},
		{name: "worker negative react cost", mutate: func(c *Config) { enableWorker(c); c.Agent.Worker.ReactMaxCost = -1 }, wantErr: "token and cost budgets must not be negative"},
		{name: "worker negative supervisor tokens", mutate: func(c *Config) { enableWorker(c); c.Agent.Worker.SupervisorMaxTokens = -1 }, wantErr: "token and cost budgets must not be negative"},
		{name: "worker negative supervisor cost", mutate: func(c *Config) { enableWorker(c); c.Agent.Worker.SupervisorMaxCost = -1 }, wantErr: "token and cost budgets must not be negative"},
		{name: "worker react steps exhaust attempts", mutate: func(c *Config) {
			enableWorker(c)
			c.Agent.Worker.ReactMaxSteps = 6
		}, wantErr: "leave room in agent.model_routing.max_attempts_per_run"},
		{name: "worker zero approval poll", mutate: func(c *Config) { enableWorker(c); c.Agent.Worker.ApprovalPoll = dur(0) }, wantErr: "approval_poll must be positive"},
		{name: "worker negative output tokens", mutate: func(c *Config) { enableWorker(c); c.Agent.Worker.MaxOutputTokens = -1 }, wantErr: "output tokens and temperature are invalid"},
		{name: "worker temperature too high", mutate: func(c *Config) { enableWorker(c); c.Agent.Worker.Temperature = 2.1 }, wantErr: "output tokens and temperature are invalid"},
		{name: "worker zero global concurrency", mutate: func(c *Config) { enableWorker(c); c.Agent.Worker.GlobalConcurrency = 0 }, wantErr: "concurrency limits must be positive"},
		{name: "worker per user exceeds global", mutate: func(c *Config) { enableWorker(c); c.Agent.Worker.PerUserConcurrency = 101 }, wantErr: "per-user concurrency cannot exceed global concurrency"},
		{name: "worker zero concurrency lease", mutate: func(c *Config) { enableWorker(c); c.Agent.Worker.ConcurrencyLease = dur(0) }, wantErr: "concurrency_lease must be positive"},
		// Budgets
		{name: "negative user daily tokens", mutate: func(c *Config) { c.Agent.Budgets.UserDailyTokens = -1 }, wantErr: "budgets limits must not be negative"},
		{name: "negative agent daily cost", mutate: func(c *Config) { c.Agent.Budgets.AgentDailyCost = -0.1 }, wantErr: "budgets limits must not be negative"},
		{name: "alert threshold above one", mutate: func(c *Config) { c.Agent.Budgets.AlertThreshold = 1.1 }, wantErr: "alert_threshold must be within [0, 1]"},
		{name: "negative alert threshold", mutate: func(c *Config) { c.Agent.Budgets.AlertThreshold = -0.1 }, wantErr: "alert_threshold must be within [0, 1]"},
		// Memory
		{name: "negative hot window messages", mutate: func(c *Config) { c.Agent.Memory.HotWindowMessages = -1 }, wantErr: "hot window size and ttl must not be negative"},
		{name: "negative hot window ttl", mutate: func(c *Config) { c.Agent.Memory.HotWindowTTL = dur(-time.Hour) }, wantErr: "hot window size and ttl must not be negative"},
		// Summary
		{name: "summary valid", mutate: enableSummary},
		{name: "summary trigger not above keep recent", mutate: func(c *Config) {
			enableSummary(c)
			c.Agent.Memory.Summary.TriggerMessages = 20
		}, wantErr: "trigger_messages must be greater than keep_recent"},
		{name: "summary zero size limit", mutate: func(c *Config) { enableSummary(c); c.Agent.Memory.Summary.MaxSourceMessages = 0 }, wantErr: "summary size limits must be positive"},
		{name: "summary temperature out of range", mutate: func(c *Config) { enableSummary(c); c.Agent.Memory.Summary.Temperature = 3 }, wantErr: "summary.temperature must be within [0, 2]"},
		{name: "summary zero request timeout", mutate: func(c *Config) { enableSummary(c); c.Agent.Memory.Summary.RequestTimeout = dur(0) }, wantErr: "summary timeouts and intervals must be positive"},
		{name: "summary heartbeat not shorter than lease", mutate: func(c *Config) {
			enableSummary(c)
			c.Agent.Memory.Summary.Heartbeat = c.Agent.Memory.Summary.Lease
		}, wantErr: "summary.heartbeat must be positive and shorter than lease"},
		{name: "summary zero concurrency", mutate: func(c *Config) { enableSummary(c); c.Agent.Memory.Summary.Concurrency = 0 }, wantErr: "summary.concurrency must be positive"},
		// Vector
		{name: "vector valid", mutate: enableVector},
		{name: "vector mmr top above top k", mutate: func(c *Config) { enableVector(c); c.Agent.Memory.Vector.MMRTop = 21 }, wantErr: "top_k and mmr_top must be positive"},
		{name: "vector mmr lambda out of range", mutate: func(c *Config) { enableVector(c); c.Agent.Memory.Vector.MMRLambda = 1.1 }, wantErr: "mmr_lambda must be within [0, 1]"},
		{name: "vector min similarity out of range", mutate: func(c *Config) { enableVector(c); c.Agent.Memory.Vector.MinSimilarity = 2 }, wantErr: "min_similarity must be within [-1, 1]"},
		{name: "vector zero embedding timeout", mutate: func(c *Config) { enableVector(c); c.Agent.Memory.Vector.EmbeddingTimeout = dur(0) }, wantErr: "vector timeouts and intervals must be positive"},
		{name: "vector heartbeat not shorter than lease", mutate: func(c *Config) {
			enableVector(c)
			c.Agent.Memory.Vector.Heartbeat = c.Agent.Memory.Vector.Lease
		}, wantErr: "vector.heartbeat must be positive and shorter than lease"},
		{name: "vector zero batch size", mutate: func(c *Config) { enableVector(c); c.Agent.Memory.Vector.BatchSize = 0 }, wantErr: "message, batch and concurrency limits must be positive"},
		{name: "vector maintenance valid", mutate: func(c *Config) {
			enableVector(c)
			c.Agent.Memory.Vector.MaintenanceEnabled = true
			c.Agent.Memory.Vector.MaintenanceInterval = dur(time.Minute)
			c.Agent.Memory.Vector.CoverageThreshold = 0.9
			c.Agent.Memory.Vector.BackfillBatch = 100
		}},
		{name: "vector maintenance zero interval", mutate: func(c *Config) {
			enableVector(c)
			c.Agent.Memory.Vector.MaintenanceEnabled = true
			c.Agent.Memory.Vector.CoverageThreshold = 0.9
			c.Agent.Memory.Vector.BackfillBatch = 100
		}, wantErr: "maintenance interval and backfill batch must be positive"},
		{name: "vector maintenance invalid coverage", mutate: func(c *Config) {
			enableVector(c)
			c.Agent.Memory.Vector.MaintenanceEnabled = true
			c.Agent.Memory.Vector.MaintenanceInterval = dur(time.Minute)
			c.Agent.Memory.Vector.CoverageThreshold = 0
			c.Agent.Memory.Vector.BackfillBatch = 100
		}, wantErr: "coverage_threshold must be within (0, 1]"},
		// Facts
		{name: "facts valid", mutate: enableFacts},
		{name: "facts min confidence out of range", mutate: func(c *Config) { enableFacts(c); c.Agent.Memory.Facts.MinConfidence = 0 }, wantErr: "confidence thresholds must be within (0, 1]"},
		{name: "facts correction guard out of range", mutate: func(c *Config) { enableFacts(c); c.Agent.Memory.Facts.CorrectionGuard = 2 }, wantErr: "confidence thresholds must be within (0, 1]"},
		{name: "facts zero limits", mutate: func(c *Config) { enableFacts(c); c.Agent.Memory.Facts.MaxFactsPerUser = 0 }, wantErr: "facts limits must be positive"},
		{name: "facts min messages above recent", mutate: func(c *Config) { enableFacts(c); c.Agent.Memory.Facts.MinMessages = 21 }, wantErr: "min_messages cannot exceed recent_messages"},
		{name: "facts zero message bytes", mutate: func(c *Config) { enableFacts(c); c.Agent.Memory.Facts.MaxMessageBytes = 0 }, wantErr: "message and output limits must be positive"},
		{name: "facts temperature out of range", mutate: func(c *Config) { enableFacts(c); c.Agent.Memory.Facts.Temperature = -0.1 }, wantErr: "facts.temperature must be within [0, 2]"},
		{name: "facts zero poll interval", mutate: func(c *Config) { enableFacts(c); c.Agent.Memory.Facts.PollInterval = dur(0) }, wantErr: "facts timeouts and intervals must be positive"},
		{name: "facts heartbeat not shorter than lease", mutate: func(c *Config) {
			enableFacts(c)
			c.Agent.Memory.Facts.Heartbeat = c.Agent.Memory.Facts.Lease
		}, wantErr: "facts.heartbeat must be positive and shorter than lease"},
		{name: "facts zero concurrency", mutate: func(c *Config) { enableFacts(c); c.Agent.Memory.Facts.Concurrency = 0 }, wantErr: "facts.concurrency must be positive"},
		// Execution records & observability & retention
		{name: "unsupported capture mode", mutate: func(c *Config) { c.Agent.ExecutionRecords.DefaultCaptureMode = "raw" }, wantErr: "unsupported execution capture mode"},
		{name: "metadata only capture mode", mutate: func(c *Config) { c.Agent.ExecutionRecords.DefaultCaptureMode = "metadata_only" }},
		{name: "non positive inline payload bytes", mutate: func(c *Config) { c.Agent.ExecutionRecords.MaxInlinePayloadBytes = 0 }, wantErr: "max_inline_payload_bytes must be positive"},
		{name: "execution payloads forbidden", mutate: func(c *Config) { c.Observability.IncludeExecutionPayloads = true }, wantErr: "include_execution_payloads must remain false"},
		{name: "zero retention", mutate: func(c *Config) { c.Retention.ExecutionRecords = dur(0) }, wantErr: "retention.execution_records and retention.audit_logs must be positive"},
		// Sandbox
		{name: "sandbox missing endpoint", mutate: func(c *Config) { c.Agent.Sandbox.RequireForHighRisk = true }, wantErr: "endpoint and token are required"},
		{name: "sandbox zero timeout", mutate: func(c *Config) {
			c.Agent.Sandbox = SandboxConfig{RequireForHighRisk: true, Endpoint: "https://sandbox.internal", Token: "tok", MaxResponseBytes: 1024}
		}, wantErr: "timeout and max_response_bytes must be positive"},
		{name: "sandbox invalid url", mutate: func(c *Config) {
			c.Agent.Sandbox = SandboxConfig{RequireForHighRisk: true, Endpoint: "not a url", Token: "tok", Timeout: dur(10 * time.Second), MaxResponseBytes: 1024}
		}, wantErr: "endpoint must be a valid URL"},
		{name: "sandbox http without insecure flag", mutate: func(c *Config) {
			c.Agent.Sandbox = SandboxConfig{RequireForHighRisk: true, Endpoint: "http://sandbox.internal", Token: "tok", Timeout: dur(10 * time.Second), MaxResponseBytes: 1024}
		}, wantErr: "must use https unless allow_insecure is enabled"},
		{name: "sandbox http with insecure allowed", mutate: func(c *Config) {
			c.Agent.Sandbox = SandboxConfig{RequireForHighRisk: true, Endpoint: "http://sandbox.internal", Token: "tok", Timeout: dur(10 * time.Second), MaxResponseBytes: 1024, AllowInsecure: true}
		}},
		// Auth
		{name: "missing jwt issuer", mutate: func(c *Config) { c.Auth.JWTIssuer = "" }, wantErr: "auth jwt issuer, signing key and refresh token pepper are required"},
		{name: "short jwt secret", mutate: func(c *Config) { c.Auth.JWTSigningKeySecret = strings.Repeat("a", 31) }, wantErr: "auth jwt issuer, signing key and refresh token pepper are required"},
		{name: "short refresh pepper", mutate: func(c *Config) { c.Auth.RefreshTokenPepper = strings.Repeat("b", 31) }, wantErr: "auth jwt issuer, signing key and refresh token pepper are required"},
		{name: "zero login attempts", mutate: func(c *Config) { c.Auth.LoginRateLimit.MaxAttempts = 0 }, wantErr: "login_rate_limit must have positive attempts and window"},
		{name: "zero login window", mutate: func(c *Config) { c.Auth.LoginRateLimit.Window = dur(0) }, wantErr: "login_rate_limit must have positive attempts and window"},
		// API rate limit & proxies
		{name: "negative api rate limit", mutate: func(c *Config) { c.Server.APIRateLimit.PerIPPerMinute = -1 }, wantErr: "api_rate_limit values must not be negative"},
		{name: "invalid trusted proxy", mutate: func(c *Config) { c.Server.TrustedProxies = []string{"bad"} }, wantErr: "invalid CIDR"},
		{name: "valid trusted proxies", mutate: func(c *Config) { c.Server.TrustedProxies = []string{"10.0.0.0/8", "192.168.0.0/16"} }},
		{name: "blank metrics token", mutate: func(c *Config) { c.Server.MetricsToken = "   " }, wantErr: "server.metrics_token must not be blank"},
		{name: "valid metrics token", mutate: func(c *Config) { c.Server.MetricsToken = "metrics-secret" }},
		// Audit & quotas
		{name: "short pseudonym key", mutate: func(c *Config) { c.Audit.PseudonymKey = "short" }, wantErr: "pseudonym_key must contain at least 32 bytes"},
		{name: "zero agents quota", mutate: func(c *Config) { c.Quotas.MaxAgentsPerUser = 0 }, wantErr: "quotas.max_agents_per_user and quotas.max_sessions_per_user must be positive"},
		{name: "zero sessions quota", mutate: func(c *Config) { c.Quotas.MaxSessionsPerUser = 0 }, wantErr: "quotas.max_agents_per_user and quotas.max_sessions_per_user must be positive"},
		// Feishu group policy
		{name: "feishu topic and root fallback conflict", mutate: func(c *Config) {
			c.Feishu.Group = FeishuGroupConfig{RequireTopic: true, AllowRootReplyFallback: true}
		}, wantErr: "root reply fallback cannot be enabled when topics are required"},
		// Production
		{name: "production valid", mutate: func(c *Config) {
			c.Environment = "production"
			c.Data.Postgres.DSN = "postgres://db/harness"
			c.Data.Postgres.AuthDSN = "postgres://db/harness_auth"
			c.Data.Redis.Address = "redis:6379"
			c.Data.Redis.Password = "secret"
			c.Server.CookieSecure = true
			c.Audit.SynchronousCriticalWrites = true
			c.Audit.HashChainEnabled = true
		}},
		{name: "production missing redis password", mutate: func(c *Config) {
			c.Environment = "production"
			c.Data.Postgres.DSN = "postgres://db/harness"
			c.Data.Postgres.AuthDSN = "postgres://db/harness_auth"
			c.Data.Redis.Address = "redis:6379"
			c.Server.CookieSecure = true
			c.Audit.SynchronousCriticalWrites = true
			c.Audit.HashChainEnabled = true
		}, wantErr: "production PostgreSQL and Redis secrets must be injected"},
		{name: "production insecure cookie", mutate: func(c *Config) {
			c.Environment = "production"
			c.Data.Postgres.DSN = "postgres://db/harness"
			c.Data.Postgres.AuthDSN = "postgres://db/harness_auth"
			c.Data.Redis.Address = "redis:6379"
			c.Data.Redis.Password = "secret"
			c.Audit.SynchronousCriticalWrites = true
			c.Audit.HashChainEnabled = true
		}, wantErr: "cookie_secure must be true in production"},
		{name: "production audit hardening required", mutate: func(c *Config) {
			c.Environment = "production"
			c.Data.Postgres.DSN = "postgres://db/harness"
			c.Data.Postgres.AuthDSN = "postgres://db/harness_auth"
			c.Data.Redis.Address = "redis:6379"
			c.Data.Redis.Password = "secret"
			c.Server.CookieSecure = true
		}, wantErr: "synchronous audit writes and hash chain are required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newValidConfig()
			if tt.mutate != nil {
				tt.mutate(cfg)
			}
			err := cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateFeishuEnabled(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{name: "webhook valid", mutate: enableFeishu},
		{name: "invalid login redirect url", mutate: func(c *Config) { enableFeishu(c); c.Feishu.LoginRedirectURL = "notaurl" }, wantErr: "login_redirect_url must be an absolute HTTP(S) URL"},
		{name: "valid login redirect url", mutate: func(c *Config) { enableFeishu(c); c.Feishu.LoginRedirectURL = "https://example.com/callback" }},
		{name: "zero event window", mutate: func(c *Config) { enableFeishu(c); c.Feishu.EventWindow = dur(0) }, wantErr: "event_window must be positive"},
		{name: "zero max body bytes", mutate: func(c *Config) { enableFeishu(c); c.Feishu.MaxBodyBytes = 0 }, wantErr: "max_body_bytes must be positive"},
		{name: "encrypt key missing id", mutate: func(c *Config) {
			enableFeishu(c)
			c.Feishu.EncryptKeys = []FeishuEncryptKey{{Secret: "s1"}}
		}, wantErr: "encrypt_keys[0].id is required"},
		{name: "encrypt key missing secret", mutate: func(c *Config) {
			enableFeishu(c)
			c.Feishu.EncryptKeys = []FeishuEncryptKey{{ID: "k1"}}
		}, wantErr: "encrypt_keys[0].secret is required"},
		{name: "duplicate encrypt key id", mutate: func(c *Config) {
			enableFeishu(c)
			c.Feishu.EncryptKeys = []FeishuEncryptKey{{ID: "k1", Secret: "s1"}, {ID: "k1", Secret: "s2"}}
		}, wantErr: "not unique"},
		{name: "empty encrypt key placeholder skipped", mutate: func(c *Config) {
			enableFeishu(c)
			c.Feishu.EncryptKeys = []FeishuEncryptKey{{}, {ID: "k1", Secret: "s1"}}
		}},
		{name: "missing app secret", mutate: func(c *Config) { enableFeishu(c); c.Feishu.AppSecret = " " }, wantErr: "feishu.app_secret is required"},
		{name: "missing verification token", mutate: func(c *Config) { enableFeishu(c); c.Feishu.VerificationToken = "" }, wantErr: "feishu.verification_token is required"},
		{name: "zero default agent version", mutate: func(c *Config) { enableFeishu(c); c.Feishu.DefaultAgentVersion = 0 }, wantErr: "default_agent_version must be positive"},
		{name: "negative default agent version in long connection", mutate: func(c *Config) {
			enableFeishu(c)
			c.Feishu.EventMode = FeishuEventModeLongConnection
			c.Feishu.DefaultAgentVersion = -1
		}, wantErr: "default_agent_version must not be negative"},
		{name: "zero worker concurrency", mutate: func(c *Config) { enableFeishu(c); c.Feishu.Worker.Concurrency = 0 }, wantErr: "feishu.worker.concurrency must be positive"},
		{name: "zero worker poll interval", mutate: func(c *Config) { enableFeishu(c); c.Feishu.Worker.PollInterval = dur(0) }, wantErr: "feishu.worker intervals and lease must be positive"},
		{name: "zero reply concurrency", mutate: func(c *Config) { enableFeishu(c); c.Feishu.Reply.Concurrency = 0 }, wantErr: "feishu.reply.concurrency must be positive"},
		{name: "zero reply lease", mutate: func(c *Config) { enableFeishu(c); c.Feishu.Reply.Lease = dur(0) }, wantErr: "feishu.reply intervals and lease must be positive"},
		{name: "zero reply poll run interval", mutate: func(c *Config) { enableFeishu(c); c.Feishu.Reply.PollRunInterval = dur(0) }, wantErr: "run polling interval and wait timeout must be positive"},
		{name: "zero reply max text bytes", mutate: func(c *Config) { enableFeishu(c); c.Feishu.Reply.MaxTextBytes = 0 }, wantErr: "reply.max_text_bytes must be positive"},
		{name: "zero reply per chat concurrency", mutate: func(c *Config) { enableFeishu(c); c.Feishu.Reply.MaxConcurrentPerChat = 0 }, wantErr: "reply concurrency limits must be positive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newValidConfig()
			tt.mutate(cfg)
			err := cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestDurationUnmarshalYAML(t *testing.T) {
	var d Duration
	if err := yaml.Unmarshal([]byte("5s"), &d); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if d.Duration != 5*time.Second {
		t.Fatalf("unexpected duration %s", d.Duration)
	}

	var nonScalar Duration
	if err := yaml.Unmarshal([]byte("{a: 1}"), &nonScalar); err == nil || !strings.Contains(err.Error(), "duration must be a scalar") {
		t.Fatalf("expected scalar error, got %v", err)
	}

	var invalid Duration
	if err := yaml.Unmarshal([]byte("not-a-duration"), &invalid); err == nil || !strings.Contains(err.Error(), "parse duration") {
		t.Fatalf("expected parse error, got %v", err)
	}
}

func TestLoadFileErrors(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil || !strings.Contains(err.Error(), "read config") {
		t.Fatalf("expected read error, got %v", err)
	}
	if _, err := Load(writeConfig(t, "server:\n  http: [")); err == nil || !strings.Contains(err.Error(), "decode config") {
		t.Fatalf("expected decode error, got %v", err)
	}
	badDuration := strings.Replace(validYAML, "timeout: 15s", "timeout: 15x", 1)
	if _, err := Load(writeConfig(t, badDuration)); err == nil || !strings.Contains(err.Error(), "parse duration") {
		t.Fatalf("expected duration parse error, got %v", err)
	}
	nonScalarDuration := strings.Replace(validYAML, "timeout: 15s", "timeout:\n      nested: 1", 1)
	if _, err := Load(writeConfig(t, nonScalarDuration)); err == nil || !strings.Contains(err.Error(), "duration must be a scalar") {
		t.Fatalf("expected scalar duration error, got %v", err)
	}
}

func TestLoadEnvironment(t *testing.T) {
	setCredentialEnvironment(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/harness")
	t.Setenv("AUTH_DATABASE_URL", "postgres://localhost/harness_auth")
	t.Setenv("REDIS_ADDR", "localhost:6379")
	t.Setenv("REDIS_PASSWORD", "secret")
	t.Chdir(t.TempDir())
	if err := os.Mkdir("configs", 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("configs", "config.yaml"), []byte(validYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	// 空环境名回落到 dev，无 overlay 时只加载 base。
	cfg, err := LoadEnvironment("")
	if err != nil {
		t.Fatalf("LoadEnvironment() error = %v", err)
	}
	if cfg.Environment != "development" {
		t.Fatalf("unexpected environment %q", cfg.Environment)
	}

	// 存在对应环境 overlay 时叠加覆盖。
	overlay := "environment: staging\nserver:\n  http:\n    addr: 127.0.0.1:8081\n"
	if err := os.WriteFile(filepath.Join("configs", "config.staging.yaml"), []byte(overlay), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadEnvironment("staging")
	if err != nil {
		t.Fatalf("LoadEnvironment(staging) error = %v", err)
	}
	if cfg.Environment != "staging" || cfg.Server.HTTP.Address != "127.0.0.1:8081" {
		t.Fatalf("overlay not applied: %+v", cfg.Server.HTTP)
	}
}

func TestLoadEnvironmentMissingBase(t *testing.T) {
	t.Chdir(t.TempDir())
	if _, err := LoadEnvironment(""); err == nil || !strings.Contains(err.Error(), "read config") {
		t.Fatalf("expected read config error, got %v", err)
	}
}

// TestValidateWorkerRoleAndPools 校验进程角色与资源池配置边界。
func TestValidateWorkerRoleAndPools(t *testing.T) {
	cfg := newValidConfig()
	enableWorker(cfg)
	cfg.Agent.Worker.Role = "worker"
	cfg.Agent.Worker.Pools = []WorkerPoolConfig{{
		Name: "react", Lanes: []string{"react"}, Concurrency: 4, GlobalConcurrency: 8,
		Lease: dur(60 * time.Second), Heartbeat: dur(20 * time.Second),
	}}
	cfg.Agent.Worker.PoolFilter = "react"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid worker pools rejected: %v", err)
	}

	// worker 角色漏配 pool_filter 会静默启动全部池，必须拒绝。
	cfg.Agent.Worker.PoolFilter = ""
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "pool_filter is required") {
		t.Fatalf("expected explicit pool_filter error, got %v", err)
	}
	// 不启动 execution 的组件不受池过滤约束。
	cfg.Agent.Worker.Components = "background,feishu"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("non-execution worker should not require pool_filter: %v", err)
	}
	cfg.Agent.Worker.Components = ""
	cfg.Agent.Worker.PoolFilter = "react"

	cfg.Agent.Worker.Role = "invalid"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "worker.role") {
		t.Fatalf("expected invalid role error, got %v", err)
	}

	cfg.Agent.Worker.Role = "all"
	cfg.Agent.Worker.Pools = append(cfg.Agent.Worker.Pools, cfg.Agent.Worker.Pools[0])
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected duplicate pool error, got %v", err)
	}
}

// TestValidateWorkerPoolLanes 校验资源池 lane 与 pool_filter 的启动期约束。
func TestValidateWorkerPoolLanes(t *testing.T) {
	cases := []struct {
		name            string
		pools           []WorkerPoolConfig
		filter          string
		drainNegative   bool
		pendingNegative bool
		wantErr         string
	}{
		{
			name:    "unsupported lane",
			pools:   []WorkerPoolConfig{{Name: "react", Lanes: []string{"reakt"}}},
			wantErr: "unsupported run lane",
		},
		{
			name:    "background lane on execution pool",
			pools:   []WorkerPoolConfig{{Name: "react", Lanes: []string{"background"}}},
			wantErr: "unsupported run lane",
		},
		{
			name: "lane claimed by two pools",
			pools: []WorkerPoolConfig{
				{Name: "react-a", Lanes: []string{"react"}},
				{Name: "react-b", Lanes: []string{"react"}},
			},
			wantErr: "claimed by both",
		},
		{
			name:    "duplicate lane in one pool",
			pools:   []WorkerPoolConfig{{Name: "react", Lanes: []string{"react", "REACT"}}},
			wantErr: "duplicate lane",
		},
		{
			name:    "empty lane",
			pools:   []WorkerPoolConfig{{Name: "react", Lanes: []string{" "}}},
			wantErr: "must not contain empty lane",
		},
		{
			name:    "missing lanes",
			pools:   []WorkerPoolConfig{{Name: "react"}},
			wantErr: "lanes must not be empty",
		},
		{
			name:    "filter without pools",
			filter:  "react",
			wantErr: "requires agent.worker.pools",
		},
		{
			name:    "filter case insensitive",
			pools:   []WorkerPoolConfig{{Name: "react", Lanes: []string{"react"}}},
			filter:  "REACT",
			wantErr: "",
		},
		{
			name:   "star starts all pools",
			pools:  []WorkerPoolConfig{{Name: "react", Lanes: []string{"react"}}, {Name: "plan", Lanes: []string{"plan"}}},
			filter: "*",
		},
		{
			name:    "star mixed with names",
			pools:   []WorkerPoolConfig{{Name: "react", Lanes: []string{"react"}}},
			filter:  "*,plan",
			wantErr: "must not combine *",
		},
		{
			name:          "negative drain timeout",
			pools:         []WorkerPoolConfig{{Name: "react", Lanes: []string{"react"}}},
			filter:        "react",
			drainNegative: true,
			wantErr:       "drain_timeout",
		},
		{
			name:            "negative pending cap",
			pools:           []WorkerPoolConfig{{Name: "react", Lanes: []string{"react"}}},
			filter:          "react",
			pendingNegative: true,
			wantErr:         "max_pending_runs_per_user",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := newValidConfig()
			enableWorker(cfg)
			cfg.Agent.Worker.Pools = tc.pools
			cfg.Agent.Worker.PoolFilter = tc.filter
			if tc.drainNegative {
				cfg.Agent.Worker.DrainTimeout = dur(-time.Second)
			}
			if tc.pendingNegative {
				cfg.Agent.Worker.MaxPendingRunsPerUser = -1
			}
			err := cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}
