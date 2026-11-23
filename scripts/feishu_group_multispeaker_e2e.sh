#!/usr/bin/env bash
# 飞书群聊多发言人链路自动化测试。
# 前置条件：独立 PostgreSQL 空库已执行迁移，并通过 TEST_*_DATABASE_URL 指向该库。
# 不要在本地真实开发库上运行：测试会创建并清理用户、会话、Run 与事件回执。
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

for name in TEST_ADMIN_DATABASE_URL TEST_APP_DATABASE_URL TEST_AUTH_DATABASE_URL; do
  if [[ -z "${!name:-}" ]]; then
    echo "缺少环境变量 $name；请指向已迁移的独立测试库。" >&2
    exit 1
  fi
done

REQUIRE_DATABASE=true go test -tags=integration \
  ./internal/data \
  -run TestFeishuGroupMultiSpeakerEndToEnd \
  -count=1 -v
