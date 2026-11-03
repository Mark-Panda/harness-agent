# 供应链与部署安全

## 制品

| 证据 | 生成方式 | 产物 |
| --- | --- | --- |
| SBOM（仓库/镜像，CycloneDX + SPDX） | `scripts/sbom.sh` | `artifacts/sbom/*.json` |
| 构建来源证明（SLSA v1 predicate） | `scripts/supply_chain.sh` 或 release workflow | `artifacts/signing/provenance.json` |
| 签名 | cosign sign-blob（本地演练）/ cosign sign（默认 keyless） | `provenance.sig` 或 registry 签名 |
| 部署审批 | 签名验证通过后生成 `approval.json` | 镜像 digest + 哈希 |

## 本地演练

```bash
scripts/sbom.sh harness-agent-app:latest
scripts/supply_chain.sh harness-agent-app:latest
deploy/signing/verify.sh artifacts/signing/approval.json <image-id>
```

本地镜像无 registry manifest digest 时使用 image ID 作为 subject；生产必须使用推送后的 `sha256:` manifest digest。演练密钥仅用于本地，禁止用于生产。

## 生产发布

`.github/workflows/release.yml` 在 `v*` tag 上执行：

1. 构建并推送 GHCR 镜像，输出 manifest digest。
2. Trivy 扫描镜像，HIGH/CRITICAL 直接失败。
3. 生成镜像与仓库 SBOM。
4. 生成 SLSA provenance 并以 GitHub OIDC keyless 方式 `cosign sign` 与 `cosign attest`。
5. 使用固定 workflow identity 与 OIDC issuer 验证签名和证明，上传证据。

部署系统只能拉取 `approval.json` 中批准的 digest；禁止仅按 tag 部署。验证命令：

```bash
deploy/signing/verify.sh artifacts/signing/approval.json "$IMAGE_ID"
cosign verify "$IMAGE" --certificate-identity-regexp ... --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## 生产加密与网络

- 应用层 TLS：`server.http.tls.enabled=true`，证书由密钥管理挂载，最低 TLS 1.3；入口到应用端到端加密。
- 数据库与备份静态加密：托管磁盘加密或卷加密；备份文件使用 AES-256-GCM（`deploy/backup`）。
- Redis：必须配置密码/ACL；生产优先 TLS；Redis 不作为事实数据源。
- 网络分段：见 `deploy/network/README.md`；只暴露 443，数据区无公网入站，管理接口仅 VPN/管理区。
- 应用容器：`docker-compose.yml` 已启用只读根文件系统、`cap_drop: ALL`、`no-new-privileges`、内存与 CPU 限制。

## 审批与回滚

- 每个版本生成独立 `approval.json`，与 SBOM、provenance、签名一起归档。
- 部署前校验 digest 与签名；校验失败立即阻断。
- 回滚使用上一个已批准 digest，禁止重新构建覆盖旧 tag。
