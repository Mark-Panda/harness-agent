# Harness Agent 故障注入与降级 Runbook

本文覆盖 PostgreSQL 高可用/故障转移、Redis 丢失与恢复、数据库连接池耗尽、LLM/MCP/飞书慢调用和不可用。
每个场景都给出自动降级行为、用户可见提示、恢复动作与验收命令；所有故障注入均可在本地或预发布环境执行。

## 故障矩阵

| 场景 | 检测信号 | 自动降级 | 用户提示 | 恢复动作 |
| --- | --- | --- | --- | --- |
| PostgreSQL 主备切换 | `/health/ready` 503、`harness_db_pool_*` 异常、5xx 升高 | 实例退出负载均衡；Worker 认领失败后按轮询重试；在途 Run 依赖租约与 fencing 恢复 | API 返回 500 `internal_error` 或 503 `dependency_timeout`（带 `Retry-After: 5`） | 确认新主可写、连接池重建、readiness 恢复后重新放入流量 |
| Redis 进程丢失 | `session:*:messages` 读取失败日志、登录 503 `rate_limit_unavailable` | 热消息回退 PostgreSQL；并发舱壁与模型熔断放行；登录限流失败关闭；刷新并发重放窗口失效 | 聊天与执行保持可用；登录/注册短暂 503；并发刷新可能被判定重用并要求重新登录 | Redis 恢复后客户端自动重连；无需重启应用 |
| 连接池耗尽 | `harness_db_pool_in_use_connections / max_open > 0.9`、请求等待 | 请求按上下文超时失败；超时返回 503；不无限阻塞 | 503 `dependency_timeout`，客户端按 `Retry-After` 重试 | 定位慢查询/连接泄漏，临时扩容副本或提高 `max_open_conns`；禁止重启前未摘流 |
| LLM 慢调用/不可用 | 尝试耗时、`timeout` 错误类、熔断打开 | 按 `request_timeout` 放弃慢调用；同一模型重试后切换健康候选；熔断打开后跳过该模型 | Run 失败码 `no_model_candidates`/`model_attempts_exhausted`；前端执行树显示尝试与失败原因 | 确认 Provider 恢复；熔断冷却后半开自动探测；必要时调整候选优先级 |
| MCP 慢调用/不可用 | `prepare MCP tools failed` 日志、工具步骤缺失 | MCP 准备超时或 5xx 时降级为空工具集，Run 继续执行；凭证读取失败跳过该 MCP | 回答可能缺少工具能力，但不阻断对话 | 确认 MCP 端点与凭证；发布新 Agent 版本或等待下一轮 Run 自动重载 |
| 飞书慢调用/不可用 | 回复 Worker 重试、`read feishu response` 错误、卡片降级日志 | HTTP 超时快速失败；回复任务保留在 Outbox，按退避重试；卡片更新失败降级最终文本 | Web 端不受影响；飞书侧可能延迟收到回复或只收到文本 | 确认开放平台状态与 Token；恢复后 Outbox 自动重试，不重复发送 |

## HarnessDBPoolSaturation

- 级别：High。`harness_db_pool_in_use_connections` 达到 `max_open_connections` 的 90% 持续 5 分钟触发。
- 排查步骤：
  1. Grafana「数据库连接池与等待」查看 `in use`、`max` 与 `waits/s`，区分容量不足与连接泄漏。
  2. 检查 `/health/ready` 的 `dependencies.postgres`，确认是否伴随故障转移或网络抖动。
  3. 在数据库侧查看长事务、慢查询与锁等待；应用侧查看 `request_id` 对应的 Trace。
  4. 对比 `max_open_conns` 与副本数，确认总连接数未超过数据库上限。
- 恢复动作：
  - 临时扩容 API/Worker 副本前先确认数据库剩余连接容量。
  - 修复慢查询或泄漏后，连接会随请求释放自动回落；无需重启实例。
  - 若为迁移/故障转移导致，等待 readiness 恢复并确认写入成功。
- 验收：`REQUIRE_DATABASE=true go test -tags=integration ./internal/data -run TestConnectionPoolExhaustionTimeoutAndRecovery`。

## PostgreSQL 高可用与故障转移

- 自动行为：
  - `/health/ready` 的 `postgres` 探针为关键依赖，失败返回 503，负载均衡摘除实例。
  - 连接池在数据库恢复后自动重建连接；`OpenPostgres` 设置最大连接数、空闲超时与生命周期上限。
  - Worker 认领任务失败时按 `poll_interval` 重试；已认领任务依赖租约、心跳与 fencing token 恢复，不重复产生副作用。
  - API 依赖调用超时返回 503 `dependency_timeout` 与 `Retry-After: 5`。
