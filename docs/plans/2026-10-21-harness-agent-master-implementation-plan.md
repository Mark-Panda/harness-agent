# Harness Agent 全栈多用户系统分步实施计划

> **执行约束：** 按阶段逐项实施；产品代码遵循测试先行，阶段门禁通过后再进入下一阶段。

**需求来源：** `AGENTS.md`
**实施模式：** 单人串行开发，原 28 周功能基线，分 3 个可验证版本；阶段 0 须按本计划的生产化范围重新基线化排期
**发布定义：** Release A 为内部测试版，Release B 为预发布 Beta，Release C 为生产 GA；A/B 不得视为生产就绪
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
- `internal/biz`、`pkg/agent`、`pkg/memory` 行覆盖率不得低于 80%，鉴权与租户隔离目标不低于 90%；覆盖率不能替代关键路径与攻击场景断言。
- 应用按 API、Worker、Migration 角色运行同一镜像；API/Worker 无状态且可水平扩容，禁止依赖容器本地磁盘保存业务事实。
- 持久化任务采用至少一次投递，消费者必须幂等；任务租约、重试、退避、死信和 Outbox 调度不得依赖单机内存。
- 所有创建 Run、执行有副作用工具及外部事件处理必须支持幂等键或业务去重键，禁止以“网络重试”制造重复副作用。
- 生产密钥来自环境注入的 Secret Manager/KMS，禁止写入镜像、仓库或 Compose 明文；密文携带密钥版本并支持轮换。
- 数据库变更采用 Expand → Migrate → Contract，保证新旧应用并存期间的兼容性；回滚优先前滚修复，禁止以破坏性 `down` 迁移作为常规发布回滚。
- 全链路使用 UTC `timestamptz` 与单调时钟计算超时；所有超时、重试、分页、请求体和队列批次必须有上限。
- Docker Compose 仅作为开发与验收环境；生产拓扑必须包含负载均衡、至少两个应用副本，以及具备高可用与备份能力的 PostgreSQL/Redis。
- 开发、预发布、生产环境隔离账号、网络、数据库和密钥；生产数据不得复制到非生产环境，调试使用脱敏或合成数据。

## 生产级非功能验收基线

以下默认值在阶段 0 与业务方确认后可调整，调整必须记录 ADR；未量化、未演练或存在阻断项时不得 GA。

- **可用性与性能：** 核心 API 月可用性不低于 99.9%，系统原因错误率低于 1%；在确认的目标峰值下，非 LLM API p95 不高于 500ms。LLM 首 Token、完整生成和工具耗时单独统计，不用外部模型延迟掩盖系统指标。
- **流式链路：** 请求受理至首个持久化 `meta` 事件 p95 不高于 1 秒；断线后可按事件游标续传，重连不得重复执行 Run 或丢失终态。
- **容量基线：** 100 并发 SSE 仅为最低验证值；阶段 0 根据预期用户、消息长度、Provider 限额和工具并发建立容量模型，GA 前通过 1.5 倍目标峰值负载测试及长时间稳定性测试，无 Goroutine/连接/内存泄漏和无界积压。
- **灾备目标：** 初始 RPO 不超过 5 分钟、RTO 不超过 60 分钟；GA 前必须从独立备份实际恢复到干净环境并校验数据、RLS、向量和任务一致性。
- **安全门禁：** 无未处置的 Critical/High 漏洞，无硬编码密钥，无跨用户数据泄露；鉴权、RLS、SSRF、Prompt Injection、工具越权、Webhook 重放和供应链检查全部通过。
- **可靠性门禁：** PostgreSQL、Redis、LLM、MCP、飞书故障及 API/Worker 滚动重启均完成故障注入；系统可降级或明确失败，不出现静默丢消息、重复副作用或永久卡住的活动 Run。
- **Agent 质量门禁：** 建立版本化黄金集与对抗集，覆盖三种模式、工具参数、循环终止、记忆相关性/冲突、摘要忠实度、跨用户隔离和注入防护；阈值在阶段 0 固化，GA 前不得相对已批准基线回退。
- **数据治理：** 用户可导出并删除其数据；删除覆盖 PG、向量、缓存、外部绑定及异步任务，备份按保留周期最终清除；所有管理操作与删除任务可审计。
- **运维门禁：** SLO 看板、告警、值班责任人、Runbook、事件分级与复盘流程齐全；每个 Critical/High 告警都有可执行处置步骤和升级路径。

## 目标架构

```text
api/harness/v1/          Protobuf、HTTP/gRPC 契约
cmd/server/              API、Worker、静态资源统一进程入口
cmd/migrate/             数据库迁移命令
configs/                 非密 YAML 基线及 dev/staging/production 覆盖配置
internal/biz/            聚合、领域服务、Repository 接口
internal/data/           Gorm、Redis、外部服务实现
internal/service/        Protobuf Service、鉴权上下文转换
internal/server/         HTTP、gRPC、SSE、静态文件和中间件
internal/platform/       auth、crypto、jobs、observability、audit、redaction、ratelimit、config
pkg/agent/               Eino 适配与三种 Agent Runner
pkg/agent/model/         模型目录、1M+ 准入、路由、健康与故障转移
pkg/memory/              上下文预算、摘要、向量和事实记忆
pkg/feishu/              飞书验签、身份、话题绑定、事件和回复适配
migrations/              PostgreSQL/pgvector 版本化迁移
web/                     React 前端
deploy/                  开发 Compose、生产拓扑、监控、告警与备份恢复示例
tests/                   集成、契约、E2E、负载、故障注入、迁移与 Agent 质量评测
```

核心 API 使用 `/api/v1` 前缀，覆盖认证、平台配置、Agent、Session、Message、Run、Execution、Audit、Plan、Memory 与飞书事件。聊天接口使用 SSE，事件类型固定为 `meta`、`message.delta`、`model.attempt.started`、`model.attempt.failed`、`model.switched`、`tool.started`、`tool.completed`、`plan.updated`、`worker.started`、`worker.completed`、`run.completed`、`run.failed`、`heartbeat`。

API 统一使用稳定业务错误码、`request_id`、`retryable` 语义和游标分页；列表页大小、请求体、批量操作和并发均有上限。写接口定义 `Idempotency-Key` 的作用域、指纹和保留期，冲突请求返回原结果或明确冲突。Protobuf 通过 Buf Breaking 检查兼容性是 CI 阻断项，契约废弃必须有迁移期。SSE 事件包含 `event_id`、`run_id`、单调 `seq`、`schema_version`、`occurred_at`，客户端以游标重放；浏览器使用 Fetch 流或安全 Cookie 鉴权，禁止在 URL 中携带 Token。

