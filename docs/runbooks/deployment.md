# 部署、配置与扩容 Runbook

## 1. 部署形态

- 单镜像同时包含 API、Worker 与前端静态资源；生产至少部署 2 个副本，API 与 Worker 同进程但按配置可分别启用。
- PostgreSQL 是唯一事实源；Redis 仅作为热缓存与限流，丢失后从 PostgreSQL 重建。
- Docker Compose 只用于本地/预发布验证，不承担生产高可用；生产使用编排平台或托管数据库。

## 2. 首次部署

1. 准备 PostgreSQL（启用 pgvector）与 Redis，创建 `harness`、`harness_app`、`harness_auth`、`harness_admin` 角色，执行 `deploy/postgres/init/001_roles.sql`。
2. 通过 Secret Manager 注入至少以下环境变量：
   - `DATABASE_URL`（应用角色）、`AUTH_DATABASE_URL`（受限认证角色）
   - `REDIS_ADDR`、`REDIS_PASSWORD`
   - `JWT_SIGNING_KEY_ID`、`JWT_SIGNING_KEY_SECRET`、`REFRESH_TOKEN_PEPPER`
   - `AUDIT_PSEUDONYM_KEY`
   - `CREDENTIAL_ENCRYPTION_KEY_ID`、`CREDENTIAL_ENCRYPTION_KEY_SECRET`
   - 生产 `TLS_ENABLED`、`TLS_CERT_FILE`、`TLS_KEY_FILE`、`SANDBOX_REQUIRED=true`
3. 先运行迁移：`bin/harness-migrate up`，确认 `schema_migrations.dirty=false`。
4. 启动应用，检查 `/health/live`、`/health/ready`、`/api/v1/health`。
5. 首个管理员引导：使用 `bin/harness-admin -username <name>` 创建管理员，密码通过 `ADMIN_PASSWORD` 或标准输入注入，禁止写入配置或镜像。创建后确认 `users.role=admin`，并将引导命令从日常权限中移除。
6. 注册策略：开发/预发布默认开放注册（`auth.registration_enabled=true`）；生产默认关闭，由管理员通过 `POST /api/v1/admin/users` 邀请创建，并使用一次性重置令牌交付初始凭据。
7. 执行 Smoke：注册/登录、创建会话、发起 Run、SSE 接收、审计查询。

## 3. 配置分层

| 层 | 文件 | 说明 |
| --- | --- | --- |
| 基线 | `configs/config.yaml` | 非密默认值，环境变量占位符 |
| 环境覆盖 | `configs/config.{staging,production}.yaml` | 生产 TLS、Sandbox 强制等差异 |
| 密钥 | Secret Manager / KMS | 禁止写入 YAML 或镜像 |
| 动态目录 | PostgreSQL | Provider、模型、策略、Skill、MCP、Agent 由管理接口维护 |

启动时使用 `KnownFields(true)` 与 `Validate()`；未知字段、缺失密钥、低上下文模型、生产默认口令均直接失败。

API 限流与来源 IP 推导：

| 配置 | 默认 | 说明 |
| --- | --- | --- |
| `server.api_rate_limit.global_per_minute` | 12000 | 单实例全局配额，0 关闭 |
| `server.api_rate_limit.per_user_per_minute` | 600 | 单登录用户配额，0 关闭 |
| `server.api_rate_limit.per_ip_per_minute` | 1200 | 单来源 IP 配额，0 关闭 |
| `server.trusted_proxies` | `[]` | 可信反向代理 CIDR；仅当直连地址命中网段时才从 `X-Forwarded-For` 从右向左取第一个非可信地址 |

- 未配置 `trusted_proxies` 时忽略 `X-Forwarded-For`，限流按直连地址计数；部署在七层网关后必须显式配置网关网段，否则所有客户端会共享网关 IP 配额。
- 限流失败关闭：Redis 不可用时 `/api/v1` 返回 503 `rate_limit_unavailable`，登录/注册同样返回 503，避免绕过保护。

## 4. 扩容

| 组件 | 扩容信号 | 动作 |
| --- | --- | --- |
| API/Worker | P95 > 200ms 持续 10 分钟；CPU > 70% | 增加副本；先排查连接池与慢查询 |
| Worker 并发 | 队列在途持续 > 100 | 提高副本数或 `agent.worker.concurrency`，同时评估 Provider 限流 |
| PostgreSQL | 连接池使用率 > 90% | 先修复慢查询/长事务，再评估 `max_open_conns` 与实例规格 |
| Redis | 延迟升高或内存接近上限 | 扩容或清理过期热窗口；Redis 故障按降级 Runbook 处理 |
| SSE | 连接数接近 500/实例 | 增加 API 副本；检查慢消费者写超时 |

- 扩容前确认用户舱壁 `per_user_concurrency=2` 与全局并发 `global_concurrency=100` 仍满足公平性要求。
- 每次扩容作为一个受控变更记录版本、操作者、观察指标与结果。

## 5. 验证命令

```bash
make build
docker compose up -d --build
curl -fsS http://localhost:8080/health/ready
bin/harness-loadgen -base http://localhost:8080 -concurrency 50 -rps 100 -duration 20s -json
```

## 6. CI 与分支保护

`main` 分支必须启用保护规则，以下检查为必过项（GitHub Required status checks）：

| 检查 | 覆盖内容 |
| --- | --- |
| `verify` | 生成物一致性、lint、单测、Race、覆盖率门禁、Buf Build/Breaking、前端测试、构建、数据库集成、Smoke、迁移版本与 pgvector |
| `security` | SAST、依赖/密钥/IaC 扫描、API Fuzz、许可证合规、镜像 HIGH/CRITICAL 扫描 |

- 要求 PR 至少 1 名评审通过、分支与目标分支同步后才可合并；禁止直接推送 `main`。
- `Buf Breaking` 以 PR 目标分支的 merge-base 为基线；基线尚无 protobuf 时按首次引入 API 跳过。
- 许可证例外必须登记在 `scripts/license_allowlist.txt` 并注明确认理由，禁止为通过检查批量豁免。

## 7. 迁移流程（Expand → Migrate → Contract）

1. **Expand**：只做加法变更——新增表用 `CREATE TABLE IF NOT EXISTS`，新增列必须可空或带 `DEFAULT`，新增索引使用 `CREATE INDEX CONCURRENTLY IF NOT EXISTS`。`make test-migration` 会拒绝 `DROP`、改类型、重命名与不可回填的列。
2. **Migrate**：由后台任务或双写完成数据回填；新旧代码并行期间旧查询必须继续可用。集成测试 `TestMigrationVersionToVersionUpgrade` 会在中间版本与最新版本各执行一次旧代码查询。
3. **Contract**：删除旧列/表必须等旧代码下线后单独评审执行，不得混入常规发布迁移。
4. **失败重入与前滚**：确认 `harness-migrate version` 的 `dirty=true` 后，修复 SQL，执行 `harness-migrate force <失败前版本>`，再执行 `harness-migrate up`；`TestMigrationFailureReentryAndRollForward` 覆盖该路径。
5. **历史不可变**：已发布迁移禁止修改；`migrations/checksums.txt` 由 `scripts/migration_checksum.sh` 生成并在 CI 校验，修复必须通过新的前滚迁移完成。
6. **漂移校验**：对现有库与全新迁移库分别执行 `pg_dump --schema-only --no-owner` 并比较；除 `\restrict` 等会话随机标记外必须无差异。权限漂移同样视为 schema 漂移，例如 `password_reset_tokens` 只允许 `harness_app` 拥有 `SELECT/DELETE`。
