package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validYAML = `
environment: development
server:
  http:
    addr: 0.0.0.0:8080
    timeout: 15s
  grpc:
    addr: 0.0.0.0:9090
  shutdown_timeout: 10s
data:
  postgres:
    dsn: ${DATABASE_URL}
    auth_dsn: ${AUTH_DATABASE_URL}
    max_open_conns: 30
  redis:
    addr: ${REDIS_ADDR}
    password: ${REDIS_PASSWORD}
agent:
  model_routing:
    min_context_tokens: 1000000
    max_prompt_tokens: 300000
    reserve_output_tokens: 16384
    safety_margin_tokens: 4096
    max_attempts_per_model: 2
    max_attempts_per_run: 6
    max_switches_per_run: 2
    circuit_breaker:
      failure_threshold: 3
      window: 30s
      open_for: 30s
  execution_records:
    default_capture_mode: redacted
    max_inline_payload_bytes: 262144
    retain_partial_response: true
auth:
  jwt_issuer: harness-test
  jwt_signing_key_id: test
  jwt_signing_key_secret: 01234567890123456789012345678901
  jwt_verification_keys: []
  refresh_token_pepper: 12345678901234567890123456789012
  login_rate_limit:
    max_attempts: 5
    window: 5m
credentials:
  active_key_id: ${CREDENTIAL_ENCRYPTION_KEY_ID}
  active_key_secret: ${CREDENTIAL_ENCRYPTION_KEY_SECRET}
  previous_keys: []
quotas:
  max_agents_per_user: 50
  max_sessions_per_user: 200
observability:
  include_execution_payloads: false
retention:
  execution_records: 2160h
  audit_logs: 8760h
audit:
  synchronous_critical_writes: true
  hash_chain_enabled: true
  pseudonym_key: 01234567890123456789012345678901
feishu:
  group:
    require_topic: true
    auto_reply_in_bound_topic: true
    allow_root_reply_fallback: false
`

const credentialTestSecret = "01234567890123456789012345678901"

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func setCredentialEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("CREDENTIAL_ENCRYPTION_KEY_ID", "credentials-test")
	t.Setenv("CREDENTIAL_ENCRYPTION_KEY_SECRET", credentialTestSecret)
}

