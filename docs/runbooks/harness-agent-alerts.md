# Harness Agent 告警与 SLO Runbook

本文对应 `deploy/prometheus/alerts.yaml` 中全部 Critical/High 告警。告警触发测试位于
`deploy/prometheus/alerts_test.yaml`，执行 `make monitoring-check` 会在 Prometheus 镜像内运行
`promtool check rules` 与 `promtool test rules`。

## 通用排查步骤

1. 在 Prometheus/Grafana 确认告警开始时间、影响实例与最近一次发布。
2. 检查 `http://<instance>:8080/health/live`、`/health/ready`、`/api/v1/health`。
3. 查看结构化日志中的 `request_id`、`trace_id`、`run_id`，并在 Jaeger 按 Trace 下钻。
4. 检查 PostgreSQL/Redis 连接、迁移版本与 `dirty` 状态。
5. 变更抑制：确认非预期发布导致时，按发布流程回滚；禁止直接删除审计数据。

## HarnessAgentDown

- 级别：Critical。
- 现象：`up{job="harness-agent"} < 1` 持续 2 分钟。
- 排查：容器/Pod 是否 OOM、readiness 是否失败、端口 8080 是否监听、网络策略与证书。
- 恢复：重启实例前先确认流量已摘除；若为数据库/Redis 故障，先恢复依赖再启动应用。

## HarnessAvailabilityBudgetBurn

- 级别：Critical（30 天 SLO 99.9%，1 小时燃烧率 ≥ 14.4 倍）。
- 排查：按 `route` 与 `status` 拆分 `harness_api_requests_total`，确认 5xx 是否集中于单一接口；
  检查最近发布、数据库连接池、Redis 延迟与 LLM Provider 错误。
- 恢复：优先回滚最近发布或摘除异常实例；错误预算耗尽前冻结非必要变更。

## HarnessAPI5xxHigh

- 级别：High。
- 排查：`sum by (route,status) (rate(harness_api_requests_total{status=~"5.."}[5m]))`；
  结合 Trace 定位到 Data/Service 层错误码。
- 常见原因：连接池耗尽、迁移未完成、加密密钥版本不匹配、外部 Provider 认证失败。
- 恢复：修复依赖或回滚；确认 5xx 比例持续低于 5% 后再解除告警抑制。

## HarnessAPILatencyHigh

- 级别：High（P95 > 2s 持续 10 分钟）。
- 排查：`histogram_quantile(0.95, sum by (le) (rate(harness_api_request_duration_seconds_bucket[5m])))`；
  检查慢查询、Redis 超时、LLM/飞书慢调用、SSE 长连接与 Worker 排队。
- 恢复：扩容 Worker/副本、调整超时与限流；禁止盲目重试放大外部依赖压力。

## HarnessDBPoolSaturation

- 级别：High（连接池使用率 ≥ 90% 持续 5 分钟）。
- 排查：查看 `harness_db_pool_in_use_connections`、`harness_db_pool_wait_total` 与慢查询/长事务；
  同时检查 `/health/ready` 的 `dependencies.postgres` 是否伴随故障转移。
- 恢复：修复慢查询或连接泄漏，按数据库容量评估扩容；详细步骤见
  [故障注入与降级 Runbook](failure-degradation.md#harnessdbpoolsaturation)。

## HarnessQueueBacklog

- 级别：High（在途任务 > 100 持续 15 分钟）。
- 排查：`sum by (type) (harness_agent_jobs_inflight)`、`rate(harness_agent_jobs_total{outcome="failure"}[10m])`；
  检查 Worker 副本数、租约是否可恢复、Provider 限流与单任务耗时。
- 恢复：扩容 Worker；若为毒消息，定位死信并按死信重放流程处理。

## HarnessOutboxFailures

- 级别：High（审计 Outbox 失败速率持续 10 分钟）。
- 排查：`rate(harness_agent_outbox_events_total{outcome="failure"}[5m])` 按 `event_type` 拆分；
  检查 `audit_logs` 写入权限、Hash Chain 校验、数据库磁盘与锁等待。
- 恢复：审计写入失败属于合规阻断，必须修复数据库/权限；禁止跳过审计继续写入业务数据。

## HarnessLLMCostSpike

- 级别：High（30 分钟费用速率 > 5）。
- 排查：按 `mode` 查看 `harness_agent_llm_tokens_total` 与预算告警事件；检查异常重试、
  模型切换、上下文摘要失控与单用户高频调用。
- 恢复：调整每日预算/限流；确认非正常调用后按租户限流或暂停相关 Agent。

## HarnessSSEConnectionSurge

- 级别：High（SSE 连接 > 500 持续 10 分钟）。
- 排查：客户端重连风暴、`after` 游标回放、实例重启与负载均衡超时。
- 恢复：扩容副本、调整连接超时；确认 SSE 可断线重连且不会重复触发 Run。

## HarnessMemoryFailures

- 级别：High（记忆操作失败率 > 20% 持续 10 分钟）。
- 排查：向量表与 pgvector 索引状态、Embedding Provider、摘要模型与数据库连接。
- 恢复：记忆失败按设计降级为无记忆上下文，不得阻断 Run；修复依赖后观察失败率回落。

## 看板与错误预算

Grafana 看板 `deploy/grafana/dashboards/harness-agent-slo.json` 覆盖：
- 可用性、5xx 比例与 P95 延迟（API RED）。
- Run 吞吐、工具失败、Job 在途与 Outbox 结果。
- Token、费用、SSE 连接与记忆操作失败率（容量与成本）。
- 资源 USE 指标来自 Prometheus Go/Process Collector。

错误预算策略：
- 30 天可用性 SLO 99.9%，1 小时错误率 ≥ 1.44% 立即告警并冻结变更。
- 发布门禁：错误预算剩余 < 25% 时只允许修复类发布；< 10% 时停止非紧急发布。

## 升级路径

1. 值班工程师 5 分钟内确认告警与影响面。
2. 15 分钟未恢复升级至服务负责人，并同步 Incident 记录。
3. 涉及跨用户数据泄露、审计链断裂或密钥泄露时立即按安全事件流程升级。
