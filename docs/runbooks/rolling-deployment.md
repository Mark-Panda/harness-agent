# 滚动发布与优雅退出 Runbook

## 发布前检查

1. 确认迁移已先行完成，且 `go run ./cmd/migrate version` 为 `dirty=false`。
2. 确认错误预算剩余 > 10%；否则只允许修复类发布。
3. 确认 Sandbox Runner、OTLP Collector、PostgreSQL、Redis 健康。
4. 备份当前镜像 digest 与配置版本，准备回滚。

## 滚动顺序

1. 先滚动 Worker 副本：新副本启动后保持 readiness=false 一小段时间，确认迁移兼容后置 true。
2. 再滚动 API 副本；负载均衡只向 readiness=true 的实例转发。
3. 每批次至少保留一个旧副本处理在途 Run 与 SSE 连接。
4. 观察 15 分钟：5xx、P95、QueueBacklog、OutboxFailures、SSE 连接与费用指标。
5. 异常时立即回滚上一镜像 digest，保留数据库迁移（迁移必须向后兼容）。

## 优雅退出行为

应用收到 SIGTERM 后：

1. `readiness` 立即返回 503，负载均衡摘流。
2. `BeforeStop` 等待在途任务计数归零，最长 10 秒；期间不领取新任务。
3. 超过等待窗口后取消 Worker 领取循环，未完成任务由 Job 租约与 fencing token 恢复。
4. SSE 连接断开后客户端携带 `Last-Event-ID` 或 `after` 游标重连，从断点回放事件。
5. Worker 使用 `run_id` 幂等键、审批 CAS 与 Outbox 去重，重启不会重复副作用。

## 双副本验证脚本

```bash
# 启动两个应用副本（不带宿主端口，由负载均衡/Service 访问）
docker compose -f docker-compose.yml -f deploy/compose/rolling.yaml up -d --build

# 模拟滚动：只重建一个副本，观察 readiness 与在途任务
docker compose -f docker-compose.yml -f deploy/compose/rolling.yaml up -d --scale app=2 --no-recreate app

# 持续探测 readiness 与 SSE 重连
curl -fsS http://localhost:8080/health/ready
curl -fsS -N http://localhost:8080/api/v1/runs/<run_id>/events?after=<seq>
```

验收标准：

- 任一时刻至少一个副本 readiness=true。
- 滚动期间 5xx 不显著上升，SSE 客户端可携带游标重连并补齐事件。
- 被终止副本的在途 Run 在租约过期后由存活副本继续，且不产生重复工具副作用。
- Outbox/审计链无缺口，Run 终态唯一。
