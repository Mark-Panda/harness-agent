#!/usr/bin/env bash
# Harness Agent 安全扫描入口：SAST、依赖/密钥/IaC 扫描、API Fuzz 与模块完整性校验。
# 需要本机安装 staticcheck/gosec/govulncheck；Trivy 通过 Docker 运行。
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"
FUZZ_TIME=${FUZZ_TIME:-10s}
TRIVY_IMAGE=${TRIVY_IMAGE:-aquasec/trivy:0.67.0}
SECURITY_FAIL_ON_HIGH=${SECURITY_FAIL_ON_HIGH:-true}

echo "== gofmt =="
UNFORMATTED=$(gofmt -l api cmd internal pkg)
if [[ -n "$UNFORMATTED" ]]; then
  echo "以下文件未格式化：$UNFORMATTED" >&2
  exit 1
fi

echo "== go vet =="
go vet ./...

echo "== staticcheck =="
if command -v staticcheck >/dev/null 2>&1; then
  staticcheck ./...
else
  echo "staticcheck 未安装，跳过" >&2
fi

echo "== go mod verify =="
go mod verify

echo "== gosec =="
if command -v gosec >/dev/null 2>&1; then
  gosec -exclude-generated -severity high -confidence high ./...
else
  echo "gosec 未安装，跳过" >&2
fi

echo "== govulncheck =="
if command -v govulncheck >/dev/null 2>&1; then
  govulncheck ./...
else
  echo "govulncheck 未安装，跳过；CI 中必须安装并在发布阻断" >&2
fi

echo "== Trivy 文件系统（依赖/密钥/IaC）=="
if command -v docker >/dev/null 2>&1; then
  SCAN_ARGS=(fs --scanners vuln,secret,misconfig --skip-version-check --format table --skip-dirs artifacts --skip-dirs backups)
  if [[ "$SECURITY_FAIL_ON_HIGH" == "true" ]]; then
    SCAN_ARGS+=(--exit-code 1 --severity HIGH,CRITICAL)
  fi
  docker run --rm -v "$ROOT_DIR:/src:ro" "$TRIVY_IMAGE" "${SCAN_ARGS[@]}" /src
else
  echo "docker 不可用，跳过 Trivy 文件系统扫描" >&2
fi

echo "== Trivy 容器镜像 =="
if [[ -n "${SECURITY_IMAGE:-}" ]]; then
  IMAGE_ARGS=(image --scanners vuln,secret,misconfig --skip-version-check --format table)
  if [[ "$SECURITY_FAIL_ON_HIGH" == "true" ]]; then
    IMAGE_ARGS+=(--exit-code 1 --severity HIGH,CRITICAL)
  fi
  docker run --rm -v /var/run/docker.sock:/var/run/docker.sock "$TRIVY_IMAGE" "${IMAGE_ARGS[@]}" "$SECURITY_IMAGE"
else
  echo "SECURITY_IMAGE 未设置，跳过容器镜像扫描" >&2
fi

echo "== API Fuzz =="
go test -run '^$' -fuzz=FuzzDecodeJSON -fuzztime="$FUZZ_TIME" ./internal/server
go test -run '^$' -fuzz=FuzzWriteSSE -fuzztime="$FUZZ_TIME" ./internal/server
go test -run '^$' -fuzz=FuzzDecryptPayload -fuzztime="$FUZZ_TIME" ./pkg/feishu
go test -run '^$' -fuzz=FuzzDecodeEnvelope -fuzztime="$FUZZ_TIME" ./pkg/feishu

echo "安全扫描完成"
