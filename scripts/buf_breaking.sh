#!/usr/bin/env bash
# Protobuf 兼容性检查：以 PR 目标分支或上一提交为基线执行 buf breaking。
# 关键步骤：确定基线 ref → 计算 merge-base → 使用该提交快照对比当前 api/ 定义。
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

BASE_REF="${BUF_BREAKING_BASE_REF:-}"
if [[ -z "$BASE_REF" && "${GITHUB_EVENT_NAME:-}" == "pull_request" ]]; then
  : "${GITHUB_BASE_REF:?pull_request event requires GITHUB_BASE_REF}"
  BASE_REF="origin/${GITHUB_BASE_REF}"
fi
if [[ -z "$BASE_REF" && "${GITHUB_EVENT_NAME:-}" == "push" ]]; then
  # 推送构建没有目标分支，退化为与上一提交比较。
  if git rev-parse --verify -q HEAD~1 >/dev/null; then
    BASE_REF="HEAD~1"
  fi
fi
if [[ -z "$BASE_REF" ]]; then
  for candidate in origin/main main; do
    if git rev-parse --verify -q "$candidate" >/dev/null; then
      BASE_REF="$candidate"
      break
    fi
  done
fi
if [[ -z "$BASE_REF" ]] || ! git rev-parse --verify -q "$BASE_REF" >/dev/null; then
  # PR 场景缺少目标分支属于 CI 配置错误，必须失败而不是给出虚假通过。
  if [[ "${GITHUB_EVENT_NAME:-}" == "pull_request" ]]; then
    echo "buf breaking: base ref ${BASE_REF:-<empty>} is unavailable" >&2
    exit 1
  fi
  echo "buf breaking: no baseline ref available, skipping" >&2
  exit 0
fi

MERGE_BASE=$(git merge-base HEAD "$BASE_REF")
if [[ -z "$MERGE_BASE" ]]; then
  echo "buf breaking: cannot compute merge-base with $BASE_REF" >&2
  exit 1
fi
# 基线尚无 protobuf 时视为首次引入 API，兼容性检查没有可比较对象。
if ! git ls-tree -r --name-only "$MERGE_BASE" -- api | grep -q '\.proto$'; then
  echo "buf breaking: baseline $MERGE_BASE has no protobuf files, skipping"
  exit 0
fi
echo "buf breaking against $BASE_REF ($MERGE_BASE)"
buf breaking --against ".git#ref=${MERGE_BASE}"
