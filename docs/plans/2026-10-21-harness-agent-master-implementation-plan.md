# Harness Agent 全栈多用户系统分步实施计划

> **执行约束：** 按阶段逐项实施；产品代码遵循测试先行，阶段门禁通过后再进入下一阶段。

**需求来源：** `AGENTS.md`  
**实施模式：** 单人串行开发，28 周，分 3 个可发布版本  
**技术基线：** Go 1.25、Kratos v3、Eino v0.9.21、Node.js 24 LTS、PostgreSQL 18 + pgvector 0.8.7、Redis 8.2 LTS

## 全局约束

- API、Service、Biz、Data 严格分层，Biz 层定义 Repository 接口，Data 层实现。
- 用户身份只从 JWT 或可信飞书身份映射产生；普通接口不接受客户端指定的 `user_id`。
- PostgreSQL 是唯一事实源，Redis 仅用于缓存、限流与短期状态。
- 用户私有 Repository 方法必须显式接收 `userID`；私有表以 PostgreSQL RLS 做第二道隔离。
- Access Token 15 分钟，Refresh Token 30 天并轮换；密码使用 Argon2id。
- Provider/MCP 凭证使用 AES-256-GCM 加密，任何 API、日志、Trace 均不得返回明文。
- 不保存或展示隐藏思维链，只记录可公开的 `reasoning_summary`、工具调用和状态变化。
- Web 与 Go 后端进入同一非 root 应用镜像；数据库、缓存与可观测组件独立部署。
- `internal/biz`、`pkg/agent`、`pkg/memory` 行覆盖率不得低于 80%，鉴权与租户隔离目标不低于 90%。

## 目标架构

```text
api/harness/v1/          Protobuf、HTTP/gRPC 契约
cmd/server/              API、Worker、静态资源统一进程入口
cmd/migrate/             数据库迁移命令
internal/biz/            聚合、领域服务、Repository 接口
internal/data/           Gorm、Redis、外部服务实现
internal/service/        Protobuf Service、鉴权上下文转换
internal/server/         HTTP、gRPC、SSE、静态文件和中间件
internal/platform/       auth、crypto、jobs、observability、ratelimit
pkg/agent/               Eino 适配与三种 Agent Runner
pkg/memory/              上下文预算、摘要、向量和事实记忆
pkg/feishu/              飞书验签、身份、事件和回复适配
migrations/              PostgreSQL/pgvector 版本化迁移
web/                     React 前端
deploy/                  Docker Compose、监控和生产示例
tests/                   集成、契约、E2E、负载测试
```

核心 API 使用 `/api/v1` 前缀，覆盖认证、平台配置、Agent、Session、Message、Run、Plan、Memory 与飞书事件。聊天接口使用 SSE，事件类型固定为 `meta`、`message.delta`、`tool.started`、`tool.completed`、`plan.updated`、`worker.started`、`worker.completed`、`run.completed`、`run.failed`、`heartbeat`。

核心数据表包括：`users`、`user_identities`、`refresh_tokens`、`llm_providers`、`skills`、`mcps`、`agents`、`agent_skills`、`agent_mcps`、`sessions`、`session_members`、`messages`、`agent_runs`、`execution_steps`、`run_events`、`plans`、`plan_steps`、`session_summaries`、`memory_vectors`、`memory_facts`、`external_conversation_bindings`、`feishu_event_receipts`、`jobs`、`outbox_events`、`audit_logs`。

## 分阶段任务

### 阶段 0：工程基线与技术验证（第 1–2 周）

- [ ] 初始化 Git、Go Module、Kratos v3、Buf、React/Vite 和 Makefile。
- [ ] 建立代码生成、lint、单测、集成测试、前端测试和镜像构建命令。
- [ ] 编写认证、隔离、Agent Runtime、任务、SSE 与密钥管理 ADR。
- [ ] 建立 PostgreSQL/pgvector、Redis、Jaeger、Prometheus 的 Docker Compose。
- [ ] 完成 Kratos、Eino、SSE、pgvector 与前端 embed 的最小技术验证。
- [ ] 建立 CI；验证健康检查、迁移、前端占位页及镜像构建。

### 阶段 1：数据库、鉴权与隔离基座（第 3–5 周）

- [ ] 编写身份、配置、会话、执行、记忆、任务和审计迁移。
- [ ] 实现 Gorm 通用字段、事务、分页与错误映射。
- [ ] 实现用户 Scope、RLS 与跨用户攻击测试。
- [ ] 实现注册、登录、刷新、注销、Argon2id 与 Token 轮换。
- [ ] 实现 Principal、RBAC、限流、审计与敏感字段脱敏。
- [ ] 建立 Repository Mock 与 Biz 测试模板。

### 阶段 2：平台配置、Agent 与会话领域（第 6–8 周）

