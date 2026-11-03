-- 000007：为业务表与关键字段补充中文注释（仅元数据，不改变结构与数据）。
-- 说明：注释通过 COMMENT ON 写入 PostgreSQL 目录，便于运维与数据治理审计。

COMMENT ON TABLE users IS '平台用户账号表：保存身份、角色、账号状态与凭据版本，是所有私有数据的隔离根';
COMMENT ON COLUMN users.id IS '用户主键，同时作为全部私有表的 RLS 隔离键';
COMMENT ON COLUMN users.username_normalized IS '规范化用户名（小写去空白），登录匹配与唯一约束使用';
COMMENT ON COLUMN users.password_hash IS 'Argon2id 密码哈希，禁止保存明文密码';
COMMENT ON COLUMN users.role IS '平台角色：user 普通用户，admin 管理员';
COMMENT ON COLUMN users.status IS '账号状态：active 正常，disabled 禁用';
COMMENT ON COLUMN users.credential_version IS '凭据版本，提升后全部已签发令牌失效';

COMMENT ON TABLE user_identities IS '外部身份映射表：飞书 open_id 等外部身份到内部用户';
COMMENT ON COLUMN user_identities.provider IS '外部身份提供方，如 feishu';
COMMENT ON COLUMN user_identities.tenant_key IS '外部身份租户标识，群聊与单聊隔离使用';
COMMENT ON COLUMN user_identities.external_subject IS '外部身份主体标识，如飞书 open_id';
COMMENT ON COLUMN user_identities.user_id IS '映射到的内部用户 ID';

COMMENT ON TABLE refresh_tokens IS '不透明刷新令牌表：支持家族轮换、重用检测与全端撤销';
COMMENT ON COLUMN refresh_tokens.token_hash IS '带 Pepper 的令牌哈希，禁止保存明文令牌';
COMMENT ON COLUMN refresh_tokens.family_id IS '令牌家族 ID，轮换产生的令牌共享同一家族';
COMMENT ON COLUMN refresh_tokens.parent_token_id IS '轮换来源令牌 ID，用于追溯家族链';
COMMENT ON COLUMN refresh_tokens.credential_version IS '签发时用户凭据版本，版本不匹配即失效';
COMMENT ON COLUMN refresh_tokens.rotated_at IS '已轮换时间，非空表示该令牌已被使用';
COMMENT ON COLUMN refresh_tokens.reuse_detected_at IS '检测到令牌重用的时间，命中后整族撤销';

COMMENT ON TABLE llm_providers IS 'LLM 供应商连接表：仅保存连接与加密凭证，与具体模型分离';
COMMENT ON COLUMN llm_providers.base_url IS 'OpenAI-compatible HTTPS 基础地址';
COMMENT ON COLUMN llm_providers.api_key_encrypted IS 'API Key 的 AES-256-GCM 密文，禁止明文入库与回显';
COMMENT ON COLUMN llm_providers.key_version IS '密文对应的密钥版本，支持不停机轮换解密';
COMMENT ON COLUMN llm_providers.data_region IS '数据区域，用于合规路由过滤';

COMMENT ON TABLE llm_models IS '模型目录表：记录供应商下的具体部署、上下文窗口、能力与核验信息';
COMMENT ON COLUMN llm_models.remote_model_name IS '供应商侧真实模型名';
COMMENT ON COLUMN llm_models.model_kind IS '模型用途：chat 生成、embedding 向量化、rerank 重排';
COMMENT ON COLUMN llm_models.context_window_tokens IS '上下文窗口 Token 数，chat 类型必须大于等于 1000000';
COMMENT ON COLUMN llm_models.capabilities IS '模型能力 JSON：流式、工具调用、结构化输出';
COMMENT ON COLUMN llm_models.verification_source IS '上下文窗口等元数据的核验来源，启用时必填';
COMMENT ON COLUMN llm_models.verified_at IS '管理员核验时间，启用时必填';

COMMENT ON TABLE model_policies IS '模型路由策略表：每个版本一行，发布后不可变';
COMMENT ON COLUMN model_policies.version IS '策略版本号，与名称共同唯一';
COMMENT ON COLUMN model_policies.min_context_tokens IS '策略要求的最小上下文窗口，不得低于 1000000';
COMMENT ON COLUMN model_policies.rules IS '路由规则 JSON，如健康优先级与成本约束';
COMMENT ON COLUMN model_policies.status IS '生命周期：draft 草稿、published 已发布、disabled 已停用';