核心数据表包括：`users`、`user_identities`、`refresh_tokens`、`llm_providers`、`llm_models`、`model_policies`、`model_policy_candidates`、`skills`、`mcps`、`agents`、`agent_skills`、`agent_mcps`、`sessions`、`session_members`、`messages`、`agent_runs`、`execution_steps`、`execution_payloads`、`run_events`、`plans`、`plan_steps`、`session_summaries`、`memory_vectors`、`memory_facts`、`external_conversation_bindings`、`feishu_event_receipts`、`jobs`、`outbox_events`、`idempotency_keys`、`model_calls`、`deletion_requests`、`audit_logs`。

生产数据约束必须落实到迁移与模型测试：

- `jobs` 具有 `dedupe_key`、状态、尝试次数、`available_at`、租约属主/到期时间、fencing token、最后错误和死信信息；`outbox_events` 与业务写入同事务提交。
- `run_events` 对 `(run_id, seq)` 建唯一约束并长期保留至对应 Run 删除；`agent_runs` 固化 Agent、Prompt、Skill、MCP、模型与记忆策略版本快照。
- `execution_steps` 对 `(run_id, step_seq)` 建唯一约束；`execution_payloads` 显式携带 `user_id` 并启用 RLS，内容字段受大小、Capture Mode 和 Redaction Policy 约束。
- `audit_logs` 使用追加型分区、白名单元数据和 Hash Chain；业务角色禁止更新/删除，Checkpoint 签名与外部副本用于独立验链。
- `refresh_tokens` 支持 Token Family、轮换和重用检测；`users` 支持禁用、凭据版本和全端会话撤销。
- `llm_models` 记录 Provider Deployment、上下文窗口、能力和数据区域；`model_policies` 与 `model_policy_candidates` 保存版本化路由策略，`agents` 引用策略版本。
- `memory_vectors` 记录 Embedding Provider、模型、维度与版本，不同版本不得混用同一索引；`model_calls` 按 `run_id + attempt_no` 记录模型候选、Request Hash、Token、费用、延迟、切换原因和脱敏错误分类。
- 所有外键、唯一约束、删除策略、常用查询索引和 JSON Schema 均有迁移测试；禁止无界 JSON、文本、数组及无 TTL 的 Redis Key。

## 模型管理、1M+ 准入与智能故障转移

### 模型目录与路由策略

- `llm_providers` 只管理连接、凭证和 Provider 能力；`llm_models` 管理具体 Provider Deployment，包括远端模型名、上下文窗口、最大输出、工具/流式/结构化输出能力、输入输出价格、数据区域和启停状态，二者不得混为一个配置对象。
- `model_policies` 定义可复用的路由规则，`model_policy_candidates` 保存有序候选及单候选约束。Agent 引用策略版本，Run 固化候选、规则和模型元数据快照，运行中修改策略不改变在途 Run。
- “智能切换”采用**确定性规则 + 共享健康状态**，不额外调用 LLM 决定模型。先按优先级选择健康候选，再结合熔断状态、上下文适配、配额、数据区域和成本约束路由，保证行为可解释、可测试、可回放。
- Redis 保存多副本共享的熔断、半开探测和短期健康状态；PostgreSQL 的 `model_calls` 保存每次 Attempt 的事实。Redis 故障时只允许使用明确配置的静态候选顺序，禁止因健康缓存缺失而绕过准入或无限重试。

### 1M+ 上下文硬准入

- Chat、ReAct、Planner、Supervisor 等生成模型的 `context_window_tokens` 必须大于等于 `1_000_000`；该约束不适用于独立 Embedding/Rerank 模型。数据库按 `model_kind` 执行条件 CHECK，并与 Biz 校验、启动策略校验共同阻断不合规模型，不能仅依赖前端提示。
- 低于 1M 的模型不得启用或加入新策略；已有低上下文模型只能保留历史 Run 快照，不得创建新 Run。候选切换也只能选择同属 1M+ 且满足当前 Agent 工具、流式、JSON 和合规能力的模型。
- 上下文窗口来自经管理员核验的模型元数据，并记录来源与核验时间；OpenAI-compatible 自动发现只能作为辅助，不能根据模型名称猜测，也不能把未核验的自报值直接作为准入依据。
- 1M 是能力下限，不是每次请求的填充目标。实际输入预算为 `min(context_window - reserve_output - safety_margin, policy.max_prompt_tokens, Run 剩余 Token/费用预算)`；滑动窗口、摘要和长期记忆仍然保留，避免超长上下文带来的成本、延迟和注意力稀释。

### 路由与故障转移规则

1. 请求前执行硬过滤：模型启用、1M+、能力匹配、输入适配、数据区域/合规允许、用户与 Provider 配额可用；无候选时快速失败并给出可诊断原因。
2. 默认按策略中的显式优先级选择首个健康候选，同一个 Run 保持模型粘性；只有符合切换规则时才进入下一候选。熔断按 `provider_id + model_id` 隔离，连续失败达到阈值后打开，冷却后仅允许少量半开探测恢复。
3. 每次调用生成单调 `attempt_no`，在调用前持久化模型、Request Hash、上下文快照版本和策略版本；结束后记录 Token、费用、延迟与错误分类。默认单模型最多 2 次 Attempt、每 Run 总 Attempt 不超过 6 次、最多切换 2 次，并受 Run 总 Deadline 约束。
4. 模型切换通过 `model.attempt.started`、`model.attempt.failed`、`model.switched` 事件和执行树展示原模型、目标模型及脱敏原因；不得静默换模型或隐藏额外费用。

| 失败/状态 | 同模型重试 | 切换策略 | 约束 |
|---|---|---|---|
| 连接错误、超时、可重试 5xx | 指数退避加抖动，最多 1 次 | 仍失败则切换下一健康候选 | 不突破 Run Deadline 和 Attempt 上限 |
| 429/Provider 配额耗尽 | 仅在 Retry-After 可接受时等待 | 立即或等待预算耗尽后切换 | 按 Provider/Model 熔断，防止重试风暴 |
| 401/403、Endpoint 配置失效 | 不重复当前 Endpoint | 可切换另一个合规 Provider | 当前 Endpoint 熔断并告警，不伪造成功 |
| 400、Schema/工具参数错误、内容审核拒绝、用户取消 | 否 | 否 | 修正请求或终止，禁止用切换绕过安全和格式校验 |
| 实际上下文超出模型窗口 | 否 | 仅当更大候选能容纳完整请求时切换 | 否则先按确定性规则压缩或明确失败 |
| 已向客户端发送流式 Delta 后中断 | 不直接续写拼接 | 默认不自动切换 | 保留部分输出并明确失败；新 Attempt 必须从规范上下文重新生成并标记，已完成工具不得重放 |

- 只有尚未产生外部副作用的模型 Attempt 才能自动重试/切换。已完成的工具调用、PlanStep 和外部回复从持久化结果恢复，禁止因更换模型再次执行。
- 预发布必须覆盖连续失败、429/Retry-After、跨 Provider 熔断与半开、流式中途失败、上下文超限、策略无候选、费用上限和切换后工具幂等测试。

## Agent 执行记录、监控日志与审计设计

### 执行事实、应用观测与安全审计分离

