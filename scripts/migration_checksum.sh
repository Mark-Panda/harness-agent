#!/usr/bin/env bash
# 重新生成已发布迁移的校验清单；仅在明确批准的历史迁移修复后执行。
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

python3 - <<'PY'
import hashlib
import os

directory = os.path.join(os.getcwd(), "migrations")
entries = []
for name in sorted(os.listdir(directory)):
    if not name.endswith(".sql"):
        continue
    path = os.path.join(directory, name)
    digest = hashlib.sha256(open(path, "rb").read()).hexdigest()
    entries.append(f"{digest}  {name}")
with open(os.path.join(directory, "checksums.txt"), "w", encoding="utf-8") as handle:
    handle.write("# 已发布迁移的 SHA-256 校验清单；新增迁移后运行 scripts/migration_checksum.sh 更新。\n")
    handle.write("\n".join(entries) + "\n")
print(f"wrote {len(entries)} checksums")
PY