COMMENT ON TABLE model_policy_candidates IS '策略候选表：保存有序候选模型与单候选约束';
COMMENT ON COLUMN model_policy_candidates.priority IS '显式优先级，数值越小越优先，同一策略内唯一';
COMMENT ON COLUMN model_policy_candidates.constraints IS '单候选约束 JSON，如数据区域与费用上限';

COMMENT ON TABLE skills IS '平台级 Skill 模板表：提供可复用提示词与参数定义';
COMMENT ON COLUMN skills.prompt_template IS 'Skill 提示词模板';
COMMENT ON COLUMN skills.parameters_json IS 'Skill 参数定义 JSON Schema';

COMMENT ON TABLE mcps IS 'MCP 工具接入表：保存远端端点、鉴权与风险等级';
COMMENT ON COLUMN mcps.endpoint IS 'MCP 远端地址，执行层需做允许列表与 SSRF 校验';
COMMENT ON COLUMN mcps.auth_credentials_encrypted IS '鉴权凭证密文，禁止明文入库';
COMMENT ON COLUMN mcps.risk_level IS '风险等级：low/medium/high，高风险工具需人工审批';

COMMENT ON TABLE agents IS '用户私有 Agent 版本表：草稿与发布版本分离，发布后固化配置快照';
COMMENT ON COLUMN agents.user_id IS '所属用户，所有查询强制按此隔离';
COMMENT ON COLUMN agents.mode IS '编排模式：react、plan、super';
COMMENT ON COLUMN agents.model_policy_version IS '引用的策略版本，与策略 ID 一起构成不可变引用';
COMMENT ON COLUMN agents.version IS 'Agent 版本号，同用户同名递增';
COMMENT ON COLUMN agents.config_snapshot IS '发布时固化的策略、模型、Skill、MCP 非密快照';
COMMENT ON COLUMN agents.status IS '生命周期：draft 草稿、published 已发布、disabled 已停用';

COMMENT ON TABLE agent_skills IS 'Agent 与 Skill 绑定表';
COMMENT ON COLUMN agent_skills.user_id IS '所属用户，RLS 隔离键';
COMMENT ON TABLE agent_mcps IS 'Agent 与 MCP 绑定表';
COMMENT ON COLUMN agent_mcps.user_id IS '所属用户，RLS 隔离键';

COMMENT ON TABLE sessions IS '用户私有会话表：承载消息序列与上下文归属';
COMMENT ON COLUMN sessions.user_id IS '所属用户，RLS 隔离键';
COMMENT ON COLUMN sessions.agent_id IS '绑定的已发布 Agent 版本，可为空';
COMMENT ON COLUMN sessions.source IS '来源渠道：web、feishu、api';
COMMENT ON COLUMN sessions.status IS '生命周期：active 活跃、archived 已归档、deleted 已删除';
COMMENT ON COLUMN sessions.version IS '乐观锁版本号，归档与写消息时递增';
COMMENT ON COLUMN sessions.last_message_at IS '最近消息时间，用于会话列表排序';

COMMENT ON TABLE session_members IS '会话成员表：记录会话参与者与角色';
COMMENT ON COLUMN session_members.member_role IS '成员角色：owner 所有者、member 成员';

COMMENT ON TABLE messages IS '会话消息表：保存规范对话内容，不含隐藏思维链';
COMMENT ON COLUMN messages.user_id IS '所属用户，RLS 隔离键';
COMMENT ON COLUMN messages.role IS '消息角色：system/user/assistant/tool';
COMMENT ON COLUMN messages.content IS '消息正文，单条上限 4MB';
COMMENT ON COLUMN messages.tool_calls IS '工具调用元数据 JSON 数组，不保存隐藏推理';
COMMENT ON COLUMN messages.token_count IS 'Token 估算值，供上下文预算使用';
COMMENT ON COLUMN messages.external_message_id IS '外部渠道消息 ID，用于幂等去重';
COMMENT ON COLUMN messages.speaker_open_id IS '飞书群聊发言人 open_id，单聊为空';

COMMENT ON TABLE agent_runs IS 'Agent Run 表：一次执行的不可变配置快照与状态机';
COMMENT ON COLUMN agent_runs.user_id IS '所属用户，RLS 隔离键';
COMMENT ON COLUMN agent_runs.mode IS '本次 Run 使用的编排模式';
COMMENT ON COLUMN agent_runs.status IS '状态机：queued/running/waiting_approval/completed/failed/cancelled/reconciliation_required';
COMMENT ON COLUMN agent_runs.config_snapshot IS 'Run 创建时固化的 Agent 配置快照，运行中不受策略变更影响';
COMMENT ON COLUMN agent_runs.model_policy_snapshot IS 'Run 创建时固化的候选、规则与模型元数据快照';
COMMENT ON COLUMN agent_runs.current_step_seq IS '当前执行步骤序号';
COMMENT ON COLUMN agent_runs.attempt_no IS '模型调用尝试序号，用于故障切换';
COMMENT ON COLUMN agent_runs.idempotency_key IS '创建幂等键，同用户内唯一';
COMMENT ON COLUMN agent_runs.deadline_at IS 'Run 截止时间，超时后不再发起新调用';

