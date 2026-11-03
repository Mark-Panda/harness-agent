#!/usr/bin/env bash
# Harness Agent PITR 基础备份脚本。
# 前提：PostgreSQL 已使用 deploy/compose/pitr.yaml 启动，WAL 归档目录持续写入。
# 关键步骤：pg_basebackup -> 记录起止 LSN -> 加密基础备份与 WAL 包 -> 写 Manifest。
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_CMD=${COMPOSE_CMD:-"docker compose"}
COMPOSE_FILES=${COMPOSE_FILES:-"-f docker-compose.yml -f deploy/compose/pitr.yaml"}
POSTGRES_SERVICE=${POSTGRES_SERVICE:-postgres}
POSTGRES_USER=${POSTGRES_USER:-harness}
BACKUP_DIR=${BACKUP_DIR:-"$ROOT_DIR/backups/pitr"}
BACKUP_ENCRYPTION_KEY_FILE=${BACKUP_ENCRYPTION_KEY_FILE:-"$ROOT_DIR/backups/backup.key"}
BACKUP_KEY_ID=${BACKUP_KEY_ID:-"pitr-$(date -u +%Y%m)"}
BACKUP_BIN=${BACKUP_BIN:-"$ROOT_DIR/bin/harness-backup"}

if [[ ! -x "$BACKUP_BIN" ]]; then
  echo "缺少 ${BACKUP_BIN}，请先执行 make build" >&2
  exit 1
fi
mkdir -p "$BACKUP_DIR"
WORK_DIR="$(mktemp -d)"
trap 'rm -rf "$WORK_DIR"' EXIT

STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
CONTAINER_DIR="/tmp/harness-base-$STAMP"
START_LSN=$(docker compose $COMPOSE_FILES exec -T "$POSTGRES_SERVICE" psql -U "$POSTGRES_USER" -d postgres -tAc "SELECT pg_current_wal_lsn()")
TIMELINE=$(docker compose $COMPOSE_FILES exec -T "$POSTGRES_SERVICE" psql -U "$POSTGRES_USER" -d postgres -tAc "SELECT timeline_id FROM pg_control_checkpoint()")

echo "开始基础备份 ${STAMP}（start_lsn=${START_LSN}）"
docker compose $COMPOSE_FILES exec -T "$POSTGRES_SERVICE" \
  pg_basebackup -U "$POSTGRES_USER" -D "$CONTAINER_DIR" -Ft -z -X stream -c fast
docker compose $COMPOSE_FILES cp "$POSTGRES_SERVICE:$CONTAINER_DIR/base.tar.gz" "$WORK_DIR/base.tar.gz"
docker compose $COMPOSE_FILES cp "$POSTGRES_SERVICE:$CONTAINER_DIR/pg_wal.tar.gz" "$WORK_DIR/pg_wal.tar.gz"
END_LSN=$(docker compose $COMPOSE_FILES exec -T "$POSTGRES_SERVICE" psql -U "$POSTGRES_USER" -d postgres -tAc "SELECT pg_current_wal_lsn()")
docker compose $COMPOSE_FILES exec -T "$POSTGRES_SERVICE" rm -rf "$CONTAINER_DIR"

"$BACKUP_BIN" encrypt -in "$WORK_DIR/base.tar.gz" -out "$BACKUP_DIR/base-$STAMP.tar.gz.enc" \
  -key-file "$BACKUP_ENCRYPTION_KEY_FILE" -key-id "$BACKUP_KEY_ID" > "$WORK_DIR/base.json"
"$BACKUP_BIN" encrypt -in "$WORK_DIR/pg_wal.tar.gz" -out "$BACKUP_DIR/pg_wal-$STAMP.tar.gz.enc" \
  -key-file "$BACKUP_ENCRYPTION_KEY_FILE" -key-id "$BACKUP_KEY_ID" > "$WORK_DIR/wal.json"

python3 - "$WORK_DIR/base.json" "$WORK_DIR/wal.json" "$BACKUP_DIR/base-$STAMP.manifest.json" \
  "$START_LSN" "${END_LSN}" "$TIMELINE" "$BACKUP_KEY_ID" <<'PY'
import json, sys, datetime
base = json.load(open(sys.argv[1]))
wal = json.load(open(sys.argv[2]))
manifest = {
    "version": 1, "type": "basebackup",
    "created_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
    "start_lsn": sys.argv[4], "end_lsn": sys.argv[5], "timeline": int(sys.argv[6]),
    "key_id": sys.argv[7], "algorithm": base["algorithm"],
    "wal_archive": "pg_wal_archive",
    "files": {"base.tar.gz": base, "pg_wal.tar.gz": wal},
}
json.dump(manifest, open(sys.argv[3], "w"), indent=2, ensure_ascii=False)
PY

echo "基础备份完成：$BACKUP_DIR/base-$STAMP.tar.gz.enc"
echo "Manifest：$BACKUP_DIR/base-$STAMP.manifest.json"
echo "PITR 起止 LSN：$START_LSN -> ${END_LSN}"