- 用户提示：Web 端显示「服务暂时不可用，请稍后重试」；SSE 客户端按事件游标重连。
- 恢复动作：
  1. 确认主库可写、复制延迟与迁移版本（`migrate version`）正常。
  2. 观察 `harness_db_pool_open_connections` 恢复且 `/health/ready` 返回 `status=ready`。
  3. 再放入流量；检查队列积压与在途 Run 是否继续推进。
  4. 核对最近审计 Hash Chain 与 Outbox 是否有积压。
- 禁止：在未摘流的情况下直接重启全部实例；手工删除审计或执行记录。

## Redis 丢失与恢复

- 自动行为：
  - 热消息读取失败时 Worker 回退 PostgreSQL 并重建缓存，同时写入 `context.degraded` 事件。
  - 并发舱壁（全局/用户/供应商）Redis 故障时放行，优先保证执行可用性。
  - 模型熔断状态读取失败时退化为静态候选顺序，仍保留 1M+ 与能力硬过滤。
  - 登录/注册限流失败关闭，返回 503 `rate_limit_unavailable`；刷新并发重放不可用时按重用保护撤销令牌族。
- 用户提示：聊天与执行继续可用；登录/注册短暂 503；并发刷新可能要求重新登录。
- 恢复动作：
  1. 恢复 Redis 后确认 `/health/ready` 从 `degraded` 回到 `ready`。
  2. 观察热缓存重建与登录恢复；无需重启应用。
  3. 若发生过刷新重用保护，确认受影响的用户重新登录。
- 验收：
  - `go test ./internal/data -run TestHotMessageStoreRecoversAfterRedisRestart`
  - `go test ./internal/platform/ratelimit -run TestRedisLimiterFailsClosedThenRecovers`

## 连接池耗尽

- 自动行为：获取连接受请求上下文约束；超时后请求失败并返回 503，不会无限占用 Worker。
- 用户提示：503 `dependency_timeout`，`Retry-After: 5`。
- 恢复动作：
  1. 通过连接池指标与 Trace 定位长事务；修复后连接自动释放。
  2. 需要扩容时同步评估数据库最大连接数，避免压垮数据库。
- 验收：`REQUIRE_DATABASE=true go test -tags=integration ./internal/data -run TestConnectionPoolExhaustionTimeoutAndRecovery`。

## LLM 慢调用与不可用

- 自动行为：
  - 单次调用受 `request_timeout` 约束；超时计入健康失败并触发重试/切换。
  - 连续失败达到阈值后熔断，冷却后半开只放行一个探测请求。
  - 永久错误（400/内容审核/上下文超限）不通过切换绕过校验。
- 用户提示：Run 失败码 `no_model_candidates`、`model_attempts_exhausted` 或 `model_switches_exhausted`；执行树展示每次尝试的耗时和错误分类。
- 恢复动作：
  1. 在「模型路由」页面确认熔断状态与半开探测结果。
  2. Provider 恢复后无需手工清除熔断，冷却结束自动探测。
  3. 若长时间不可用，调整候选优先级或启用备用供应商。
- 验收：`go test ./pkg/agent/model -run TestRouterSlowProviderTimesOutAndSwitches`。

## MCP 慢调用与不可用

- 自动行为：`mcp_timeout` 覆盖工具发现与调用；准备超时、5xx、客户端构造失败或凭证读取失败时降级为空工具集，Run 继续执行。
- 用户提示：回答可能缺少工具结果，但不会因 MCP 故障中断对话。
- 恢复动作：确认端点、TLS 与凭证；发布新 Agent 版本或等待下一轮 Run 重新加载绑定。
- 验收：`go test ./internal/biz -run TestPrepareMCPTools`。

## 飞书慢调用与不可用

- 自动行为：HTTP 客户端超时快速失败；回复任务保留在 Outbox 并按退避重试；卡片更新失败降级为最终文本。
- 用户提示：飞书侧可能延迟回复或只收到文本；Web 端不受影响。
- 恢复动作：确认飞书开放平台状态、应用凭证与 Token；恢复后 Outbox 自动重试，业务去重键避免重复发送。
- 验收：`go test ./pkg/feishu -run TestClientSlowUpstreamFailsFast`。

## 统一验收命令

```bash
go test ./internal/server -run TestReadiness
go test ./pkg/metrics -run TestRegisterDBPool
go test ./pkg/agent/model -run TestRouterSlowProviderTimesOutAndSwitches
go test ./internal/biz -run TestPrepareMCPTools
go test ./pkg/feishu -run TestClientSlowUpstreamFailsFast
go test ./internal/data -run TestHotMessageStoreRecoversAfterRedisRestart
go test ./internal/platform/ratelimit -run TestRedisLimiterFailsClosedThenRecovers
REQUIRE_DATABASE=true go test -tags=integration ./internal/data -run TestConnectionPoolExhaustionTimeoutAndRecovery
make monitoring-check
```
