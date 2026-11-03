# Harness Agent 容量与压测基线

本文定义目标峰值、1.5 倍峰值、实测结果、资源规格与扩容阈值。压测工具为 `cmd/loadgen`，可重复执行。

## 目标峰值定义

| 维度 | 目标峰值 | 1.5 倍峰值 | 依据 |
| --- | --- | --- | --- |
| 读 API | 100 RPS、50 并发 | 150 RPS、75 并发 | 单实例 API + 数据库读取 |
| API P95 | ≤ 50ms | ≤ 100ms | 用户可感知延迟 |
| 单实例 SSE | 500 连接 | 750 连接 | `server.sse.max_connections` 默认 500 |
| Run 并发 | 4（2 Worker 副本 × concurrency 2） | 6 | `agent.worker.concurrency=2` + 2 副本 |
| 队列在途 | ≤ 100 | ≤ 150 | `HarnessQueueBacklog` 阈值 |

## 实测环境

- 本地 Docker Desktop（Apple Silicon），单 API/Worker 实例，容器内存上限 1 GiB。
- PostgreSQL 18 + pgvector 0.8.7，Redis 8.2.3，均为单实例。
- 压测路径：`GET /api/v1/sessions`、`GET /api/v1/agents`（Bearer 鉴权，覆盖 RLS 与数据库读取）。
- 命令：

```bash
make build
bin/harness-loadgen -base http://localhost:8080 -concurrency 50 -rps 100 -duration 20s -json
bin/harness-loadgen -base http://localhost:8080 -concurrency 75 -rps 150 -duration 20s -json
bin/harness-loadgen -base http://localhost:8080 -concurrency 100 -rps 200 -duration 20s -json
```

## 实测结果

| 负载 | 请求数 | RPS | p50 | p95 | p99 | 5xx | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 100 RPS（峰值） | 1991 | 99.5 | 3.1ms | 5.9ms | 40.0ms | 0 | 无错误 |
| 150 RPS（1.5 倍） | 2992 | 149.6 | 2.6ms | 4.4ms | 104.1ms | 0 | 无错误 |
| 200 RPS（2 倍） | 4000 | 200.0 | 2.4ms | 3.0ms | 4.1ms | 0 | 1 次客户端瞬时连接错误，无 5xx |

1.5 倍峰值下资源采样：

| 组件 | CPU | 内存 |
| --- | --- | --- |
| harness-agent | 18.5% | 40.3 MiB / 1 GiB |
| PostgreSQL | 11.8% | 55.8 MiB |
| Redis | 0.5% | 11.3 MiB |

读 API 路径在 1.5 倍峰值下仍有明显余量；瓶颈优先出现在数据库连接池与外部 LLM/飞书调用，而不是 API 进程本身。

## 长时间稳定性

10 分钟持续 100 RPS 的结果见 [soak-result.md](soak-result.md)：

- 59,971 请求，99.95 RPS，5xx = 0，1 次客户端瞬时连接错误。
- P50 3.21ms、P95 4.85ms、P99 6.78ms。
- Goroutine 31–33、连接池等待 0；RSS 从约 59 MiB 增长到约 95 MiB（Go 堆保留），无持续单调增长。

## 专项覆盖

| 场景 | 验证方式 | 参考测试 |
| --- | --- | --- |
| 大消息 | 上下文 Token 预算裁剪、超长摘要输入截断 | `TestBuildMessagesAppliesTokenBudget`、`pkg/memory` 裁剪测试 |
| 慢消费者 | SSE 写入超时断开、连接上限 503 | `TestWriteSSEDetectsSlowConsumer`、`TestSSEConnectionCapacityAndRelease` |
| 工具风暴 | 工具循环检测、步数上限、预算上限 | `TestRunnerDetectsToolLoop`、`TestRunnerStepLimit`、`TestRunnerBudgetExceeded` |
| 队列积压 | 全局/用户舱壁退回任务且不消耗重试预算 | `TestWorkerDefersJobWhenGlobalLimitReached`、`TestWorkerDefersJobWhenUserLimitReached` |
| 多用户公平性 | 按用户舱壁隔离，单用户不得占满全局并发 | `TestWorkerDefersJobWhenUserLimitReached`、`TestWorkerReleasesSlotsAfterExecution` |

## 资源规格建议

| 组件 | 最小规格 | 推荐生产规格 | 说明 |
| --- | --- | --- | --- |
| API/Worker | 2 vCPU / 2 GiB | 2 vCPU / 4 GiB × ≥2 副本 | Worker 与 API 同进程；LLM 解析与 SSE 需要额外内存 |
| PostgreSQL | 2 vCPU / 8 GiB | 4 vCPU / 16 GiB，连接上限 ≥ 200 | 连接池 `max_open_conns` 默认 30/池 |
| Redis | 1 vCPU / 1 GiB | 1 vCPU / 2 GiB | 仅缓存与限流，丢失去可恢复 |
| 备份存储 | 与数据量同量级 | 主副本 + 异故障域镜像，保留 30 天 | 见 `deploy/backup/README.md` |

## 扩容阈值

| 信号 | 阈值 | 动作 |
| --- | --- | --- |
| API P95 | > 200ms 持续 10 分钟 | 增加 API 副本；先排查连接池与慢查询 |
| 容器 CPU | > 70% 持续 10 分钟 | 增加副本或提升 CPU |
| 数据库连接池使用率 | > 90% 持续 5 分钟 | 先排查慢查询/泄漏，再评估扩容与 `max_open_conns` |
| 在途 Job | > 100 持续 15 分钟 | 增加 Worker 副本；检查租约恢复与外部限流 |
| SSE 连接 | > 400/实例 | 增加副本或调整 `server.sse.max_connections`，同时扩容上游 |
| Outbox 失败率 | > 1% 持续 10 分钟 | 停止发布，检查审计写入与数据库状态 |
| LLM 费用 | > 5 USD/h | 检查重试/切换风暴与预算配置 |

## 结论

- 目标峰值 100 RPS 与 1.5 倍峰值 150 RPS 在单实例读路径下通过，P95 低于 5ms；2 倍峰值仍有 200 RPS 吞吐，出现 1 次客户端瞬时连接错误但无 5xx。
- 生产扩容以 150 RPS/实例为软上限，超过后增加副本；数据库连接池与外部 Provider 是首要容量风险。
- 长时稳定性与专项故障场景由上述测试持续守护，任何阈值变更需同步更新告警与 Runbook。