func TestLoadExpandsEnvironment(t *testing.T) {
	setCredentialEnvironment(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/harness")
	t.Setenv("AUTH_DATABASE_URL", "postgres://localhost/harness_auth")
	t.Setenv("REDIS_ADDR", "localhost:6379")
	t.Setenv("REDIS_PASSWORD", "secret")
	cfg, err := Load(writeConfig(t, validYAML))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Data.Postgres.DSN != "postgres://localhost/harness" {
		t.Fatalf("unexpected DSN %q", cfg.Data.Postgres.DSN)
	}
	if cfg.Server.HTTP.Timeout.Duration != 15*time.Second {
		t.Fatalf("unexpected timeout %s", cfg.Server.HTTP.Timeout.Duration)
	}
	if cfg.Credentials.ActiveKeyID != "credentials-test" || cfg.Credentials.ActiveKeySecret != credentialTestSecret {
		t.Fatal("credential environment variables were not expanded")
	}
}

func TestLoadOverlayPreservesInheritedValues(t *testing.T) {
	setCredentialEnvironment(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/harness")
	t.Setenv("AUTH_DATABASE_URL", "postgres://localhost/harness_auth")
	t.Setenv("REDIS_ADDR", "localhost:6379")
	t.Setenv("REDIS_PASSWORD", "secret")
	base := writeConfig(t, validYAML)
	overlay := writeConfig(t, "server:\n  http:\n    addr: 127.0.0.1:9090\n")
	cfg, err := Load(base, overlay)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Server.HTTP.Address != "127.0.0.1:9090" {
		t.Fatalf("overlay address = %q", cfg.Server.HTTP.Address)
	}
	if cfg.Server.HTTP.Timeout.Duration != 15*time.Second {
		t.Fatalf("inherited timeout = %s", cfg.Server.HTTP.Timeout.Duration)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	_, err := Load(writeConfig(t, validYAML+"\nunknown_field: true\n"))
	if err == nil || !strings.Contains(err.Error(), "unknown_field") {
		t.Fatalf("expected unknown field error, got %v", err)
	}
}

func TestLoadRejectsSmallContextModels(t *testing.T) {
	content := strings.Replace(validYAML, "min_context_tokens: 1000000", "min_context_tokens: 999999", 1)
	_, err := Load(writeConfig(t, content))
	if err == nil || !strings.Contains(err.Error(), "min_context_tokens") {
		t.Fatalf("expected context validation error, got %v", err)
	}
}

func TestLoadRequiresProductionSecrets(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("AUTH_DATABASE_URL", "")
	t.Setenv("REDIS_ADDR", "")
	t.Setenv("REDIS_PASSWORD", "")
	content := strings.Replace(validYAML, "environment: development", "environment: production", 1)
	_, err := Load(writeConfig(t, content))
	if err == nil || !strings.Contains(err.Error(), "production PostgreSQL") {
		t.Fatalf("expected production secret validation error, got %v", err)
	}
}

func TestLoadValidatesCredentials(t *testing.T) {
	tests := []struct {
		name         string
		keyID        string
		secret       string
		previousKeys string
		wantPrevious int
		wantErr      string
	}{
		{
			name:   "previous keys default to empty",
			keyID:  "credentials-test",
			secret: credentialTestSecret,
		},
		{
			name:         "valid previous keys",
			keyID:        "credentials-test",
			secret:       credentialTestSecret,
			previousKeys: "\n    - id: previous\n      secret: 12345678901234567890123456789012",
			wantPrevious: 1,
		},
		{
			name:    "missing active id",
			secret:  credentialTestSecret,
			wantErr: "credentials.active_key_id",
		},
		{
			name:    "blank active id",
			keyID:   "  ",
			secret:  credentialTestSecret,
			wantErr: "credentials.active_key_id",
		},
		{
			name:    "short active secret",
			keyID:   "credentials-test",
			secret:  strings.Repeat("a", 31),
			wantErr: "active_key_secret must be exactly 32 bytes",
		},
		{
			name:    "long active secret",
			keyID:   "credentials-test",
			secret:  strings.Repeat("a", 33),
			wantErr: "active_key_secret must be exactly 32 bytes",
		},
		{
			name:         "missing previous id",
			keyID:        "credentials-test",
			secret:       credentialTestSecret,
			previousKeys: "\n    - secret: 12345678901234567890123456789012",
			wantErr:      "previous_keys[0].id",
		},
		{
			name:         "short previous secret",
			keyID:        "credentials-test",
			secret:       credentialTestSecret,
			previousKeys: "\n    - id: previous\n      secret: 1234567890123456789012345678901",
			wantErr:      "previous_keys[0].secret must be exactly 32 bytes",
		},
		{
			name:         "long previous secret",
			keyID:        "credentials-test",
			secret:       credentialTestSecret,
			previousKeys: "\n    - id: previous\n      secret: 123456789012345678901234567890123",
			wantErr:      "previous_keys[0].secret must be exactly 32 bytes",
		},
		{
			name:         "duplicate previous ids",
			keyID:        "credentials-test",
			secret:       credentialTestSecret,
			previousKeys: "\n    - id: previous\n      secret: 12345678901234567890123456789012\n    - id: previous\n      secret: 01234567890123456789012345678901",
			wantErr:      "not unique",
		},
		{
			name:         "previous key overrides active",
			keyID:        "credentials-test",
			secret:       credentialTestSecret,
			previousKeys: "\n    - id: credentials-test\n      secret: 12345678901234567890123456789012",
			wantErr:      "not unique",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CREDENTIAL_ENCRYPTION_KEY_ID", tt.keyID)
			t.Setenv("CREDENTIAL_ENCRYPTION_KEY_SECRET", tt.secret)
			content := validYAML
			if tt.previousKeys != "" {
				content = strings.Replace(content, "  previous_keys: []", "  previous_keys:"+tt.previousKeys, 1)
			}
			cfg, err := Load(writeConfig(t, content))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Load() error = %v, want error containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if len(cfg.Credentials.PreviousKeys) != tt.wantPrevious {
				t.Fatalf("previous key count = %d, want %d", len(cfg.Credentials.PreviousKeys), tt.wantPrevious)
			}
		})
	}
}

// feishuGroupTail 是基础配置的飞书段，测试替换为启用配置。
const feishuGroupTail = `feishu:
  group:
    require_topic: true
    auto_reply_in_bound_topic: true
    allow_root_reply_fallback: false
`

// feishuEnabledBlock 是启用飞书的完整配置，缺少任一安全字段都必须在启动时失败。
const feishuEnabledBlock = `feishu:
  enabled: true
  app_id: app-id
  app_secret: app-secret
  verification_token: verify-token
  encrypt_keys:
    - id: current
      secret: encrypt-secret
  event_window: 5m
  max_body_bytes: 1048576
  bot_open_id: ou_bot
  default_agent_id: 11111111-1111-1111-1111-111111111111
  default_agent_version: 1
  mode: react
  worker:
    concurrency: 2
    poll_interval: 1s
    lease: 30s
  reply:
    concurrency: 2
    poll_interval: 1s
    lease: 30s
    poll_run_interval: 2s
    run_wait_timeout: 10m
    max_text_bytes: 4096
    card_update_interval: 5s
    max_concurrent_per_chat: 1
    max_concurrent_per_user: 2
  group:
    require_topic: true
    auto_reply_in_bound_topic: true
    allow_root_reply_fallback: false
`

// TestLoadValidatesTracingConfiguration 覆盖追踪启停、端点格式、采样比例与导出超时校验。
func TestLoadValidatesTracingConfiguration(t *testing.T) {
	setCredentialEnvironment(t)
	tracingEnabled := `observability:
  include_execution_payloads: false
  tracing:
    enabled: true
    service_name: harness-agent
    endpoint: http://collector:4318
    sample_ratio: 0.25
    batch_timeout: 5s
    export_timeout: 10s
    headers:
      authorization: "Bearer collector-token"
`
	base := strings.Replace(validYAML, "observability:\n  include_execution_payloads: false", tracingEnabled, 1)
	cfg, err := Load(writeConfig(t, base))
	if err != nil {
		t.Fatalf("Load() tracing config error = %v", err)
	}
	if !cfg.Observability.Tracing.Enabled || cfg.Observability.Tracing.SampleRatio != 0.25 {
		t.Fatalf("tracing config = %+v", cfg.Observability.Tracing)
	}

	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{name: "missing endpoint", content: strings.Replace(base, "    endpoint: http://collector:4318\n", "", 1), wantErr: "endpoint is required"},
		{name: "invalid endpoint scheme", content: strings.Replace(base, "http://collector:4318", "ftp://collector", 1), wantErr: "valid http(s) URL"},
		{name: "zero sample ratio", content: strings.Replace(base, "sample_ratio: 0.25", "sample_ratio: 0", 1), wantErr: "sample_ratio"},
		{name: "ratio above one", content: strings.Replace(base, "sample_ratio: 0.25", "sample_ratio: 2", 1), wantErr: "sample_ratio"},
		{name: "zero export timeout", content: strings.Replace(base, "export_timeout: 10s", "export_timeout: 0s", 1), wantErr: "timeouts must be positive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, tt.content)); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Load() error = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestLoadValidatesFeishuConfiguration 覆盖飞书启用时的凭据、密钥与策略校验。
func TestLoadValidatesFeishuConfiguration(t *testing.T) {
	tests := []struct {
		// name 用例名称。
		name string
		// block 替换后的飞书配置。
		block string
		// wantErr 期望的错误关键字；为空表示应加载成功。
		wantErr string
	}{
		{name: "enabled configuration", block: feishuEnabledBlock},
		{
			name:    "missing app id",
			block:   strings.Replace(feishuEnabledBlock, "  app_id: app-id", "  app_id: \"\"", 1),
			wantErr: "feishu.app_id",
		},
		{
			name:    "missing encrypt key",
			block:   strings.Replace(feishuEnabledBlock, "  encrypt_keys:\n    - id: current\n      secret: encrypt-secret", "  encrypt_keys: []", 1),
			wantErr: "feishu.encrypt_keys",
		},
		{
			name:    "unsupported mode",
			block:   strings.Replace(feishuEnabledBlock, "  mode: react", "  mode: unknown", 1),
			wantErr: "unsupported feishu mode",
		},
		{
			name:    "root fallback with required topic",
			block:   strings.Replace(feishuEnabledBlock, "    allow_root_reply_fallback: false", "    allow_root_reply_fallback: true", 1),
			wantErr: "root reply fallback",
		},
		{
			name:    "missing default agent",
			block:   strings.Replace(feishuEnabledBlock, "  default_agent_id: 11111111-1111-1111-1111-111111111111", "  default_agent_id: \"\"", 1),
			wantErr: "feishu.default_agent_id",
		},
		{
			name:    "missing card update interval",
			block:   strings.Replace(feishuEnabledBlock, "    card_update_interval: 5s", "    card_update_interval: 0s", 1),
			wantErr: "card_update_interval",
		},
		{
			name:    "invalid per user concurrency",
			block:   strings.Replace(feishuEnabledBlock, "    max_concurrent_per_user: 2", "    max_concurrent_per_user: 0", 1),
			wantErr: "concurrency limits",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setCredentialEnvironment(t)
			content := strings.Replace(validYAML, feishuGroupTail, tt.block, 1)
			cfg, err := Load(writeConfig(t, content))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Load() error = %v, want error containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if !cfg.Feishu.Enabled || len(cfg.Feishu.EncryptKeys) != 1 || cfg.Feishu.EventWindow.Duration != 5*time.Minute {
				t.Fatalf("unexpected feishu config %+v", cfg.Feishu)
			}
		})
	}
}
