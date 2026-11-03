package monitoring_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// alertsFile 是 Prometheus 规则文件的测试视图。
type alertsFile struct {
	Groups []struct {
		Rules []struct {
			Alert       string            `yaml:"alert"`
			Record      string            `yaml:"record"`
			Expr        string            `yaml:"expr"`
			Labels      map[string]string `yaml:"labels"`
			Annotations map[string]string `yaml:"annotations"`
		} `yaml:"rules"`
	} `yaml:"groups"`
}

// alertTestsFile 是 promtool 告警触发测试文件的测试视图。
type alertTestsFile struct {
	Tests []struct {
		AlertRuleTests []struct {
			Alertname string `yaml:"alertname"`
		} `yaml:"alert_rule_test"`
	} `yaml:"tests"`
}

// TestCriticalAlertsHaveTriggerTestsAndRunbooks 验证每个 Critical/High 告警都有触发测试与 Runbook 锚点。
func TestCriticalAlertsHaveTriggerTestsAndRunbooks(t *testing.T) {
	alerts := loadAlerts(t)
	tests := loadAlertTests(t)
	runbook := readFile(t, "../../docs/runbooks/harness-agent-alerts.md")
	tested := make(map[string]bool, len(tests.Tests))
	for _, test := range tests.Tests {
		for _, alertTest := range test.AlertRuleTests {
			tested[alertTest.Alertname] = true
		}
	}
	criticalOrHigh := 0
	for _, group := range alerts.Groups {
		for _, rule := range group.Rules {
			if rule.Alert == "" {
				if rule.Record == "" || strings.TrimSpace(rule.Expr) == "" {
					t.Fatalf("recording rule is incomplete: %+v", rule)
				}
				continue
			}
			if strings.TrimSpace(rule.Expr) == "" {
				t.Fatalf("alert %s has empty expression", rule.Alert)
			}
			severity := strings.ToLower(rule.Labels["severity"])
			if severity != "critical" && severity != "high" {
				continue
			}
			criticalOrHigh++
			runbookURL := rule.Annotations["runbook_url"]
			if runbookURL == "" {
				t.Fatalf("alert %s is missing runbook_url", rule.Alert)
			}
			anchor := "#" + strings.ToLower(rule.Alert)
			if !strings.Contains(strings.ToLower(runbookURL), anchor) {
				t.Fatalf("alert %s runbook anchor is not canonical: %s", rule.Alert, runbookURL)
			}
			if !strings.Contains(strings.ToLower(runbook), strings.ToLower(rule.Alert)) {
				t.Fatalf("runbook missing section for %s", rule.Alert)
			}
			if !tested[rule.Alert] {
				t.Fatalf("alert %s has no promtool trigger test", rule.Alert)
			}
		}
	}
	if criticalOrHigh == 0 {
		t.Fatal("no critical or high alerts configured")
	}
}