| 数据链路 | 主要内容 | 存储与用途 | 内容约束 |
|---|---|---|---|
| Agent 执行事实 | 模型请求/响应、工具输入/输出、Plan/Worker、Attempt、错误与状态迁移 | PostgreSQL 的 `agent_runs`、`execution_steps`、`execution_payloads`、`model_calls`、`run_events`，用于执行树、排障、恢复和导出 | 按用户隔离，按策略保存脱敏内容，支持截断、保留和删除 |
| 应用观测 | Trace、Metric、结构化日志、健康与告警 | OTel、Prometheus、日志平台，用于 SLO 和系统运维 | 默认只放关联 ID、状态、耗时和脱敏错误，不放 Prompt、Completion 或工具原文 |
| 安全审计 | 谁代表谁，在何时对何资源执行何操作，结果与原因 | 追加型 `audit_logs`，用于安全、合规与追责 | 只记录必要元数据和白名单差异，不复制执行载荷，独立保留与防篡改 |

执行记录是受 RLS 保护的业务数据，不是 stdout 日志。任何执行内容进入 PostgreSQL、SSE、日志平台或导出文件前都必须经过统一 Redaction Pipeline；OTel 的“不采集原文”不妨碍受控执行记录按策略保存脱敏请求/响应。

### 关联标识、顺序与数据模型

- 全链路传播并保存 `request_id`、`trace_id`、`run_id`、`parent_run_id`、`session_id`、`step_id`、`attempt_no`、`tool_call_id`、`event_seq`、Provider Request ID、外部 message_id 和幂等键；Worker/飞书/异步 Job 不得生成一套无法关联的新身份。
- `(run_id, event_seq)` 与 `(run_id, step_seq)` 单调递增并唯一，模型 Attempt 对 `(run_id, attempt_no)` 唯一，工具尝试对 `(run_id, tool_call_id, attempt_no)` 唯一。排序使用序号，不能以精度不足的 `created_at` 推断执行顺序。
- `model_calls` 保存模型/策略版本、状态、Request Manifest、请求/响应 Payload 引用、TTFT/总耗时、Token/费用、Finish Reason、Provider Request ID、切换原因和脱敏错误。
- `execution_steps` 保存 `model/tool/plan/worker/system` 类型、状态、开始/结束时间、工具及 Schema 版本、风险等级、审批记录、幂等键和重试关系。
- `execution_payloads` 保存 `run_id`、`step_id`、`user_id`、Payload 类型、JSON 或文本内容、Content-Type、Capture Mode、Schema/Redaction 版本、原始大小、保存大小、`truncated`、脱敏后 Hash 和创建时间；私有执行记录显式携带 `user_id` 并启用 RLS。
- `run_events` 只负责可重放的时间线和 SSE，不重复存储完整模型/工具载荷；`messages` 保存规范对话内容，执行 Payload 保存某次 Attempt 实际收发的调试事实，二者不得混用。

### 模型请求/响应记录

1. 调用模型前先创建 `model_call=running` 和对应 Step，持久化 Request Manifest、脱敏请求及模型参数；意图记录失败时不得发起外部调用。
2. Request Manifest 包含有序 Message ID/版本、System Prompt/Skill/工具 Schema 版本、记忆摘要与事实快照、模型参数和各部分 Hash。小请求可保存完整脱敏 JSON；1M 级请求默认保存 Manifest、版本化引用和脱敏预览，避免每个 Attempt 重复复制超长上下文，同时仍能精确重建请求。
3. 流式 Delta 通过既有 Run Event 持久化，不逐 Token 写 `execution_payloads`；完成时聚合保存最终响应，失败/取消时保存已产生的部分响应并明确 `partial=true`。
4. 响应只保存最终文本、Refusal、工具调用、Finish Reason、Usage 和可公开的 `reasoning_summary`；Provider 返回的隐藏思维链、加密思维或其他原始推理字段必须在适配层丢弃，不能进入任何执行 Payload。
5. 模型请求不保存 Authorization/Cookie 等请求头，只保存模型、消息、工具定义和采样参数；Provider 原始错误响应先脱敏，再按错误分类保存，不直接透传上游可能包含的敏感回显。

### 工具调用与返回记录

- 工具执行前先持久化 Tool Call ID、工具/Schema 版本、脱敏参数、风险等级、审批/策略决定、幂等键和 `started` 状态；没有意图记录不得产生副作用。MCP、Sandbox、飞书回复及其他外部副作用统一复用该记录链路。
- 工具结束后先保存脱敏返回值或错误、执行耗时、退出/HTTP 状态和 `truncated`，再推进 Step 并发送 `tool.completed`；工具返回的 Prompt Injection 内容只作为不可信数据保存，不能转写成系统指令。
- 文本/JSON 按工具和字段配置大小上限；超限保存确定性截断预览、结构摘要、大小和 Hash，并显式标记截断。文件、图片和二进制只保存元数据、Checksum 与受控内容引用，不直接写入日志或 JSON。
- 工具 Schema 增加字段级 `public/internal/sensitive/secret` 分类和 Capture Policy。`secret` 永远移除，`sensitive` 默认仅保存元数据，`internal/public` 按 `metadata_only` 或 `redacted` 策略保存；未声明字段按更严格级别处理。
- 如果工具已产生副作用但结果无法持久化，Run 进入 `reconciliation_required`，禁止自动盲重试并触发告警；恢复时依据幂等键/外部查询对账，不能假定工具失败。

### 脱敏、完整性与保留

- Capture Mode 至少支持 `metadata_only` 和 `redacted`，默认 `redacted`；不存在“未脱敏完整原文”模式。模型、工具、Memory 和飞书可设置更严格策略，但不能放宽全局 Secret/隐藏思维链禁令。
- Redaction Pipeline 同时覆盖凭证字段、连接串、私钥、Token/Cookie、配置中的敏感 JSON Path 和已识别的个人信息；未知序列化格式或脱敏失败时只保存元数据与 `capture_error`，不得降级写入原文。
- Payload Hash 对规范化且已脱敏的内容计算，用于完整性/去重，不保存可能泄露低熵秘密的原始内容 Hash。截断、缺失、部分响应和脱敏失败必须在 UI/API 中可见，不能伪装成完整记录。
- 执行元数据、执行 Payload 和审计分别配置保留期；用户删除 Session/账户时同步删除其执行内容。审计仅在合规要求下保留必要的去内容化/假名化记录，到期物理删除，备份按既有周期最终清除。

### 审计事件与防篡改