COMMENT ON TABLE execution_steps IS '执行步骤表：模型、工具、计划、Worker 等步骤事实';
COMMENT ON COLUMN execution_steps.step_seq IS 'Run 内单调递增步骤序号，唯一';
COMMENT ON COLUMN execution_steps.kind IS '步骤类型：model/tool/plan/worker/system';
COMMENT ON COLUMN execution_steps.status IS '步骤状态机，与 Run 状态联动';
COMMENT ON COLUMN execution_steps.risk_level IS '工具风险等级，高风险需审批';
COMMENT ON COLUMN execution_steps.idempotency_key IS '工具执行幂等键，防止重复副作用';

COMMENT ON TABLE execution_payloads IS '执行载荷表：保存脱敏后的模型请求/响应与工具输入输出';
COMMENT ON COLUMN execution_payloads.user_id IS '所属用户，显式携带并启用 RLS';
COMMENT ON COLUMN execution_payloads.payload_type IS '载荷类型：model_request/model_response/tool_input/tool_output 等';
COMMENT ON COLUMN execution_payloads.capture_mode IS '采集模式：metadata_only 仅元数据、redacted 脱敏内容';
COMMENT ON COLUMN execution_payloads.redaction_version IS '脱敏规则版本，便于追溯';
COMMENT ON COLUMN execution_payloads.truncated IS '是否因大小上限被截断';
COMMENT ON COLUMN execution_payloads.partial IS '流式中断时是否为部分响应';

COMMENT ON TABLE run_events IS 'Run 事件表：SSE 可重放时间线，按 seq 单调递增';
COMMENT ON COLUMN run_events.seq IS 'Run 内单调递增事件序号，与 run_id 共同唯一';
COMMENT ON COLUMN run_events.event_type IS '事件类型：meta/message.delta/tool.started/run.completed 等';

COMMENT ON TABLE model_calls IS '模型调用事实表：每次 Attempt 的模型、用量、延迟与切换原因';
COMMENT ON COLUMN model_calls.attempt_no IS 'Run 内单调递增尝试序号';
COMMENT ON COLUMN model_calls.policy_version IS '本次调用使用的策略版本';
COMMENT ON COLUMN model_calls.request_manifest IS '请求清单：消息、Prompt、工具 Schema 版本与 Hash';
COMMENT ON COLUMN model_calls.status IS '尝试状态：running/succeeded/failed/cancelled';
COMMENT ON COLUMN model_calls.request_hash IS '脱敏请求 Hash，用于重放与一致性校验';

-- plans 与 plan_steps 的注释由 000016 在建表后写入，避免在 000004 未建表的全新数据库中报错。

COMMENT ON TABLE session_summaries IS '短期记忆摘要表：按会话版本化保存递归摘要';
COMMENT ON COLUMN session_summaries.version IS '摘要版本号，同一会话内递增';
COMMENT ON COLUMN session_summaries.summary_text IS '摘要文本，单条约 1MB 上限';
COMMENT ON COLUMN session_summaries.source_message_ids IS '摘要覆盖的源消息 ID 列表';

COMMENT ON TABLE memory_vectors IS '长期记忆向量表：1536 维 embedding 与 HNSW 索引';
COMMENT ON COLUMN memory_vectors.embedding IS '1536 维向量，检索按 cosine 距离';
COMMENT ON COLUMN memory_vectors.embedding_provider IS '生成向量的供应商';
COMMENT ON COLUMN memory_vectors.embedding_model IS '生成向量的模型名';
COMMENT ON COLUMN memory_vectors.embedding_dimension IS '向量维度，当前固定 1536，不同版本不得混用索引';
COMMENT ON COLUMN memory_vectors.embedding_version IS '向量化版本，用于不停机重建与双写校验';
COMMENT ON COLUMN memory_vectors.deleted_at IS '软删除时间，删除后不可被召回';