// TestDashboardAndSyntheticConfigReferenceExpectedMetrics 验证看板与合成监控配置覆盖关键指标。
func TestDashboardAndSyntheticConfigReferenceExpectedMetrics(t *testing.T) {
	var dashboard struct {
		Panels []struct {
			Targets []struct {
				Expr string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal([]byte(readFile(t, "../grafana/dashboards/harness-agent-slo.json")), &dashboard); err != nil {
		t.Fatalf("decode dashboard: %v", err)
	}
	if len(dashboard.Panels) == 0 {
		t.Fatal("dashboard has no panels")
	}
	var expressions strings.Builder
	for _, panel := range dashboard.Panels {
		for _, target := range panel.Targets {
			expressions.WriteString(target.Expr)
			expressions.WriteString("\n")
		}
	}
	// 看板必须覆盖 API RED、Run、Token/费用、队列、Outbox、SSE 与记忆。
	for _, metric := range []string{
		"harness_api_requests_total", "harness_api_request_duration_seconds_bucket",
		"harness_agent_runs_total", "harness_agent_llm_tokens_total", "harness_agent_llm_cost_total",
		"harness_agent_jobs_inflight", "harness_agent_outbox_events_total",
		"harness_agent_sse_connections", "harness_agent_memory_operations_total", "harness_agent_tool_calls_total",
		"harness_db_pool_in_use_connections",
	} {
		if !strings.Contains(expressions.String(), metric) {
			t.Fatalf("dashboard missing metric %s", metric)
		}
	}
	prometheus := readFile(t, "../prometheus/prometheus.yaml")
	for _, token := range []string{"alerts.yaml", "blackbox:9115", "health/ready"} {
		if !strings.Contains(prometheus, token) {
			t.Fatalf("synthetic scrape config missing %s", token)
		}
	}
	blackbox := readFile(t, "../prometheus/blackbox.yaml")
	if !strings.Contains(blackbox, "http_2xx") {
		t.Fatal("blackbox module missing")
	}
}

// TestFailureDegradationRunbookCoversScenarios 验证故障降级 Runbook 覆盖依赖故障、慢调用与验收命令。
func TestFailureDegradationRunbookCoversScenarios(t *testing.T) {
	content := readFile(t, "../../docs/runbooks/failure-degradation.md")
	for _, token := range []string{
		"PostgreSQL", "Redis", "连接池耗尽", "LLM", "MCP", "飞书",
		"HarnessDBPoolSaturation", "Retry-After", "dependency_timeout",
		"TestConnectionPoolExhaustionTimeoutAndRecovery", "TestHotMessageStoreRecoversAfterRedisRestart",
		"TestRouterSlowProviderTimesOutAndSwitches", "TestPrepareMCPTools", "TestClientSlowUpstreamFailsFast",
	} {
		if !strings.Contains(content, token) {
			t.Fatalf("failure degradation runbook missing %q", token)
		}
	}
}

// TestCapacityBaselineDocumentsTargetsAndThresholds 验证容量基线包含峰值、1.5 倍与扩容阈值。
func TestCapacityBaselineDocumentsTargetsAndThresholds(t *testing.T) {
	content := readFile(t, "../../docs/capacity/README.md")
	for _, token := range []string{
		"100 RPS", "150 RPS", "200 RPS", "P95", "HarnessQueueBacklog",
		"扩容阈值", "慢消费者", "工具风暴", "多用户公平性", "资源规格建议",
	} {
		if !strings.Contains(content, token) {
			t.Fatalf("capacity baseline missing %q", token)
		}
	}
}

// TestSecurityReviewDocumentsScansAndPermissionMatrix 验证安全报告与权限矩阵覆盖关键要求。
func TestSecurityReviewDocumentsScansAndPermissionMatrix(t *testing.T) {
	review := readFile(t, "../../docs/security/security-review.md")
	for _, token := range []string{
		"gosec", "staticcheck", "govulncheck", "Trivy", "Fuzz",
		"HIGH/CRITICAL = 0", "跨用户泄露不豁免", "SEC-02",
	} {
		if !strings.Contains(review, token) {
			t.Fatalf("security review missing %q", token)
		}
	}
	matrix := readFile(t, "../../docs/security/permission-matrix.md")
	for _, token := range []string{
		"/api/v1/admin/*", "拒绝", "403", "404", "Break-Glass", "RLS",
	} {
		if !strings.Contains(matrix, token) {
			t.Fatalf("permission matrix missing %q", token)
		}
	}
}

// TestSandboxBaselineRequiresIsolation 验证 Sandbox 部署基线包含全部强制隔离参数。
func TestSandboxBaselineRequiresIsolation(t *testing.T) {
	baseline := readFile(t, "../sandbox/README.md")
	for _, token := range []string{
		"--user 65534:65534", "--read-only", "--cap-drop ALL", "no-new-privileges",
		"seccomp=deploy/sandbox/seccomp.json", "--pids-limit", "--memory", "--cpus", "--network none",
	} {
		if !strings.Contains(baseline, token) {
			t.Fatalf("sandbox baseline missing %q", token)
		}
	}
	// 禁止挂载 Docker Socket 或共享宿主敏感目录。
	for _, forbidden := range []string{"/var/run/docker.sock", "--privileged"} {
		if strings.Contains(baseline, forbidden) {
			t.Fatalf("sandbox baseline contains forbidden %q", forbidden)
		}
	}
	var profile struct {
		DefaultAction string `json:"defaultAction"`
		Syscalls      []struct {
			Names  []string `json:"names"`
			Action string   `json:"action"`
		} `json:"syscalls"`
	}
	if err := json.Unmarshal([]byte(readFile(t, "../sandbox/seccomp.json")), &profile); err != nil {
		t.Fatalf("decode seccomp profile: %v", err)
	}
	if profile.DefaultAction != "SCMP_ACT_ERRNO" || len(profile.Syscalls) < 2 {
		t.Fatalf("seccomp profile is not deny-by-default: %+v", profile)
	}
	networkDenied := false
	for _, rule := range profile.Syscalls {
		if rule.Action == "SCMP_ACT_ERRNO" && len(rule.Names) > 0 && rule.Names[0] == "socket" {
			networkDenied = true
		}
	}
	if !networkDenied {
		t.Fatal("seccomp profile must deny network syscalls by default")
	}
}

// loadAlerts 读取 Prometheus 告警规则。
func loadAlerts(t *testing.T) alertsFile {
	t.Helper()
	var parsed alertsFile
	if err := yaml.Unmarshal([]byte(readFile(t, "../prometheus/alerts.yaml")), &parsed); err != nil {
		t.Fatalf("decode alerts: %v", err)
	}
	return parsed
}

// loadAlertTests 读取 promtool 触发测试。
func loadAlertTests(t *testing.T) alertTestsFile {
	t.Helper()
	var parsed alertTestsFile
	if err := yaml.Unmarshal([]byte(readFile(t, "../prometheus/alerts_test.yaml")), &parsed); err != nil {
		t.Fatalf("decode alert tests: %v", err)
	}
	return parsed
}

// readFile 读取相对路径文件，失败时终止测试。
func readFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}