- [ ] 实现 Provider、Skill、MCP 管理员 CRUD、凭证加密与连接测试。
- [ ] 实现 OpenAI-compatible Chat/Embedding 适配器。
- [ ] 实现 Agent 聚合、模式配置、Skill/MCP 绑定和配置快照。
- [ ] 实现 Session、Message、Run 聚合与状态机。
- [ ] 定义 MCP Schema、权限等级、超时、审计与 SSRF 防护。
- [ ] 完成 Protobuf、HTTP/gRPC、OpenAPI 与前端类型生成。

### 阶段 3：ReAct、工具与流式执行（第 9–11 周）

- [ ] 建立 Runner、ModelProvider、ToolRegistry 抽象与 Eino 适配。
- [ ] 实现最多 12 步、工具 60 秒、Run 5 分钟的 ReAct Runner。
- [ ] 实现工具调用 Hash 与连续重复 3 次终止规则。
- [ ] 实现 Run/Step/Event 先持久化后推送的 SSE 链路。
- [ ] 实现取消、重启恢复、幂等和上游超时映射。
- [ ] 远程 MCP 限制为管理员允许的 HTTPS Endpoint，并默认阻止内网地址。

### 阶段 4：分层记忆引擎（第 12–14 周）

- [ ] 实现 Token 预算与 Redis 滑动窗口。
- [ ] 实现带来源范围与版本的异步摘要任务。
- [ ] 实现 1536 维 embedding、HNSW cosine、Top-30 与 MMR Top-8。
- [ ] 实现结构化事实、冲突更新、来源追踪和删除。
- [ ] 按 System → 长期记忆 → 摘要 → 最近消息 → 当前输入组装上下文。
- [ ] 实现 Redis、向量和摘要故障时的明确降级。

### 阶段 5：Web 产品闭环与 Release A（第 15–17 周）

- [ ] 实现认证与受保护路由。
- [ ] 实现 Provider、Skill、MCP 管理页面。
- [ ] 实现 Agent、Session、对话及 Markdown/代码渲染。
- [ ] 实现 SSE 增量、取消、重连、重试和执行树。
- [ ] 将 Web 静态资源 embed 到 Go 二进制并支持 SPA 回退。
- [ ] 完成非 root 单应用镜像和部署说明。

### 阶段 6：Plan-and-Execute（第 18–20 周）

- [ ] 实现 Plan 与 PlanStep 状态机。
- [ ] Planner 输出执行 JSON Schema 校验，最多 50 步。
- [ ] 每步事务性持久化并支持失败重试、取消、继续和重启恢复。
- [ ] 使用乐观锁阻止重复推进，并在 SSE/Web 中展示状态。

### 阶段 7：Supervisor 与 Release B（第 21–22 周）

- [ ] 实现 Worker 能力注册、任务委派和汇总协议。
- [ ] 子任务使用独立 Session 并关联主 Run 与当前用户。
- [ ] 默认并发 4、嵌套深度 2，统一传播取消信号。
- [ ] 显式展示部分失败，并在前端显示执行树。

### 阶段 8：飞书集成（第 23–25 周）

- [ ] 实现 URL 验证、验签、事件解密和配置检查。
- [ ] 实现 open_id 身份映射、单聊与群聊 Session 映射。
- [ ] event_id 幂等入库并快速 ACK，再由持久化任务处理。
- [ ] 群聊不注入个人长期记忆；个人事实只写发言者私有记忆。
- [ ] 使用可更新卡片增量回复，失败时降级最终文本。
- [ ] 覆盖重放、重复、乱序、Token 失效和限流重试。

### 阶段 9：安全、可观测性与 Release C（第 26–28 周）

- [ ] OpenTelemetry 覆盖 HTTP/gRPC、Eino、LLM、Tool、Job、飞书。
- [ ] 暴露 Token、Run、工具、队列与 SSE 指标，指标不使用 user_id 标签。
- [ ] 高风险工具只允许在独立受限 Sandbox Runner 中执行。
- [ ] 完成 100 并发 SSE、依赖故障、Worker 重启与恢复测试。
- [ ] 完成备份恢复、迁移回滚、漏洞扫描、SBOM 与运维文档。

## 验证命令

每阶段运行：

```bash
make generate-check
make lint
make test-unit
make test-race
make test-integration
make test-web
make build
```

发布阶段额外运行：

```bash
make test-e2e
make test-security
make test-load
docker compose up --build --wait
make smoke
```

强制验证跨用户 ID 枚举、缓存/向量串扰、JWT 过期与重放、SSE 断线与取消、Agent 循环、Plan 恢复与幂等、Supervisor 部分失败、依赖降级、飞书伪造与重复投递，以及所有密钥的输出脱敏。

## 默认裁决

- v1 只提供 OpenAI-compatible Provider；其他厂商通过适配接口接入。
- Redis 清空不得造成永久数据丢失。
- 应用无状态，任务领取使用 PostgreSQL 行锁和租约。
- 群聊不使用成员个人长期记忆。
- 高风险 MCP 工具在 Sandbox 可用前保持禁用。
- 数据默认长期保留；删除 Session/Memory 时同步清理 PG、向量、缓存和外部绑定。
- 生产镜像与依赖固定精确版本并生成 SBOM。
