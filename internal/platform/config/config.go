package config

import (
	"bytes"
	"fmt"
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
	Audit         AuditConfig         `yaml:"audit"`
	Feishu        FeishuConfig        `yaml:"feishu"`
}

type ServerConfig struct {
	HTTP            HTTPServerConfig `yaml:"http"`
	ShutdownTimeout Duration         `yaml:"shutdown_timeout"`
}

type HTTPServerConfig struct {
	Address string   `yaml:"addr"`
	Timeout Duration `yaml:"timeout"`
}

type DataConfig struct {
	Postgres PostgresConfig `yaml:"postgres"`
	Redis    RedisConfig    `yaml:"redis"`
}

type PostgresConfig struct {
	DSN          string `yaml:"dsn"`
	MaxOpenConns int    `yaml:"max_open_conns"`
}

type RedisConfig struct {
	Address  string `yaml:"addr"`
	Password string `yaml:"password"`
}

type AgentConfig struct {
	ModelRouting     ModelRoutingConfig     `yaml:"model_routing"`
	ExecutionRecords ExecutionRecordsConfig `yaml:"execution_records"`
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
	IncludeExecutionPayloads bool `yaml:"include_execution_payloads"`
}

type AuditConfig struct {
	SynchronousCriticalWrites bool `yaml:"synchronous_critical_writes"`
	HashChainEnabled          bool `yaml:"hash_chain_enabled"`
}

type FeishuConfig struct {
	Group FeishuGroupConfig `yaml:"group"`
}

type FeishuGroupConfig struct {
	RequireTopic           bool `yaml:"require_topic"`
	AutoReplyInBoundTopic  bool `yaml:"auto_reply_in_bound_topic"`
	AllowRootReplyFallback bool `yaml:"allow_root_reply_fallback"`
}

func Load(basePath string, overlayPaths ...string) (*Config, error) {
	var cfg Config
	for _, path := range append([]string{basePath}, overlayPaths...) {
		raw, err := os.ReadFile(path)
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
	if c.Server.HTTP.Timeout.Duration <= 0 || c.Server.ShutdownTimeout.Duration <= 0 {
		return fmt.Errorf("server timeouts must be positive")
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
	if c.Feishu.Group.RequireTopic && c.Feishu.Group.AllowRootReplyFallback {
		return fmt.Errorf("feishu root reply fallback cannot be enabled when topics are required")
	}
	if c.Environment == "production" {
		if strings.TrimSpace(c.Data.Postgres.DSN) == "" || strings.TrimSpace(c.Data.Redis.Address) == "" || c.Data.Redis.Password == "" {
			return fmt.Errorf("production PostgreSQL and Redis secrets must be injected")
		}
		if !c.Audit.SynchronousCriticalWrites || !c.Audit.HashChainEnabled {
			return fmt.Errorf("production synchronous audit writes and hash chain are required")
		}
	}
	return nil
}