- 审计覆盖：登录/令牌重用/改密/禁用；角色与配额变更；Provider/Model/RoutePolicy/Skill/MCP/Agent 配置和凭证轮换；Run 创建/取消/人工重试；工具审批、拒绝和高风险副作用；Memory/Session/话题导出、解绑和删除；死信重放；执行 Payload 查看/导出；迁移、备份恢复和保留策略变更。
- `audit_logs` 字段至少包括 `event_id`、`schema_version`、`partition_id`、`partition_seq`、`occurred_at`、`actor_type/ref`、`on_behalf_user_ref`、`action`、`resource_type/id`、`outcome`、`reason`、`request_id`、`trace_id`、`run_id`、来源、白名单元数据/差异、`prev_event_hash`、`event_hash` 和签名版本。用户 Actor 从写入时使用带密钥的假名引用，身份映射独立保存，避免删除账户时回写审计历史。
- 用户、管理员、Agent、Worker、System 使用不同 Actor；Agent 自动操作必须同时保存发起用户引用和 Run，不能只记录一个无法追责的“system”。凭证变更前后值只记录为 `REDACTED`，审计表不保存模型/工具原文。
- 关键管理与副作用操作的业务写入和审计在同一 PostgreSQL 事务提交，审计失败则阻断操作；普通安全事件通过持久化 Outbox 按 `event_id` 幂等补记。审计不是可丢失的异步日志。
- 应用业务角色对 `audit_logs` 只有 Append/受控读取权限，禁止 UPDATE/DELETE；只有独立 Retention 角色可执行到期删除。按分区内 `partition_seq` 计算 Hash Chain，并定期生成带 KMS 签名的范围 Checkpoint，将 Checkpoint 复制到独立安全位置，支持启动和定期验链。

### 查询、监控界面与告警

- 提供 Run 执行列表与时间线 API、单 Step Payload 受权读取、审计游标查询和异步导出；支持按 Session、Agent、Run、模型、工具、状态、错误类别和时间过滤，所有接口使用 RLS、权限检查和游标分页。
- Web 执行树展示 Run → Step → Model/Tool Attempt，支持查看脱敏请求/响应、工具输入/输出、模型切换、重试、Token/费用、TTFT、耗时、截断/脱敏状态和关联 Trace；Payload 使用纯文本/JSON Tree 渲染且禁止原始 HTML，隐藏思维链不得出现在详情或导出中。
- 普通用户只能查看自己的 Run；群话题按 Session Membership 授权。运维/管理员默认只看元数据，读取用户执行内容需要独立权限、填写理由并产生审计，必要时采用限时 Break-Glass 授权。
- 导出任务记录申请人、范围、格式、保留期限和下载审计，导出内容再次脱敏并加密；调试重放默认使用快照重建上下文且 Mock 所有副作用工具，不得从日志直接重放真实副作用。
- 指标和告警覆盖执行记录写入失败、脱敏失败、Payload 截断率、长期 Running Step、`reconciliation_required`、审计写入失败、Hash Chain 校验失败和导出异常；这些标签不使用 user/run/session 等高基数维度。

## 飞书群话题会话设计

### 一话题一 Session

群聊不能再以 `chat_id` 单独映射 Session。解析优先级为 `thread_id` → `root_id/parent_id` 绑定 → 新话题；`chat_id` 只表示群边界，`thread_id` 表示会话边界。

| 场景 | 外部会话键 | 处理 |
|---|---|---|
| 话题群中的新话题 | `provider + tenant_key + chat_id + thread_id` | 创建一个 Session，后续消息全部复用 |
| 普通群主时间线 @机器人提问 | 暂用 `provider + tenant_key + chat_id + root_message_id` | 创建 Session 并以根消息建立话题回复；获得 thread_id 后补充别名 |
| 话题中的追问/回复机器人卡片 | 先查 thread_id，再查 root/parent 别名 | 进入原 Session，不新建 Run 以外的会话 |
| 另一个话题，即使是同一群同一发言人 | 新的 thread_id/root_message_id | 新建独立 Session，禁止复用群级上下文 |

- `external_conversation_bindings` 至少保存 `provider`、`tenant_key`、`chat_id`、`thread_id`、`root_message_id`、`session_id`、创建者、Agent 版本和状态；对已解析话题建立 `(provider, tenant_key, chat_id, thread_id)` 唯一约束，对非空 `root_message_id` 建立对应唯一约束。
- 首条消息创建 Session、绑定、成员和初始标题必须在同一事务中完成，依靠唯一约束解决并发首消息/重复事件；不得先查后插而不加约束。
- 话题创建后机器人卡片和所有增量/最终回复都发送到同一 thread；回复成功或失败均记录外部 message_id 与业务去重键，恢复后仍定位原话题。
- 事件乱序时可通过 root/parent/message 查询补齐绑定，并进行有界等待与重试；无法定位时保留原始事件并提示用户从话题重新提问，绝不回退到“整个群一个 Session”。

### 触发、上下文与生命周期

- 主时间线仅在 @机器人或显式命令时创建话题；已绑定话题默认自动回复，可按群配置为“仅 @或回复机器人卡片时触发”，避免把成员闲聊误当成 Agent 请求。
- 每个话题拥有独立消息窗口、摘要、Run、Plan、取消和配额状态；同一群内多个话题可并行，但按 `chat + thread + user` 限流。切换 Agent 或“重新提问”显式创建新 Session，并记录来源 Session，不能污染原话题历史。
- 群话题不读取成员个人长期记忆；每条消息保留发言人内部 user_id/open_id，个人事实只写发言者私有记忆。只有显式启用且归属同一 chat 的群级共享记忆才能跨话题检索。
- 支持关闭、归档、恢复、解绑和删除话题；关闭后迟到事件不得重启 Run，删除继续遵循 PG、向量、缓存、绑定和任务的一致清理。
- 优先使用飞书话题群/线程能力。普通群若平台或机器人权限不支持话题回复，只能按配置使用 `root_id` 回复链降级或拒绝该模式，并在管理端显示能力状态；不得把普通群消息伪装成具有完整话题隔离。
- 测试必须覆盖同群多话题并行、root/thread 别名竞态、首消息重复、事件乱序、跨话题记忆隔离、话题内取消/Plan 恢复、回复串题和普通群能力降级。

## YAML 配置与动态模型配置边界

应用增加以下非密配置结构；本阶段只纳入实施计划，工程初始化时创建真实文件：

```text
configs/
├── config.yaml              # 全环境安全默认值
├── config.dev.yaml          # 开发环境覆盖
├── config.staging.yaml      # 预发布环境覆盖
└── config.production.yaml   # 生产非密覆盖，不保存真实密钥
```

加载顺序为 `config.yaml` → 当前环境 YAML → 环境变量/Secret Manager 注入；启动时拒绝未知字段、非法 Duration/数值、缺失密钥引用和生产不安全配置。YAML 变更通过版本控制和发布生效，Provider、模型目录、候选池和 Agent 策略通过 PostgreSQL 管理 API 动态变更，不在 YAML 维护易变模型清单。

