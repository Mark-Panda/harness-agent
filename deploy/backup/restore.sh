#!/usr/bin/env bash
# Harness Agent 加密备份恢复脚本。
# 关键步骤：解密 -> 校验哈希 -> 在空库执行 pg_restore -> 校验迁移版本/行数/RLS/业务一致性 -> 输出 RTO。
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_CMD=${COMPOSE_CMD:-"docker compose"}
POSTGRES_SERVICE=${POSTGRES_SERVICE:-postgres}
POSTGRES_USER=${POSTGRES_USER:-harness}
BACKUP_FILE=${BACKUP_FILE:?请设置 BACKUP_FILE 指向 .dump.enc 文件}
MANIFEST_FILE=${MANIFEST_FILE:-"${BACKUP_FILE%.dump.enc}.manifest.json"}
BACKUP_ENCRYPTION_KEY_FILE=${BACKUP_ENCRYPTION_KEY_FILE:-"$ROOT_DIR/backups/backup.key"}
TARGET_DB=${TARGET_DB:-"harness_restore"}
TARGET_DSN=${TARGET_DSN:-"postgres://harness:harness@localhost:5432/${TARGET_DB}?sslmode=disable"}
RECREATE_TARGET=${RECREATE_TARGET:-false}
BACKUP_BIN=${BACKUP_BIN:-"$ROOT_DIR/bin/harness-backup"}

if [[ ! -x "$BACKUP_BIN" ]]; then
  echo "缺少 ${BACKUP_BIN}，请先执行 make build" >&2
  exit 1
fi
if [[ ! -r "$BACKUP_FILE" || ! -r "$MANIFEST_FILE" ]]; then
  echo "备份文件或 Manifest 不存在：$BACKUP_FILE / $MANIFEST_FILE" >&2
  exit 1
fi

WORK_DIR="$(mktemp -d)"
trap 'rm -rf "$WORK_DIR"' EXIT
RAW_DUMP="$WORK_DIR/restore.dump"
STARTED_AT=$(date +%s)

echo "解密备份到临时目录"
"$BACKUP_BIN" decrypt -in "$BACKUP_FILE" -out "$RAW_DUMP" -key-file "$BACKUP_ENCRYPTION_KEY_FILE" > "$WORK_DIR/decrypt.json"
PLAINTEXT_HASH=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["plaintext_sha256"])' "$WORK_DIR/decrypt.json")
EXPECTED_HASH=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["plaintext_sha256"])' "$MANIFEST_FILE")
if [[ "$PLAINTEXT_HASH" != "$EXPECTED_HASH" ]]; then
  echo "备份哈希不匹配：$PLAINTEXT_HASH != $EXPECTED_HASH" >&2
  exit 1
fi

EXISTS=$($COMPOSE_CMD exec -T "$POSTGRES_SERVICE" psql -U "$POSTGRES_USER" -d postgres -tAc \
  "SELECT 1 FROM pg_database WHERE datname='$TARGET_DB'")
if [[ -n "$EXISTS" ]]; then
  if [[ "$RECREATE_TARGET" != "true" ]]; then
    echo "目标库 $TARGET_DB 已存在；如需重建请设置 RECREATE_TARGET=true" >&2
    exit 1
  fi
  echo "重建目标库 $TARGET_DB"
  $COMPOSE_CMD exec -T "$POSTGRES_SERVICE" psql -U "$POSTGRES_USER" -d postgres -v ON_ERROR_STOP=1 -c \
    "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname='$TARGET_DB' AND pid <> pg_backend_pid()" > /dev/null
  $COMPOSE_CMD exec -T "$POSTGRES_SERVICE" psql -U "$POSTGRES_USER" -d postgres -v ON_ERROR_STOP=1 -c \
    "DROP DATABASE \"$TARGET_DB\"" > /dev/null
fi
$COMPOSE_CMD exec -T "$POSTGRES_SERVICE" psql -U "$POSTGRES_USER" -d postgres -v ON_ERROR_STOP=1 -c \
  "CREATE DATABASE \"$TARGET_DB\"" > /dev/null

echo "恢复数据到 $TARGET_DB"
$COMPOSE_CMD exec -T "$POSTGRES_SERVICE" pg_restore -U "$POSTGRES_USER" -d "$TARGET_DB" --no-owner < "$RAW_DUMP"

echo "校验迁移版本、行数、RLS 与业务一致性"
"$BACKUP_BIN" verify -dsn "$TARGET_DSN" -manifest "$MANIFEST_FILE" \
  -plaintext "$RAW_DUMP" -ciphertext "$BACKUP_FILE"

ELAPSED=$(( $(date +%s) - STARTED_AT ))
echo "恢复完成：${TARGET_DB}；RTO=${ELAPSED}s"
