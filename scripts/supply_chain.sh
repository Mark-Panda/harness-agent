#!/usr/bin/env bash
# 供应链证据：生成行为证明、使用 cosign 签名并验证，输出部署审批文件。
# 说明：本地镜像无 registry digest 时使用 image ID 作为 subject；CI 中应使用推送后的 manifest digest。
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"
IMAGE=${1:-harness-agent-app:latest}
OUT_DIR=${SUPPLY_DIR:-"$ROOT_DIR/artifacts/signing"}
COSIGN_IMAGE=${COSIGN_IMAGE:-gcr.io/projectsigstore/cosign:v2.4.3}
COSIGN_PASSWORD=${COSIGN_PASSWORD:-local-test-signing-only}
mkdir -p "$OUT_DIR"

IMAGE_ID=$(docker inspect --format '{{.Id}}' "$IMAGE")
if [[ -z "$IMAGE_ID" ]]; then
  echo "镜像不存在：$IMAGE" >&2
  exit 1
fi
GIT_COMMIT=$(git rev-parse HEAD 2>/dev/null || echo "unknown")
GIT_DIRTY=$(git status --porcelain 2>/dev/null | wc -l | tr -d ' ')
BUILT_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)
SBOM_HASH=""
if [[ -f "$ROOT_DIR/artifacts/sbom/image.cdx.json" ]]; then
  SBOM_HASH=$(shasum -a 256 "$ROOT_DIR/artifacts/sbom/image.cdx.json" | awk '{print $1}')
fi

python3 - "$OUT_DIR/provenance.json" "$IMAGE" "$IMAGE_ID" "$GIT_COMMIT" "$GIT_DIRTY" "$BUILT_AT" "$SBOM_HASH" <<'PY'
import json, sys
out, image, digest, commit, dirty, built_at, sbom = sys.argv[1:]
statement = {
  "_type": "https://in-toto.io/Statement/v1",
  "subject": [{"name": image, "digest": {"sha256": digest.replace("sha256:", "")}}],
  "predicateType": "https://slsa.dev/provenance/v1",
  "predicate": {
    "buildDefinition": {
      "buildType": "https://github.com/Mark-Panda/harness-agent/build/v1",
      "externalParameters": {"image": image, "command": "docker compose build app"},
      "resolvedDependencies": [
        {"uri": "git+https://github.com/Mark-Panda/harness-agent", "digest": {"gitCommit": commit}},
        {"uri": "sbom:image.cdx.json", "digest": {"sha256": sbom}},
      ],
    },
    "runDetails": {
      "builder": {"id": "https://github.com/Mark-Panda/harness-agent/ci"},
      "metadata": {"buildStartedOn": built_at, "buildFinishedOn": built_at, "gitDirtyFiles": int(dirty)},
    },
  },
}
json.dump(statement, open(out, "w"), indent=2, ensure_ascii=False)
PY

if [[ ! -f "$OUT_DIR/cosign.key" ]]; then
  echo "生成本地 cosign 密钥（仅用于演练；生产使用 KMS/密钥环）"
  docker run --rm -v "$OUT_DIR:/keys" -w /keys -e COSIGN_PASSWORD="$COSIGN_PASSWORD" \
    "$COSIGN_IMAGE" generate-key-pair
fi

docker run --rm -v "$OUT_DIR:/keys" -w /keys -e COSIGN_PASSWORD="$COSIGN_PASSWORD" \
  "$COSIGN_IMAGE" sign-blob --yes --key cosign.key --output-signature provenance.sig provenance.json
docker run --rm -v "$OUT_DIR:/keys" -w /keys \
  "$COSIGN_IMAGE" verify-blob --key cosign.pub --signature provenance.sig provenance.json

PROVENANCE_HASH=$(shasum -a 256 "$OUT_DIR/provenance.json" | awk '{print $1}')
PUB_HASH=$(shasum -a 256 "$OUT_DIR/cosign.pub" | awk '{print $1}')
python3 - "$OUT_DIR/approval.json" "$IMAGE" "$IMAGE_ID" "$PROVENANCE_HASH" "$PUB_HASH" <<'PY'
import json, sys
out, image, digest, provenance, pubkey = sys.argv[1:]
json.dump({
  "approved": True,
  "image": image,
  "digest": digest,
  "provenance_sha256": provenance,
  "cosign_public_key_sha256": pubkey,
  "provenance_file": "provenance.json",
  "signature_file": "provenance.sig",
  "public_key_file": "cosign.pub",
}, open(out, "w"), indent=2, ensure_ascii=False)
PY

echo "行为证明与签名完成：$OUT_DIR"
echo "镜像：$IMAGE ($IMAGE_ID)"
echo "审批文件：$OUT_DIR/approval.json"
