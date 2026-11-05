#!/usr/bin/env bash
# 不可变制品预发布验收：验签审批、迁移兼容与主实例健康检查。
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"
APPROVAL_FILE=${APPROVAL_FILE:?请设置 APPROVAL_FILE}
APPROVED_IMAGE=${APPROVED_IMAGE:?请设置 APPROVED_IMAGE}
IMAGE_ID=${IMAGE_ID:-$(docker inspect --format '{{.Id}}' "$APPROVED_IMAGE")}

echo "== 1. 校验审批与签名 =="
deploy/signing/verify.sh "$APPROVAL_FILE" "$IMAGE_ID"

echo "== 2. 迁移前向兼容检查 =="
go test -count=1 ./internal/data -run TestMigrationsAreForwardCompatible

echo "== 3. 主实例健康检查 =="
curl -fsS http://localhost:8080/health/ready | grep -q '"status":"ready"'
curl -fsS http://localhost:8080/api/v1/health >/dev/null

echo "== 4. 验收通过 =="
echo "切流：确认新镜像通过验收后再替换主实例；失败时保留旧实例。"