```yaml
server:
  http:
    addr: 0.0.0.0:8080
    timeout: 15s

data:
  postgres:
    dsn: ${DATABASE_URL}
    max_open_conns: 30
  redis:
    addr: ${REDIS_ADDR}
    password: ${REDIS_PASSWORD}

agent:
  model_routing:
    min_context_tokens: 1000000
    max_prompt_tokens: 300000       # 示例预算，按成本与延迟目标调整
    reserve_output_tokens: 16384
    safety_margin_tokens: 4096
    max_attempts_per_model: 2
    max_attempts_per_run: 6
    max_switches_per_run: 2
    circuit_breaker:
      failure_threshold: 3
      window: 30s
      open_for: 30s
  execution_records:
    default_capture_mode: redacted
    max_inline_payload_bytes: 262144
    retain_partial_response: true

observability:
  include_execution_payloads: false

audit:
  synchronous_critical_writes: true
  hash_chain_enabled: true

feishu:
  group:
    require_topic: true
    auto_reply_in_bound_topic: true
    allow_root_reply_fallback: false
```

`${DATABASE_URL}`、`${REDIS_PASSWORD}` 等仅表示运行时注入引用；真实密钥不得提交到任何 YAML、仓库或镜像。环境文件只保存非密差异，模型凭证继续使用加密字段和 Secret Manager/KMS。

## 分阶段任务

### 阶段 0：工程基线与技术验证（第 1–2 周）

- [ ] 初始化 Git、Go Module、Kratos v3、Buf、React/Vite 和 Makefile。
- [ ] 建立代码生成、lint、单测、集成测试、契约测试、前端测试和镜像构建命令。
- [ ] 编写认证、隔离、Agent Runtime、任务、SSE、执行记录、审计/脱敏、密钥管理、数据保留、兼容迁移与发布回滚 ADR。
- [ ] 完成 STRIDE/滥用场景威胁建模，明确信任边界、工具风险分级、人工审批点与 Prompt Injection 防护策略。
- [ ] 确认 SLO、RPO/RTO、目标峰值、数据合规与 Agent 黄金集阈值，建立容量和成本模型，并重新基线化实施排期。
- [ ] 建立 PostgreSQL/pgvector、Redis、Jaeger、Prometheus 的 Docker Compose；补充生产参考拓扑，明确 Compose 不用于生产高可用承诺。
- [ ] 完成 Kratos、Eino、SSE、pgvector 与前端 embed 的最小技术验证，验证多副本、Worker 租约和 SSE 代理超时配置。
- [ ] 建立 `configs/config.yaml` 与 dev/staging/production 非密覆盖配置，实现环境变量展开、严格反序列化、启动校验和密钥引用检查；提供 Fake LLM/MCP/飞书服务、固定时钟和可控故障注入，CI 不依赖真实外部付费 API。
- [ ] 建立 CI 与分支保护：生成物一致性、Buf Breaking、SAST、依赖/密钥/IaC/镜像扫描、许可证检查、测试与构建全部为必过检查。
- [ ] 验证 startup/readiness/liveness、优雅退出、迁移、前端占位页及镜像构建；基础镜像与依赖固定精确版本或 Digest。

### 阶段 1：数据库、鉴权与隔离基座（第 3–5 周）

- [ ] 编写身份、配置、会话、执行、记忆、任务和审计迁移，补齐外键、唯一/检查约束、索引、删除策略与数据分类。
- [ ] 实现 Gorm 通用字段、事务、游标分页与错误映射；配置连接池、语句/锁/idle-in-transaction 超时和慢查询日志。
- [ ] 建立 Expand → Migrate → Contract 工具链，测试空库升级、版本间升级、新旧代码并行、在线建索引、失败重入与前滚修复。
- [ ] 实现用户 Scope、RLS 与跨用户攻击测试；应用使用非超级用户且禁止 `BYPASSRLS`，私有表启用 `FORCE ROW LEVEL SECURITY`，连接池复用前清理租户上下文。
- [ ] 实现注册、登录、刷新、注销、Argon2id 与 Token 轮换；补充登录防爆破/账户枚举防护、一次性短期改密/找回凭据、禁用用户、全端撤销、JWT `kid` 轮换和 Refresh Token 重用检测；明确开放/邀请注册与首个管理员安全引导，生产禁止默认口令。
- [ ] 实现 Principal、最小权限 RBAC、用户/IP/全局多维限流、管理操作审计与敏感字段脱敏；只信任显式配置代理传递的客户端 IP，审计日志采用追加写入、Hash Chain 和独立保留策略。
- [ ] 建立统一 Redaction Pipeline、字段分级、执行 Payload RLS 和受控读取权限；关键管理操作的业务与审计同事务提交，秘密字段在任何日志/审计输出前脱敏。
- [ ] 实现 AES-256-GCM 密钥版本、Secret Manager/KMS 接入、启动密钥检查与不停机轮换流程；轮换期间可解旧密文，后台重加密可恢复且可审计。
- [ ] 建立 Repository Mock 与 Biz 测试模板；使用真实 PostgreSQL 非特权角色运行 RLS 集成测试，覆盖连接池、事务嵌套与并发访问。

### 阶段 2：平台配置、Agent 与会话领域（第 6–8 周）

- [ ] 实现 Provider、Skill、MCP 管理员 CRUD、凭证加密与连接测试；凭证只写不读，更新、轮换、禁用和删除均审计且不影响进行中 Run 的快照。
- [ ] 实现独立模型目录、模型能力元数据、1M+ 硬准入、路由策略和有序候选管理；数据库 CHECK、Biz 与启动校验共同阻止低上下文生成模型进入新 Run。
- [ ] 实现 OpenAI-compatible Chat/Embedding 适配器，校验模型能力、上下文窗口、流式/工具支持和向量维度，统一脱敏错误、用量与 Provider Request ID；自动发现结果必须经管理员核验后启用。
- [ ] 实现 Agent 聚合、模式配置、Skill/MCP 绑定和配置快照；草稿与已发布版本分离，Run 只引用不可变版本，支持灰度切换与快速回退。
- [ ] 实现 Session、Message、Run 聚合与状态机，定义所有合法迁移、终态、失败原因和并发冲突；非法迁移不得被异步任务绕过。
- [ ] 定义 MCP Schema、权限等级、超时、审计与 SSRF 防护；覆盖 DNS Rebinding、重定向、IPv4/IPv6 内网/元数据地址、响应大小和解压上限。
- [ ] 完成 Protobuf、HTTP/gRPC、OpenAPI 与前端类型生成；统一错误模型、游标分页、请求上限和幂等语义，并加入服务端/浏览器契约测试。
- [ ] 实现用户级 Agent 配额、Session/Message 数量与长度上限、管理员平台配额和配置变更并发控制，防止资源与存储无界增长。

### 阶段 3：ReAct、工具与流式执行（第 9–11 周）

