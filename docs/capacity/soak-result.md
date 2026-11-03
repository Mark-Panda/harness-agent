# 10 分钟稳定性压测结果

## 环境

- 单 API/Worker 实例（Docker，内存上限 1 GiB），PostgreSQL 18 + pgvector，Redis 8.2.3。
- 压测路径：`GET /api/v1/sessions`、`GET /api/v1/agents`，Bearer 鉴权。
- 命令：`bin/harness-loadgen -concurrency 50 -rps 100 -duration 10m -json`
- 时间：2026-11-03（UTC）。

## 结果

| 指标 | 结果 |
| --- | --- |
| 总请求 | 59,971 |
| 平均 RPS | 99.95 |
| 错误 | 1 次客户端瞬时连接错误；5xx = 0 |
| P50 | 3.21ms |
| P95 | 4.85ms |
| P99 | 6.78ms |

## 资源与稳定性

| 采样点 | CPU | RSS | Goroutines | 连接池等待 |
| --- | --- | --- | --- | --- |
| 运行约 2 分钟 | 15.6% | 59.4 MiB | 33 | 0 |
| 运行结束 | 2.0%（空闲） | 95.4 MiB | 31 | 0 |
| 容器内存 | — | 73.5 MiB / 1 GiB | — | — |

- 进程 RSS 在 10 分钟内从约 59 MiB 增长到约 95 MiB，属于 Go 堆增长与 GC 保留；Goroutine 数与连接池等待保持稳定，无泄漏迹象。
- 持续压测期间 P95/P99 未随时间上升，连接池 `wait_total` 始终为 0。
- 1 次客户端连接错误未产生 5xx；服务端未记录异常。

## 判定

- 通过：0 个 5xx，P95 低于 50ms 目标，资源无持续单调增长。
- 建议：生产环境以 150 RPS/实例为软上限；连续运行 24 小时后对比 RSS 基线，若仍持续增长再采集 heap profile。