COMMENT ON TABLE memory_facts IS '结构化记忆表：LLM 提取的用户事实与有效期';
COMMENT ON COLUMN memory_facts.fact_key IS '事实键，如偏好类别';
COMMENT ON COLUMN memory_facts.fact_value IS '事实值 JSON';
COMMENT ON COLUMN memory_facts.confidence IS '置信度 0-1，低置信度不得覆盖用户明确纠正';
COMMENT ON COLUMN memory_facts.valid_from IS '生效时间';
COMMENT ON COLUMN memory_facts.valid_to IS '失效时间，为空表示仍有效';

COMMENT ON TABLE external_conversation_bindings IS '外部会话绑定表：飞书话题/群聊与内部会话映射';
COMMENT ON COLUMN external_conversation_bindings.chat_id IS '飞书群 chat_id';
COMMENT ON COLUMN external_conversation_bindings.thread_id IS '飞书话题 ID，话题场景按此唯一绑定';
COMMENT ON COLUMN external_conversation_bindings.root_message_id IS '无话题能力时作为过渡会话别名';
COMMENT ON COLUMN external_conversation_bindings.session_id IS '映射到的内部会话';

COMMENT ON TABLE feishu_event_receipts IS '飞书事件回执表：用于事件幂等与重放防护';
COMMENT ON COLUMN feishu_event_receipts.event_id IS '飞书事件 ID，全局唯一';
COMMENT ON COLUMN feishu_event_receipts.payload_redacted IS '脱敏后的事件负载';
COMMENT ON COLUMN feishu_event_receipts.status IS '处理状态：received/processed/failed';

COMMENT ON TABLE jobs IS '持久化任务表：摘要、记忆、回调等异步任务';
COMMENT ON COLUMN jobs.dedupe_key IS '任务去重键，保证至少一次投递下的幂等';
COMMENT ON COLUMN jobs.available_at IS '最早可执行时间，用于指数退避';
COMMENT ON COLUMN jobs.lease_owner IS '租约持有者，防止多副本重复执行';
COMMENT ON COLUMN jobs.lease_expires_at IS '租约到期时间，过期后可被重新认领';
COMMENT ON COLUMN jobs.fencing_token IS '栅栏令牌，过期 Worker 不得提交结果';
COMMENT ON COLUMN jobs.status IS '状态：pending/running/completed/failed/dead';
COMMENT ON COLUMN jobs.max_attempts IS '最大尝试次数，超过进入死信';

COMMENT ON TABLE outbox_events IS 'Outbox 事件表：业务事务内写入，异步发布审计事件';
COMMENT ON COLUMN outbox_events.dedupe_key IS '去重键，唯一索引保证事件只发布一次';
COMMENT ON COLUMN outbox_events.payload IS '审计元数据白名单 JSON，不得含密钥或正文';
COMMENT ON COLUMN outbox_events.status IS '发布状态：pending/published/failed';
COMMENT ON COLUMN outbox_events.attempts IS '已尝试发布次数';

COMMENT ON TABLE audit_logs IS '安全审计表：追加写入、分区序号与 Hash Chain 防篡改';
COMMENT ON COLUMN audit_logs.partition_id IS '审计分区（按天），与分区序号构成主键';
COMMENT ON COLUMN audit_logs.partition_seq IS '分区内单调递增序号，用于验证链完整';
COMMENT ON COLUMN audit_logs.actor_ref IS '操作者假名化引用，不保存原始标识';
COMMENT ON COLUMN audit_logs.on_behalf_user_ref IS '被代表用户的假名化引用';
COMMENT ON COLUMN audit_logs.action IS '审计动作，如 provider.created、agent.published';
COMMENT ON COLUMN audit_logs.outcome IS '结果：success/failure/denied';
COMMENT ON COLUMN audit_logs.prev_event_hash IS '前一条事件 Hash，形成防篡改链';
COMMENT ON COLUMN audit_logs.event_hash IS '当前事件规范化 Hash';
COMMENT ON COLUMN audit_logs.signature_version IS '签名/哈希算法版本';

COMMENT ON TABLE idempotency_keys IS '写接口幂等表：保存幂等键、请求指纹与结果引用';
COMMENT ON COLUMN idempotency_keys.key IS '客户端提供的幂等键，按用户维度唯一';
COMMENT ON COLUMN idempotency_keys.request_hash IS '请求指纹，冲突请求据此判定';
COMMENT ON COLUMN idempotency_keys.expires_at IS '幂等记录过期时间';

COMMENT ON TABLE deletion_requests IS '数据删除请求表：编排 PG、向量、缓存与外部绑定的清理';
COMMENT ON COLUMN deletion_requests.resource_type IS '删除资源类型，如 session、user';
COMMENT ON COLUMN deletion_requests.resource_id IS '待删除资源 ID';
COMMENT ON COLUMN deletion_requests.requested_by IS '发起删除的操作者';
