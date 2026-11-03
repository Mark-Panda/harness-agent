#!/usr/bin/env bash
# 生成仓库与容器镜像 SBOM（CycloneDX 与 SPDX JSON）。
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
IMAGE=${1:-harness-agent-app:latest}
OUT_DIR=${SBOM_DIR:-"$ROOT_DIR/artifacts/sbom"}
TRIVY_IMAGE=${TRIVY_IMAGE:-aquasec/trivy:0.67.0}
mkdir -p "$OUT_DIR"

echo "生成仓库 SBOM"
docker run --rm -v "$ROOT_DIR:/src:ro" "$TRIVY_IMAGE" fs --skip-version-check \
  --format cyclonedx /src > "$OUT_DIR/repo.cdx.json"
docker run --rm -v "$ROOT_DIR:/src:ro" "$TRIVY_IMAGE" fs --skip-version-check \
  --format spdx-json /src > "$OUT_DIR/repo.spdx.json"

echo "生成镜像 SBOM：$IMAGE"
docker run --rm -v /var/run/docker.sock:/var/run/docker.sock "$TRIVY_IMAGE" image --skip-version-check \
  --format cyclonedx "$IMAGE" > "$OUT_DIR/image.cdx.json"
docker run --rm -v /var/run/docker.sock:/var/run/docker.sock "$TRIVY_IMAGE" image --skip-version-check \
  --format spdx-json "$IMAGE" > "$OUT_DIR/image.spdx.json"

echo "SBOM 输出：$OUT_DIR"
ls -1 "$OUT_DIR"
