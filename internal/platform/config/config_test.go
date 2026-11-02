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
  shutdown_timeout: 10s
data:
  postgres:
    dsn: ${DATABASE_URL}
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
observability:
  include_execution_payloads: false
audit:
  synchronous_critical_writes: true
  hash_chain_enabled: true
feishu:
  group:
    require_topic: true
    auto_reply_in_bound_topic: true
    allow_root_reply_fallback: false
`

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadExpandsEnvironment(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/harness")
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
}

func TestLoadOverlayPreservesInheritedValues(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/harness")
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
	t.Setenv("REDIS_ADDR", "")
	t.Setenv("REDIS_PASSWORD", "")
	content := strings.Replace(validYAML, "environment: development", "environment: production", 1)
	_, err := Load(writeConfig(t, content))
	if err == nil || !strings.Contains(err.Error(), "production PostgreSQL") {
		t.Fatalf("expected production secret validation error, got %v", err)
	}
}