- [ ] 建立 Runner、ModelProvider、ToolRegistry 抽象与 Eino 适配；Prompt/模型参数/工具 Schema 版本化，模型输出和工具参数严格校验。
- [ ] 实现最多 12 步、工具 60 秒、Run 5 分钟的 ReAct Runner，限制输入/输出 Token、工具结果大小和单 Run 总费用。
- [ ] 实现工具调用 Hash 与连续重复 3 次终止规则；循环检测按 Run 隔离，重启后仍可判断。
- [ ] 实现 Run/Step/Event 与 Outbox 同事务持久化后再推送的 SSE 链路，确保数据库提交失败不产生“幽灵事件”。
- [ ] 打通执行记录链路：模型调用前保存 Request Manifest/脱敏请求，完成或失败时保存完整/部分响应；工具执行前保存脱敏参数与审批意图，结束后先保存脱敏返回再推进状态，并贯穿关联 ID 和单调序号。
- [ ] 实现事件游标、`Last-Event-ID`/显式游标重放、心跳、最大连接时长、慢消费者背压和断连清理；客户端重连只补事件，不重建 Run。
- [ ] 实现取消、优雅关闭、重启恢复、幂等和上游超时映射；Worker 从持久化 Run 校验并注入用户/RLS 上下文，使用租约心跳与 fencing token，过期 Worker 不得提交结果。
- [ ] 实现持久化任务的指数退避加抖动、最大重试、死信/人工重放和重复消费测试；所有 Job、Outbox、摘要和外部回调节点遵循至少一次语义。
- [ ] 为 LLM/Embedding 实现分层超时、有限重试、熔断、舱壁与全局/用户/Provider 并发限制；按模型路由规则实现 Attempt 持久化、Run 粘性、共享熔断、半开恢复和受控候选切换，429/5xx 遵循 Retry-After 且有上限。
- [ ] 记录每次模型 Attempt 的 Token、估算费用、切换原因与配额占用，支持用户/Agent/Provider 预算、超限拒绝和告警；切换候选必须满足 1M+、能力和合规约束，流式已输出时禁止静默拼接另一模型结果。
- [ ] 远程 MCP 限制为管理员允许的 HTTPS Endpoint，并默认阻止内网地址；有副作用或高风险工具支持参数确认/人工审批与幂等键，未获批准不得执行。
- [ ] 将工具返回与外部内容标记为不可信数据，实施指令/数据隔离、敏感信息最小注入和外发目标限制；通过间接 Prompt Injection 与数据外泄对抗测试。

### 阶段 4：分层记忆引擎（第 12–14 周）

- [ ] 实现 Token 预算与 Redis 滑动窗口；PG 可重建缓存，设置 Key TTL、内存上限与淘汰策略，禁止缓存空值造成长期错误。
- [ ] 实现带来源范围与版本的异步摘要任务，处理消息新增/删除后的失效、任务乱序、重复执行和旧版本覆盖。
- [ ] 实现 1536 维 embedding、HNSW cosine、Top-30 与 MMR Top-8；将模型/维度/版本纳入索引元数据，提供不停机重建、双写校验、回填和切换流程。
- [ ] 实现结构化事实、冲突更新、来源追踪、有效期和删除；禁止低置信度事实静默覆盖用户明确纠正。
- [ ] 按 System → 长期记忆 → 摘要 → 最近消息 → 当前输入组装上下文，预算不足时定义确定性裁剪顺序并记录裁剪原因。
- [ ] 实现 Redis、向量和摘要故障时的明确降级；任何降级不得扩大检索范围、使用他用户缓存或把陈旧摘要冒充最新状态。
- [ ] 建立记忆质量评测：检索 Precision/Recall、MMR 多样性、摘要忠实度、事实冲突与纠正、群聊发言人归属、删除后不可召回及跨用户零泄漏。
- [ ] 实现 Memory/Session 导出与异步删除编排，清理 PG、向量、缓存、摘要、外部绑定和关联任务；失败可重试、状态可查、结果可审计。

### 阶段 5：Web 产品闭环与 Release A（第 15–17 周）

- [ ] 实现认证与受保护路由，覆盖刷新并发、Token 重用、注销、禁用用户和全端撤销；浏览器不持久化 Access Token，Cookie 使用 HttpOnly/Secure/SameSite 并配套 CSRF 防护。
- [ ] 实现 Provider、模型目录、路由策略、Skill、MCP 管理页面；展示上下文窗口核验、候选顺序、健康/熔断和切换记录，敏感字段只写不读，危险操作二次确认。
- [ ] 实现 Agent、Session、对话及 Markdown/代码渲染；默认禁用原始 HTML 并清洗 XSS，链接、图片和复制内容遵循安全策略。
- [ ] 实现 Run 执行监控与审计页面：按 Run/Step/Attempt 展示脱敏模型请求响应、工具 I/O、耗时、Token/费用、重试/切换、截断状态和 Trace；支持审计查询、受控导出和 Payload 查看理由审计。
- [ ] 实现 SSE 增量、按 `seq` 去重、游标重连、取消、有限重试和执行树；终态、断线、重放与多标签页不得造成重复消息或重复 Run。
- [ ] 配置严格 CORS 白名单、CSP、`nosniff`、Frame/Referrer 等安全头；Cookie 鉴权的写操作验证 CSRF，SSE 不在 URL 暴露 Token。
- [ ] 覆盖加载、空态、错误、限流、配额、审批和降级状态；核心流程满足键盘操作、可访问名称、对比度和焦点管理要求，并完成浏览器 E2E。
- [ ] 将 Web 静态资源 embed 到 Go 二进制并支持 SPA 回退；带 Hash 静态资源长期缓存，`index.html` 禁止长期缓存，版本升级不得混用不兼容资源。
- [ ] 完成非 root 单应用镜像、只读根文件系统兼容、资源限制和健康检查；Release A 仅在内部测试环境发布并通过核心链路回归，不作为生产 GA。

### 阶段 6：Plan-and-Execute（第 18–20 周）

- [ ] 实现 Plan 与 PlanStep 状态机，覆盖等待审批、暂停、部分失败和人工终止，禁止终态被迟到事件回写。
- [ ] Planner 输出执行 JSON Schema 校验，最多 50 步，并限制依赖深度、总输入/输出、执行时长和费用。
- [ ] 每步事务性持久化并支持失败重试、取消、继续和重启恢复；副作用步骤使用业务幂等键，非幂等步骤失败不得自动盲重试。
- [ ] 使用乐观锁与 fencing token 阻止多 Worker/过期 Worker 重复推进，并在 SSE/Web 中展示状态、尝试次数与失败原因。
- [ ] 验证步骤乱序、重复、依赖失败、配置版本升级和进程崩溃后的恢复；继续执行沿用原 Plan 快照，人工修改生成新版本。
- [ ] 将 Plan 成功、步骤重试、恢复、取消、工具成功率和成本纳入黄金集回归，防止功能通过但执行质量回退。

### 阶段 7：Supervisor 与 Release B（第 21–22 周）

