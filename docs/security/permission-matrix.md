# 权限矩阵

所有 `/api/v1` 业务接口默认要求 Bearer Token 或 HttpOnly Cookie 会话。用户身份只来自 Token，不接受请求体传入的 `user_id`。

## 角色

| 角色 | 说明 | 数据范围 |
| --- | --- | --- |
| 匿名 | 未登录 | 仅健康检查、登录/注册、CSRF、飞书回调验签 |
| user | 普通用户 | 仅自己的 Agent、Session、Run、消息、记忆与导出 |
| admin | 平台管理员 | 平台配置管理；跨用户读取仅允许 Break-Glass 且必须记录理由与审计 |

## 接口矩阵

| 接口 | 方法 | 匿名 | user | admin | 隔离规则 |
| --- | --- | --- | --- | --- | --- |
| `/health/live`、`/health/ready`、`/api/v1/health` | GET | 允许 | 允许 | 允许 | 不返回用户数据 |
| `/metrics` | GET | 允许 | 允许 | 允许 | 标签禁止 user/session/run；生产应限制网络访问 |
| `/api/v1/sse/probe` | GET | 允许 | 允许 | 允许 | 仅心跳，不读取业务数据 |
| `/api/v1/auth/register`、`login`、`refresh`、`csrf` | POST/GET | 允许 | 允许 | 允许 | 登录限流；注册受 `registration_enabled` 控制；刷新并发重放；Cookie 模式强制 CSRF |
| `/api/v1/auth/password`、`/api/v1/auth/password/reset` | POST | 仅 reset 允许 | 允许 | 允许 | 改密校验当前密码；重置令牌一次性、15 分钟过期；成功后全端失效 |
| `/api/v1/admin/users`、`users/{id}/status`、`users/{id}/password-reset` | POST | 拒绝 | 403 | 允许 | 管理员邀请创建用户、禁用/启用与一次性改密；全部写审计 |
| `/api/v1/providers`、`models`、`model-policies`、`skills`、`mcps` | GET | 拒绝 | 允许 | 允许 | 全局平台配置只读 |
| `/api/v1/agents` | GET/POST/PUT/DELETE | 拒绝 | 允许 | 允许 | 强制 `user_id`；发布版本不可跨用户复用 |
| `/api/v1/sessions`、`sessions/{id}`、`messages`、`runs` | GET/POST | 拒绝 | 允许 | 允许 | RLS + 仓储 `WHERE user_id`；跨用户返回 404 |
| `/api/v1/runs/{id}/events`、`steps`、`attempts`、`approvals` | GET | 拒绝 | 允许 | 允许 | SSE 连接上限 500；写入超时断开慢消费者 |
| `/api/v1/sessions/{id}/export`、`/api/v1/memory/export` | GET | 拒绝 | 允许 | 允许 | 导出按用户限制并二次脱敏 |
| `/api/v1/deletions` | GET/POST | 拒绝 | 允许 | 允许 | 删除请求绑定用户与审计 |
| `/api/v1/admin/*` | 任意 | 拒绝 | 403 | 允许 | 管理操作写审计 |
| `/api/v1/admin/break-glass` | POST | 拒绝 | 403 | 允许 | 理由 4–500 字符；审计失败 fail-closed |
| 飞书事件回调 | POST | 允许 | — | — | 必须验签、解密、时间窗校验与 event_id 幂等 |

## 安全约束

- PostgreSQL 所有用户表启用 RLS；查询必须通过 `WithScope` 注入 `app.user_id` 或显式 `Scope{System:true}`。
- 管理员跨用户读取只能通过 Break-Glass；普通列表接口不得返回其他用户数据。
- 跨用户泄露不属于可接受风险，禁止书面豁免。
- 导出文件必须执行凭证正则匹配与敏感键递归脱敏。
- 所有管理操作与 Break-Glass 读取写入审计 Hash Chain。
