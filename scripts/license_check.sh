#!/usr/bin/env bash
# 依赖许可证检查：离线扫描 Go 模块与前端依赖的许可证声明。
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

python3 scripts/license_check.py "$@"
