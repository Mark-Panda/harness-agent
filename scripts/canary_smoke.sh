#!/usr/bin/env bash
# 灰度 Smoke：用已批准镜像启动独立副本，验证核心探针后保留等待切流。
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"
CANARY_IMAGE=${CANARY_IMAGE:?请设置 CANARY_IMAGE 为已批准镜像}
COMPOSE_CMD=${COMPOSE_CMD:-"docker compose"}
CANARY_URL=${CANARY_URL:-http://localhost:8083}

$COMPOSE_CMD -f docker-compose.yml -f deploy/compose/canary.yaml up -d app-canary
for attempt in $(seq 1 60); do
  if curl -fsS "$CANARY_URL/health/ready" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

if ! curl -fsS "$CANARY_URL/health/ready" | grep -q '"status":"ready"'; then
  echo "灰度实例未就绪，停止并回滚" >&2
  $COMPOSE_CMD -f docker-compose.yml -f deploy/compose/canary.yaml stop app-canary
  exit 1
fi
curl -fsS "$CANARY_URL/api/v1/health" >/dev/null
curl -fsS "$CANARY_URL/api/v1/sse/probe" | grep -q "event: heartbeat"
echo "灰度 Smoke 通过：${CANARY_URL}（镜像 ${CANARY_IMAGE}）"
