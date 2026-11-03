# 预发布验收、灰度与回滚

## 前置条件

- 制品以 digest 固定：`approval.json` 必须包含镜像 digest、provenance 哈希与 cosign 签名。
- 数据库迁移只允许加法（expand-contract），仓库中不存在 `.down.sql`。
- 预发布环境使用生产等效配置（`APP_ENV=staging`，Sandbox 强制、Cookie Secure、TLS 视入口而定）。

## 验收流程

```bash
scripts/sbom.sh "$APPROVED_IMAGE"
APPROVAL_FILE=artifacts/signing/approval.json APPROVED_IMAGE="$APPROVED_IMAGE" scripts/acceptance.sh
```

`scripts/acceptance.sh` 执行：

1. `verify.sh` 校验审批、provenance 哈希与 cosign 签名。
2. `TestMigrationsAreForwardCompatible` 阻止删除列/表、改类型、重命名与 down 迁移。
3. 主实例 `/health/ready`、`/api/v1/health`。
4. 用同一镜像启动灰度副本（8083），等待 readiness，验证 health 与 SSE 探针；失败自动停止灰度，旧实例不变。

## 灰度切流

1. 灰度副本通过 Smoke 后，将入口流量按 5% → 25% → 50% → 100% 逐步切到灰度。
2. 每档观察 10 分钟：5xx、P95、队列在途、SSE 重连、审计 Outbox。
3. 100% 后再滚动替换主实例；保留旧实例 30 分钟以便回滚。
4. 灰度期间禁止执行破坏性迁移；新库必须能同时服务旧/新应用。

## 回滚

```bash
docker tag "$PREVIOUS_APPROVED_IMAGE" harness-agent-app:latest
docker compose up -d app
scripts/canary_smoke.sh   # 可选：验证回滚镜像
```

- 回滚只切换到上一个已批准 digest，不重新构建。
- 禁止执行 `.down.sql`；如新迁移必须回退，使用新的前向迁移修复数据。
- 回滚后检查：readiness、5xx、队列积压、在途 Run（租约恢复）、Outbox 与审计链。

## 兼容性验证

| 项 | 验证方式 |
| --- | --- |
| 新库兼容旧应用 | 迁移仅加法；`TestMigrationsAreForwardCompatible`；`make test-integration` 在最新 schema 运行 |
| 队列消息兼容 | Job 载荷只新增字段、旧字段语义不变；旧 Worker 忽略未知字段 |
| 在途 Run 恢复 | `docs/runbooks/rolling-deployment.md` 的租约/fencing 演练与 Worker 集成测试 |
| 破坏性降级迁移 | 仓库禁止 down 文件，测试扫描 `DROP/RENAME/ALTER TYPE/TRUNCATE` |

## 阻断条件

- 审批/签名任一失败。
- 迁移守卫失败或存在 down 迁移。
- 灰度 Smoke 失败、5xx 超阈值或 readiness 无法恢复。
- 跨用户隔离、审计写入或 Sandbox 强制策略回归。
