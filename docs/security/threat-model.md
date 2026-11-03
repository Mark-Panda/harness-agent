# 阶段 0 威胁模型

## 资产与信任边界

主要资产包括用户消息、长期记忆、Provider/MCP 凭证、JWT/Refresh Token、工具副作用、执行记录、审计日志和飞书身份绑定。

信任边界依次位于浏览器与 API、API 与 Worker、Worker 与 PostgreSQL/Redis、模型 Provider、MCP/Sandbox、飞书 Webhook 以及运维控制面。外部模型、工具返回和 Webhook 内容均视为不可信输入。

## STRIDE 与控制

| 类别 | 主要威胁 | 必需控制 |
|---|---|---|
| 伪造身份 | JWT 伪造、Refresh Token 重用、飞书签名伪造、open_id 绑定冲突 | 签名与过期校验、Token Family 重用检测、Webhook 验签和时间窗口、冲突人工处理 |
| 篡改 | 消息/Plan/Run 被并发回写、审计被修改、任务结果被过期 Worker 提交 | 状态机、乐观锁、fencing token、单调序号、Append-only 与验链 |
| 抵赖 | 管理员改配置、Agent 自动执行工具后无法追责 | Actor 与 On-Behalf 用户、关键操作同事务审计、审批与外部 Request ID |
| 信息泄露 | 跨用户 IDOR、RLS 绕过、日志泄露 Secret、Prompt/Tool 返回泄露隐私 | 用户 Scope、RLS、非特权 DB 角色、统一脱敏、受控 Payload 权限与导出审计 |
| 拒绝服务 | SSE 长连接、工具风暴、429 重试风暴、长上下文费用、队列积压 | 分层限流、并发/Token/费用预算、超时与背压、熔断、有限重试和有界队列 |
| 提权 | MCP 越权、Worker 委派提升权限、Sandbox 逃逸、管理员接口暴露 | 权限交集、工具风险分级、人工审批、受限 Sandbox、最小 RBAC 和网络策略 |

## 重点滥用场景

1. 攻击者枚举其他用户 Session、Run、Memory 或执行 Payload，必须通过每个私有 Repository 和 RLS 集成测试阻断。
2. 恶意工具返回包含间接 Prompt Injection，诱导模型泄露记忆或调用外发工具；外部内容只能作为不可信数据处理。
3. MCP Endpoint 通过重定向或 DNS Rebinding 访问内网与云元数据地址；所有解析和重定向都必须重新校验。
4. Worker 崩溃后租约过期，旧 Worker 与新 Worker 同时提交结果；fencing token 和状态迁移必须拒绝旧结果。
5. 飞书事件重复、乱序或 root/thread 绑定竞态导致串话题；唯一约束、幂等消费和独立 Session 必须阻止。
6. 工具已产生副作用但结果未落库，系统错误地自动重试；Run 必须进入对账状态而非假定失败。
7. 运维人员直接读取用户执行内容或导出后绕过审计；内容读取需要独立权限、理由和完整访问审计。

## 安全验收

跨用户泄露、明文 Secret、隐藏思维链输出、伪造 Webhook、未经批准的高风险工具执行均为发布阻断项。漏洞风险接受不能豁免租户隔离和凭证泄露。
