package signing_test

import (
	"os"
	"strings"
	"testing"
)

// readFile 读取部署文件，失败时终止测试。
func readFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}

// TestSupplyChainScriptsCoverSBOMProvenanceAndVerification 验证 SBOM、证明、签名与部署校验脚本存在。
func TestSupplyChainScriptsCoverSBOMProvenanceAndVerification(t *testing.T) {
	sbom := readFile(t, "../../scripts/sbom.sh")
	for _, token := range []string{"cyclonedx", "spdx-json", "image.cdx.json"} {
		if !strings.Contains(sbom, token) {
			t.Fatalf("sbom.sh missing %q", token)
		}
	}
	chain := readFile(t, "../../scripts/supply_chain.sh")
	for _, token := range []string{"slsa.dev/provenance/v1", "sign-blob", "verify-blob", "approval.json"} {
		if !strings.Contains(chain, token) {
			t.Fatalf("supply_chain.sh missing %q", token)
		}
	}
	verify := readFile(t, "verify.sh")
	for _, token := range []string{"approval.json", "verify-blob", "digest"} {
		if !strings.Contains(verify, token) {
			t.Fatalf("verify.sh missing %q", token)
		}
	}
}

// TestReleaseWorkflowSignsAndVerifiesImage 验证发布工作流包含扫描、SBOM、keyless 签名与验证。
func TestReleaseWorkflowSignsAndVerifiesImage(t *testing.T) {
	workflow := readFile(t, "../../.github/workflows/release.yml")
	for _, token := range []string{
		"trivy", "--exit-code 1", "cyclonedx", "cosign sign", "cosign attest",
		"cosign verify", "id-token: write", "provenance.json",
	} {
		if !strings.Contains(workflow, token) {
			t.Fatalf("release workflow missing %q", token)
		}
	}
}

// TestReleaseAcceptanceCoversCanaryAndMigrationGuard 验证预发布验收涵盖灰度、签名与迁移守卫。
func TestReleaseAcceptanceCoversCanaryAndMigrationGuard(t *testing.T) {
	acceptance := readFile(t, "../../scripts/acceptance.sh")
	for _, token := range []string{"verify.sh", "TestMigrationsAreForwardCompatible", "canary_smoke.sh", "health/ready"} {
		if !strings.Contains(acceptance, token) {
			t.Fatalf("acceptance.sh missing %q", token)
		}
	}
	canary := readFile(t, "../../scripts/canary_smoke.sh")
	for _, token := range []string{"CANARY_IMAGE", "app-canary", "8083", "stop app-canary"} {
		if !strings.Contains(canary, token) {
			t.Fatalf("canary_smoke.sh missing %q", token)
		}
	}
	overlay := readFile(t, "../compose/canary.yaml")
	for _, token := range []string{"app-canary", "8083", "CANARY_IMAGE"} {
		if !strings.Contains(overlay, token) {
			t.Fatalf("canary overlay missing %q", token)
		}
	}
	doc := readFile(t, "../../docs/runbooks/release-acceptance.md")
	for _, token := range []string{"灰度", "回滚", "禁止执行 `.down.sql`", "队列消息兼容", "在途 Run 恢复"} {
		if !strings.Contains(doc, token) {
			t.Fatalf("release acceptance doc missing %q", token)
		}
	}
}

// TestNetworkAndContainerHardeningDocumented 验证网络分段与容器最小权限配置。
func TestNetworkAndContainerHardeningDocumented(t *testing.T) {
	network := readFile(t, "../network/README.md")
	for _, token := range []string{"443", "5432", "6379", "169.254.169.254", "VPN", "TLS 1.3"} {
		if !strings.Contains(network, token) {
			t.Fatalf("network doc missing %q", token)
		}
	}
	overlay := readFile(t, "../../docker-compose.yml")
	for _, token := range []string{"read_only: true", "cap_drop", "no-new-privileges", "tmpfs"} {
		if !strings.Contains(overlay, token) {
			t.Fatalf("compose hardening missing %q", token)
		}
	}
	tls := readFile(t, "../../configs/config.production.yaml")
	for _, token := range []string{"tls:", "enabled:", "min_version: 1.3"} {
		if !strings.Contains(tls, token) {
			t.Fatalf("production TLS config missing %q", token)
		}
	}
}