- [ ] 实现 Worker 能力注册、任务委派和汇总协议；Worker 权限取用户、主 Agent 与 Worker 配置交集，委派不得提升工具或数据权限。
- [ ] 子任务使用独立 Session 并关联主 Run 与当前用户，RLS、审计、记忆写入和删除均保留原始用户及父子关系。
- [ ] 默认并发 4、嵌套深度 2，统一传播取消信号；增加委派环检测、用户级并发上限，以及所有子任务共享的 Token/费用/时长预算。
- [ ] 显式展示部分失败，并在前端显示执行树；汇总区分事实、Worker 推断与缺失结果，禁止把失败子任务描述为成功。
- [ ] 覆盖 Worker 超时、崩溃、重复领取、迟到结果、父 Run 取消和部分失败恢复，确保子任务不成为永久孤儿。
- [ ] 在预发布环境以生产等效配置发布 Release B，完成双副本、全量回归、Agent 质量评测、迁移/回滚及恢复演练后，方可进入受控 Beta。

### 阶段 8：飞书集成（第 23–25 周）

- [ ] 实现 URL 验证、验签、事件解密和配置检查；校验时间戳/重放窗口，支持密钥轮换，原始事件与错误日志不记录明文敏感内容。
- [ ] 实现 open_id 身份映射与单聊映射；身份绑定冲突必须人工处理，禁止仅凭可伪造字段自动合并已有账户。
- [ ] 实现“一话题一 Session”：以 tenant/chat/thread 为主键、root_message_id 为过渡别名，在同一事务创建 Session/绑定/成员，支持同群多话题并发且禁止回退为群级共享 Session。
- [ ] event_id 幂等入库并在持久化成功后快速 ACK，再由任务处理；超时、5xx 与飞书重推遵循至少一次语义，消费幂等且可死信重放。
- [ ] 实现主时间线 @机器人创建话题、话题内追问复用 Session、机器人卡片同话题回复、关闭/归档/恢复/解绑；处理乱序、撤回、编辑、并发发言和普通群能力降级。
- [ ] 群聊不注入个人长期记忆；个人事实只写发言者私有记忆，提供群级禁用记忆、解绑、数据删除与管理员授权策略。
- [ ] 使用可更新卡片增量回复，失败时降级最终文本；限制卡片更新频率、消息长度与每个 chat/user 的并发，避免放大飞书限流。
- [ ] 实现 Tenant Access Token 缓存、主动刷新、失效重取和最小权限 Scope，所有外部回复使用业务去重键防止重复发送。
- [ ] 覆盖伪造签名、重放、重复、乱序、Token 失效、限流重试、进程崩溃、回复部分成功和飞书长期不可用；专项验证同群多话题上下文隔离、root/thread 竞态、话题内恢复及恢复后不重复打扰用户。

### 阶段 9：安全、可观测性与 Release C（第 26–28 周）

- [ ] OpenTelemetry 覆盖 HTTP/gRPC、Eino、LLM、Tool、Job、飞书，贯通 `request_id/run_id/step_id`；配置采样、出口认证与脱敏，OTel/stdout 默认不采集 Prompt/Completion/工具原文，受控执行内容只进入 RLS 保护的执行记录。
- [ ] 完成执行记录与审计生产验收：验证大请求 Manifest 重建、部分响应、工具结果落库失败后的对账状态、审计同事务、Hash Chain/Checkpoint 验链、保留删除、Break-Glass 读取和导出再脱敏。
- [ ] 暴露 API RED、资源 USE、Token/费用、Run、工具、队列、Outbox、SSE 与记忆指标；指标不使用 user_id/session_id/run_id 等高基数标签，结构化日志统一级别、关联 ID 和保留期。
- [ ] 建立 SLO/容量/成本看板、错误预算与分级告警，提供健康探测和外部合成监控；每个 Critical/High 告警完成触发测试并关联 Runbook。
- [ ] 高风险工具只允许在独立受限 Sandbox Runner 中执行：非 root、只读根文件系统、临时工作目录、Drop Capabilities、Seccomp、CPU/内存/PID/时长限制、默认拒绝网络及禁止挂载 Docker Socket。
- [ ] 验证至少两个 API/Worker 副本的滚动发布与优雅退出：readiness 摘流、停止领取新任务、SSE 可重连、租约可恢复，重启不造成重复副作用。
- [ ] 完成 PostgreSQL 高可用/故障转移、Redis 丢失与恢复、连接池耗尽、LLM/MCP/飞书慢调用和不可用等故障注入；明确各场景降级、用户提示和恢复动作。
- [ ] 实施加密备份、跨故障域副本、PITR 与保留策略；从空环境恢复 PostgreSQL、向量、任务与配置，校验 RLS 和行数/业务一致性，实测满足 RPO/RTO，Redis 无需作为事实恢复。
- [ ] 完成目标峰值及 1.5 倍峰值负载、压力、长时间稳定性和容量测试，覆盖大消息、慢消费者、工具风暴、队列积压与多用户公平性，输出资源规格和扩容阈值。
- [ ] 完成 SAST/DAST、依赖/容器/IaC/密钥扫描、API Fuzz、权限矩阵和独立安全审查；Critical/High 必须修复或有书面风险接受，跨用户泄露一律不得豁免。
- [ ] 生成 SBOM、构建来源证明并签名镜像，部署系统校验签名和批准版本；生产启用全链路 TLS、数据库/备份静态加密、Redis 认证、网络分段、最小端口/egress 和管理接口访问控制。
- [ ] 建立不可变制品的预发布验收、生产灰度/滚动发布、自动 Smoke 与回滚流程；验证新库兼容旧应用、队列消息新旧兼容、在途 Run 可恢复，禁止自动执行破坏性降级迁移。
- [ ] 完成全账户导出/删除、保留周期、审计保全、备份过期清除和密钥吊销演练；核对第三方 Provider/飞书数据处理与配置符合确认的合规边界。
- [ ] 交付部署、配置、扩容、备份恢复、故障处理、安全事件、值班升级与复盘 Runbook；完成 GA Checklist 签字后发布 Release C，未通过项必须有明确阻断而非口头豁免。

## 验证命令

每阶段运行：

```bash
make generate-check
make lint
make test-unit
make test-race
make test-integration
make test-contract
make test-web
make build
```

涉及数据库或 Agent 行为变更时增加：

```bash
make test-migration
make test-agent-eval
```

Release A/B/C 额外运行：

```bash
make test-e2e
make test-security
make test-load
make test-chaos
make test-agent-eval
make test-execution-audit
make test-migration
docker compose up --build --wait
make smoke
```

Release C/GA 再增加：

```bash
make test-soak
make test-restore
make verify-supply-chain
make verify-rollback
```

所有 Makefile 目标必须在 CI 或预发布环境真实执行，禁止以本地口头验证代替。测试不得默认跳过；隔离不稳定用例必须记录负责人、原因和到期时间，阻断级用例不得隔离后发布。

## 质量门禁与测试矩阵

