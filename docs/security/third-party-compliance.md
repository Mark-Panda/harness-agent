# 第三方数据处理与合规边界核对

日期：2026-11-03。范围：OpenAI-compatible 模型 Provider、飞书开放平台。本文档记录技术控制与部署核对项，不构成法律意见；生产上线前仍须由合规负责人确认合同、数据区域与子处理者清单。

## 1. 数据流与数据类别

| 数据类别 | 是否外发 | 接收方 | 用途与边界 |
| --- | --- | --- | --- |
| 会话最近消息、滚动摘要、长期记忆、结构化画像 | 是 | 模型 Provider | 仅作为推理上下文；群聊话题不注入个人长期记忆与画像 |
| System Prompt、Skill 指令、工具定义与工具结果 | 是 | 模型 Provider / MCP 工具 | 工具结果按不可信内容处理；高风险工具在 Sandbox 中执行 |
| Provider / MCP 凭证 | 是（仅认证头） | 对应 Provider / MCP | AES-256-GCM 加密存储，带 `key_version`；日志与导出不返回明文 |
| 用户消息正文、发言人 `open_id`、chat/thread ID | 是 | 飞书开放平台（回复与卡片更新） | 仅用于回复与话题绑定；出站消息有业务去重键 |
| 飞书事件原始载荷 | 否 | 无 | 入库用于重放，按发言人回填 `user_id`，默认保留 7 天 |
| 隐藏思维链、Prompt/Completion 原文、工具原文 | 否 | 无 | OTel/stdout 默认不采集；`observability.include_execution_payloads` 启动时强制为 false |
| 审计日志 | 否 | 无 | 追加型、假名化 Actor，不复制执行载荷 |

## 2. 模型 Provider 控制

- v1 仅支持 OpenAI-compatible Provider；其他厂商需通过适配接口接入并重新做合规评审。
- Provider 注册时必须填写 HTTPS `base_url` 与 `data_region`；模型启用必须经过管理员核验并满足 1M+ 上下文门槛。
- 数据区域约束 `constraints.data_region`：
  - 发布策略时硬校验候选模型的 `data_region`，不一致直接拒绝发布；
  - 创建 Run 时再次校验，历史策略快照不能绕过；
  - Worker 组装候选时再校验，不匹配的候选不会执行。
- 未配置 `data_region` 约束的候选不受地理限制；生产准入评审必须为每个候选配置该约束，或者在书面风险接受中说明原因。
- 跨 Provider 故障转移只允许切换到同样满足 1M+、能力与数据区域约束的候选；流式已输出后禁止静默拼接其他模型结果。
- Provider 凭证轮换见 `docs/runbooks/key-revocation.md`；轮换期保留旧密钥解密，确认旧 `key_version` 清零后再移除。

## 3. 飞书数据处理

- Webhook 必须通过签名、时间窗口、事件解密与 `max_body_bytes` 校验；原始事件与错误日志不记录明文敏感内容。
- `feishu_event_receipts` 保存原始事件载荷用于重放：
  - 事件处理解析身份后回填 `user_id`；
  - 账号删除按 `user_id` 删除回执，避免正文跨账号残留；
  - 默认保留 7 天（`retention.feishu_events`），由 `cmd/retention` 清理。
- 群聊只处理明确 @ 机器人的消息；默认要求话题模式，一话题一 Session，禁止仅按 `chat_id` 汇聚整个群上下文。
- 群聊话题使用 `group` 记忆策略：不注入个人长期记忆与结构化画像，也不写入个人事实；个人事实只写入发言者私有记忆。
- 出站回复使用业务去重键，卡片更新失败降级为最终文本，避免重复打扰；飞书限流与不可用按 Runbook 降级。
- 飞书身份映射冲突必须人工处理，禁止仅凭可伪造字段自动合并账户。

## 4. 导出、删除与保留期

| 数据 | 导出 | 删除 | 保留期 |
| --- | --- | --- | --- |
| 账号（Agent、会话、消息、摘要、事实、向量） | `GET /api/v1/account/export`，二次脱敏 | `POST /api/v1/account/deletion`，异步清空业务数据并匿名化用户行 | 业务数据长期保留，按用户请求删除 |
| 会话 / 长期记忆 | `GET /api/v1/sessions/{id}/export`、`GET /api/v1/memory/export` | 对应 deletion 接口 | 同上 |
| 执行载荷 | 经 Break-Glass 与审计的脱敏读取 | 随会话/账号删除 | `retention.execution_records`，默认 90 天 |
| 审计日志与检查点 | 受权限与审计约束的查询/导出 | 仅保留期任务可删除 | `retention.audit_logs`，默认 365 天 |
| 飞书事件回执 | 不提供导出 | 账号删除或保留期清理 | `retention.feishu_events`，默认 7 天 |
| 备份 | 不直接对用户提供 | 随保留期最终过期清除 | `BACKUP_RETENTION_DAYS`，默认 30 天 |

账号删除保留 `audit_logs`、`outbox_events`、`deletion_requests` 以维持审计链路与合规追溯；用户行以 `deleted_<uuid>` 匿名化，`credential_version` 递增使全部令牌失效。

## 5. 部署核对清单

- [ ] 每个 Provider 已确认数据区域、子处理者、跨境传输条件与合同/DPA 状态，并写入 `data_region`。
- [ ] 每个已发布策略的候选都配置了 `constraints.data_region`，或存在书面风险接受。
- [ ] 生产 OTLP Collector 未采集 Prompt/Completion/工具原文，`include_execution_payloads=false`。
- [ ] 飞书应用仅申请最小 Scope；事件加密密钥、Verification Token 与 App Secret 由 Secret Manager 注入。
- [ ] 飞书事件回执保留期与账号删除流程经过演练，确认无跨用户正文残留。
- [ ] 备份保留期、跨故障域副本与密钥轮换流程已签字确认。
- [ ] 第三方服务不可用时的用户提示与降级路径已纳入 Runbook。

## 6. 验证证据

- `go test ./internal/biz -run TestPolicyPublishRejectsDataRegionMismatch`：数据区域约束在发布时硬校验。
- `REQUIRE_DATABASE=true go test -tags=integration ./internal/data -run 'TestPurgeExpiredRetention|TestDeletionExecutorDeletesAccount'`：保留期清理与账号删除覆盖飞书事件回执。
- `go test ./internal/platform/config`：生产配置强制关闭执行载荷采集、校验飞书密钥与时间窗口。
- `go test ./internal/server -run TestAccountExportAndDeletionHTTP`：账号导出脱敏、附件响应与鉴权边界。
