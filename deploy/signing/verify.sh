#!/usr/bin/env bash
# 部署前校验：验签行为证明、比对哈希，并确认镜像 digest 在审批文件中。
set -euo pipefail

APPROVAL_FILE=${1:?请传入 approval.json}
IMAGE=${2:-}
DIR="$(cd "$(dirname "$APPROVAL_FILE")" && pwd)"
COSIGN_IMAGE=${COSIGN_IMAGE:-gcr.io/projectsigstore/cosign:v2.4.3}

python3 - "$APPROVAL_FILE" "$DIR" "$IMAGE" <<'PY'
import hashlib, json, os, sys
approval = json.load(open(sys.argv[1]))
if not approval.get("approved"):
    raise SystemExit("镜像未被批准")
root = sys.argv[2]
def digest(name):
    path = os.path.join(root, name)
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()
if digest(approval["provenance_file"]) != approval["provenance_sha256"]:
    raise SystemExit("provenance 哈希不匹配")
pub = approval["public_key_file"]
if digest(pub) != approval["cosign_public_key_sha256"]:
    raise SystemExit("cosign 公钥哈希不匹配")
if sys.argv[3] and sys.argv[3] != approval["digest"]:
    raise SystemExit(f"镜像 digest 不在审批文件中：{sys.argv[3]} != {approval['digest']}")
print(f"审批记录有效：{approval['image']} {approval['digest']}")
PY

docker run --rm -v "$DIR:/keys" -w /keys "$COSIGN_IMAGE" \
  verify-blob --key cosign.pub --signature provenance.sig provenance.json
echo "签名验证通过"