- **单元测试：** 核心逻辑使用表驱动、边界、并发与错误路径测试；状态机、Token 预算、游标、重试、签名、Redaction Pipeline 和 Schema 解析增加 Property/Fuzz 测试。
- **集成测试：** 使用真实 PostgreSQL/pgvector、Redis 和非特权数据库角色；覆盖事务、RLS、连接池、约束、索引、Outbox、租约、执行 Payload 隔离、审计 Append-only 和缓存重建，不能用 Mock 替代基础设施验证。
- **契约测试：** 覆盖 Protobuf/HTTP/gRPC/OpenAPI、SSE 事件顺序与版本、错误码、分页、幂等、前后端类型和旧客户端兼容性；Buf Breaking 不通过不得合并。
- **E2E 测试：** 覆盖注册/登录/撤销、模型目录与路由策略、三种 Agent、模型故障转移、执行时间线与 Payload 查看、审计查询/导出、取消/重连、审批、删除、飞书单聊及同群多话题和管理员权限；至少两个用户并发执行以证明隔离。
- **Agent 质量评测：** 黄金集与对抗集版本化，固定模型/参数/Prompt 快照并重复运行以观察波动；CI 使用确定性 Fake Model 验证连续失败、限流、熔断、候选切换与流式中断，预发布再对批准的实时模型验收，结果与基线比较并保留人工评审记录。
- **安全测试：** 强制验证跨用户 ID 枚举、缓存/向量串扰、JWT 过期/重放、RLS 绕过、CSRF/XSS/CORS、SSRF/DNS Rebinding、Prompt Injection、工具越权、Sandbox 逃逸、飞书伪造与重复投递，以及所有密钥和敏感内容脱敏。
- **执行与审计测试：** 验证模型请求先于调用落库、完成/失败/取消响应、工具 I/O 与顺序、1M Manifest 重建、脱敏/截断、隐藏思维链丢弃、跨用户读取阻断、Break-Glass 理由审计、审计同事务、Hash Chain 验链，以及结果落库失败后的 `reconciliation_required`。
- **可靠性与性能测试：** 强制验证 SSE 断线/慢消费/取消、Agent 循环、Plan 恢复与幂等、Supervisor 部分失败、任务重复/乱序/死信、Worker/API 重启、PostgreSQL 故障转移、Redis 清空和外部依赖降级；负载之外必须做压力与稳定性测试。
- **灾备与发布测试：** 每个候选版本验证空库和上一版本升级、新旧代码并行、应用回滚、备份恢复和密钥恢复；恢复后执行登录、RLS、历史 Run、向量检索、任务续跑和 Smoke。

## 发布、回滚与持续运营门禁

### Release A：内部测试版

- 功能闭环、核心单测/集成/契约/E2E 和安全基线通过；仅部署内部测试环境。
- 已知限制、数据重置策略和反馈入口明确；不得接入未脱敏生产数据或真实高风险工具。

### Release B：预发布 Beta

- 在生产等效拓扑、配额和数据规模下通过全量回归、Agent 质量、双副本、故障注入、迁移与回滚演练。
- 监控、告警、审计、成本预算、备份和 Runbook 可用；Beta 用户范围、支持渠道和退出/数据清理方案明确。

### Release C：生产 GA

- 所有阶段任务与生产级非功能基线通过，无阻断缺陷、未处置 Critical/High 漏洞或跨用户泄露。
- 实测容量满足目标峰值并有余量，实测恢复满足 RPO/RTO；灰度、Smoke、自动停止和回滚路径经过真实演练。
- SLO 看板、告警、值班责任人、升级路径、成本预算、数据治理、合规确认和运维交接全部完成并签字。
- 首次上线先小流量灰度并观察核心指标；出现数据损坏/越权、重复副作用、持续 SLO 突破、队列不可恢复或费用异常时立即停止扩量并回滚。

### 回滚与事件处置

- 制品不可变且可按版本回退；先停止灰度/摘流并保留证据，再回退兼容应用、暂停相关 Worker 或工具，处理在途 Run 和队列后恢复服务。
- 数据库优先前滚修复；只有已证明兼容且数据无损时才执行反向迁移。回滚后必须验证旧应用对新 Schema、事件和任务消息的兼容性。
- 数据泄露、重复副作用或错误外发优先封禁凭据/工具/租户并控制影响，再恢复服务；按事件分级完成用户通知、根因分析、纠正措施与复盘。

### 上线后的持续运营

- 持续监控 SLO、错误预算、队列、成本与容量；错误预算耗尽时冻结非必要发布，优先可靠性修复。
- 定期执行依赖与基础镜像更新、漏洞复扫、权限复核、密钥/证书到期检查、备份恢复演练和 Agent 黄金集复评，组件停止支持前完成升级。
- 模型、Prompt、Skill、MCP、Embedding 或定价变更均视为受控发布，必须有版本、评测、灰度和回退；不得在生产直接覆盖历史配置。
- 定期复核数据保留与删除任务、审计完整性、第三方数据政策及成本异常；所有生产变更保留操作者、审批、版本和结果记录。

## 默认裁决

- v1 只提供 OpenAI-compatible Provider；其他厂商通过适配接口接入。所有新生成模型 Run 只允许使用经核验的 1M+ 上下文模型，任何候选切换必须显式配置并经过质量与合规验证，Embedding/Rerank 不适用该上下文门槛。
- PostgreSQL 是唯一事实源，Redis 清空不得造成永久数据丢失；生产使用高可用 PostgreSQL，Redis 故障时按场景降级而非绕过隔离。
- 应用无状态，任务领取使用 PostgreSQL 行锁、租约和 fencing token；系统承诺至少一次投递加幂等处理，不宣称端到端 Exactly Once。
- 单镜像是交付形式，不是单副本架构；生产至少两个 API/Worker 副本，Compose 不承担生产高可用。
- 飞书群聊默认要求话题模式并执行“一话题一 Session”，禁止仅按 chat_id 汇聚整个群上下文；群话题不使用成员个人长期记忆，Worker 与后台任务同样绑定用户上下文，不得使用共享特权身份绕过 RLS。
- 高风险 MCP 工具在 Sandbox 可用前保持禁用；非幂等副作用不得自动盲重试，必要时等待人工确认。
- 业务数据默认长期保留但必须有分类保留策略；执行 Payload 与审计采用独立保留期。删除 Session/Memory/账户时同步编排 PG、执行内容、向量、缓存、外部绑定和任务清理，审计仅按合规保留去内容化/假名化记录，备份随周期最终过期。
- YAML 只保存非密静态配置与环境覆盖；动态模型目录/候选策略存 PostgreSQL，生产密钥由 Secret Manager/KMS 管理并支持轮换。生产镜像与依赖固定精确版本或 Digest，生成 SBOM、来源证明与签名。
- Release A/B 不是生产就绪版本；只有 Release C 通过全部 GA 门禁后才可承接生产流量。
- 不保存或展示隐藏思维链；OTel/stdout 默认不采集 Prompt/Completion/工具原文。模型请求响应和工具 I/O 只作为经脱敏、受 RLS 与独立读取权限保护的执行记录保存，查看、导出和重放均受审计与副作用限制。
