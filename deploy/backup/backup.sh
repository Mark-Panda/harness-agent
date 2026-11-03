#!/usr/bin/env bash
# Harness Agent 加密逻辑备份脚本。
# 关键步骤：pg_dump 自定义格式 -> AES-256-GCM 加密 -> 采集恢复验收基线 -> 写入 Manifest -> 跨故障域镜像。
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_CMD=${COMPOSE_CMD:-"docker compose"}
POSTGRES_SERVICE=${POSTGRES_SERVICE:-postgres}
DATABASE=${DATABASE:-harness}
POSTGRES_USER=${POSTGRES_USER:-harness}
BACKUP_DIR=${BACKUP_DIR:-"$ROOT_DIR/backups"}
BACKUP_DATABASE_URL=${BACKUP_DATABASE_URL:-"postgres://harness:harness@localhost:5432/harness?sslmode=disable"}
BACKUP_ENCRYPTION_KEY_FILE=${BACKUP_ENCRYPTION_KEY_FILE:-"$ROOT_DIR/backups/backup.key"}
BACKUP_KEY_ID=${BACKUP_KEY_ID:-"backup-$(date -u +%Y%m)"}
BACKUP_MIRROR_DIR=${BACKUP_MIRROR_DIR:-}
BACKUP_RETENTION_DAYS=${BACKUP_RETENTION_DAYS:-30}
BACKUP_BIN=${BACKUP_BIN:-"$ROOT_DIR/bin/harness-backup"}

if [[ ! -x "$BACKUP_BIN" ]]; then
  echo "缺少 ${BACKUP_BIN}，请先执行 make build" >&2
  exit 1
fi
if [[ ! -r "$BACKUP_ENCRYPTION_KEY_FILE" ]]; then
  echo "缺少备份密钥文件 ${BACKUP_ENCRYPTION_KEY_FILE}；可用 openssl rand -base64 32 > \"$BACKUP_ENCRYPTION_KEY_FILE\" 生成" >&2
  exit 1
fi
mkdir -p "$BACKUP_DIR"
WORK_DIR="$(mktemp -d)"
trap 'rm -rf "$WORK_DIR"' EXIT

STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
RAW_DUMP="$WORK_DIR/harness-$STAMP.dump"
ENCRYPTED="$BACKUP_DIR/harness-$STAMP.dump.enc"
MANIFEST="$BACKUP_DIR/harness-$STAMP.manifest.json"
STARTED_AT=$(date +%s)

echo "开始备份 ${DATABASE} (${STAMP})"
$COMPOSE_CMD exec -T "$POSTGRES_SERVICE" pg_dump -U "$POSTGRES_USER" -d "$DATABASE" -Fc --no-owner > "$RAW_DUMP"
"$BACKUP_BIN" stats -dsn "$BACKUP_DATABASE_URL" > "$WORK_DIR/stats.json"
"$BACKUP_BIN" encrypt -in "$RAW_DUMP" -out "$ENCRYPTED" \
  -key-file "$BACKUP_ENCRYPTION_KEY_FILE" -key-id "$BACKUP_KEY_ID" > "$WORK_DIR/encryption.json"

python3 - "$WORK_DIR/stats.json" "$WORK_DIR/encryption.json" "$MANIFEST" "$DATABASE" "$BACKUP_KEY_ID" <<'PY'
import json, sys, datetime
stats = json.load(open(sys.argv[1]))
enc = json.load(open(sys.argv[2]))
manifest = {
    "version": 1,
    "created_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
    "database": sys.argv[4],
    "key_id": sys.argv[5],
    "algorithm": enc["algorithm"],
    "plaintext_sha256": enc["plaintext_sha256"],
    "ciphertext_sha256": enc["ciphertext_sha256"],
    "plaintext_bytes": enc["plaintext_bytes"],
    "ciphertext_bytes": enc["ciphertext_bytes"],
    "stats": stats,
}
json.dump(manifest, open(sys.argv[3], "w"), indent=2, ensure_ascii=False)
PY

if [[ -n "$BACKUP_MIRROR_DIR" ]]; then
  mkdir -p "$BACKUP_MIRROR_DIR"
  cp "$ENCRYPTED" "$MANIFEST" "$BACKUP_MIRROR_DIR/"
  # 跨故障域副本必须与主副本哈希一致，避免静默损坏。
  cmp "$ENCRYPTED" "$BACKUP_MIRROR_DIR/$(basename "$ENCRYPTED")"
  echo "已镜像到 $BACKUP_MIRROR_DIR"
fi

# 保留期：只清理本工具生成且超过保留期的加密备份与 Manifest。
"$ROOT_DIR/deploy/backup/expire.sh" "$BACKUP_DIR" "$BACKUP_RETENTION_DAYS"
if [[ -n "$BACKUP_MIRROR_DIR" && -d "$BACKUP_MIRROR_DIR" ]]; then
  "$ROOT_DIR/deploy/backup/expire.sh" "$BACKUP_MIRROR_DIR" "$BACKUP_RETENTION_DAYS"
fi

ELAPSED=$(( $(date +%s) - STARTED_AT ))
echo "备份完成：$ENCRYPTED"
echo "Manifest：$MANIFEST"
echo "耗时：${ELAPSED}s；保留期：${BACKUP_RETENTION_DAYS} 天"
