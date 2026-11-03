#!/usr/bin/env bash
# 备份过期清除脚本：只删除超过保留期且匹配本工具命名模式的加密备份与 Manifest。
# 用法：expire.sh <备份目录> [保留天数]，目录不存在时静默成功，便于幂等清理。
set -euo pipefail

BACKUP_DIR=${1:?backup directory is required}
RETENTION_DAYS=${2:-30}

if [[ ! -d "$BACKUP_DIR" ]]; then
  exit 0
fi
find "$BACKUP_DIR" -maxdepth 1 -type f -name 'harness-*.dump.enc' -mtime "+$RETENTION_DAYS" -delete
find "$BACKUP_DIR" -maxdepth 1 -type f -name 'harness-*.manifest.json' -mtime "+$RETENTION_DAYS" -delete
