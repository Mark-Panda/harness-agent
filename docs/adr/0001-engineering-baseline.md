# ADR 0001：工程基线关键决策

- 状态：已接受
- 日期：2026-11-02
- 范围：认证、隔离、Agent Runtime、任务、SSE、执行记录、密钥、迁移与发布

## 背景

系统需要同时支持多用户、长时间 Agent 执行、外部模型与工具调用，以及可恢复的生产发布。阶段 0 先固定不可逆或高修改成本的边界，避免后续实现形成不兼容架构。

## 决策

### 1. 身份与隔离

用户身份仅来自 JWT 或可信飞书映射。Service 生成 Principal，Biz 显式传递 `userID`，Data 强制用户 Scope；私有表使用 `FORCE ROW LEVEL SECURITY`，后台 Worker 从持久化 Run 恢复用户上下文。

### 2. Agent Runtime 与模型路由

Runner 与 ModelProvider、ToolRegistry 解耦。模型目录与 Provider 分离，生成模型必须具备经核验的 1M+ 上下文。路由采用确定性候选策略、共享熔断和持久化 Attempt，不用另一个 LLM 决定故障转移。流式输出已开始后不静默拼接其他模型结果。

### 3. 持久化任务

PostgreSQL 是任务事实源。Job 与业务写入通过 Outbox 同事务提交，Worker 使用租约心跳、 fencing token、幂等消费、有限重试和死信。系统承诺至少一次投递，不宣称端到端 Exactly Once。

### 4. SSE 与恢复

Run、Step、Event 先持久化再推送。事件使用 `(run_id, seq)` 唯一序号和版本，客户端通过游标重放。重连只补事件，不创建新 Run；慢消费者、最大连接时间和取消必须有明确处理。

### 5. 执行记录与审计

模型请求/响应和工具 I/O 是受 RLS 保护的业务执行事实，不能写入 stdout。大请求使用 Manifest 和版本化引用避免重复存储。审计只记录操作事实，关键操作与审计同事务写入，使用 Append-only、Hash Chain 和签名 Checkpoint。隐藏思维链和明文 Secret 永远不保存。

### 6. 密钥管理

Provider/MCP 凭证使用带密钥版本的 AES-256-GCM 密文，生产密钥由 Secret Manager/KMS 注入。支持旧密文解密、逐条重存重加密和不停机轮换。YAML 只保存非密配置及运行时引用。

### 7. 数据保留与删除

PostgreSQL 是唯一事实源，Redis 只保存可重建状态。执行 Payload、业务数据和审计采用独立保留期。删除操作使用可恢复的异步编排，覆盖 PG、向量、缓存、绑定和任务；审计仅保留合规所需的去内容化记录。

### 8. 数据库迁移

迁移使用 Expand → Migrate → Contract，保证新旧应用并行。Migrate 角色在 API 启动前执行正向迁移，提供 `up`、正向 `steps` 和 `version`。发布回滚优先前滚修复，不把破坏性 down 迁移作为常规回滚手段。

### 9. 交付与发布

前后端交付同一非 root 镜像，但生产至少运行两个 API/Worker 副本。Compose 仅用于开发和验收。Release A/B 分别用于内部测试和预发布，只有 Release C 通过 SLO、容量、恢复、安全与回滚门禁后才能进入生产。

## 后果

- 所有后续 Repository、Runner、Job 和外部适配器必须遵守上述边界。
- 改变这些决策需要新的 ADR、兼容迁移和回滚验证。
- 阶段门禁不能只检查功能，还必须检查隔离、幂等、脱敏和恢复行为。
