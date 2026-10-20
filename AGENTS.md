# 工程级 Harness Agent 系统开发计划 (全栈多用户版)

## 一、 项目概述
本项目旨在构建一个高可用、可扩展的工程级 Harness Agent 全栈系统。系统严格遵循 DDD（领域驱动设计）规范，支持多用户对话隔离，预留飞书多端接入能力。全量数据持久化至 PostgreSQL，附带前端 Web 界面，最终前后端代码打包至同一个 Docker 镜像中发布。

### 技术栈
*   **后端开发语言**: Go
*   **后端框架**: Kratos v3 (微服务框架，严格遵循 DDD 分层)
*   **接口定义**: Protobuf (gRPC + HTTP Gateway)
*   **ORM 框架**: Gorm
*   **缓存中间件**: Redis (用于多用户会话热数据与配置缓存)
*   **关系型数据库**: PostgreSQL (全量持久化，启用 pgvector 扩展支持向量检索)
*   **容器化**: Docker (前后端单镜像打包)
*   **Agent 框架**: Eino
*   **前端技术栈**: React + Vite + TypeScript + TailwindCSS + shadcn/ui
*   **未来扩展**: 飞书开放平台 SDK (事件回调与消息推送)

---

## 二、 架构与工程规范要求

### 1. 严格 DDD 分层架构
*   **API 层 (`/api`)**：定义 `.proto` 文件。接口请求必须携带 `user_id` (或 token 解析出的用户信息)。
*   **Service 层 (`/internal/service`)**：处理身份鉴权，将 `user_id` 注入 Context 透传至 BIZ 层。
*   **Biz 层 (`/internal/biz`)**：处理核心业务逻辑，所有会话、记忆操作均需校验 `user_id` 确保隔离。
*   **Data 层 (`/internal/data`)**：实现 Gorm 与 Redis 操作，SQL 查询和 Redis Key 必须携带用户维度。
*   **飞书适配层 (`/pkg/feishu`)**：解析飞书事件回调，将飞书 `open_id` 映射为系统内部 `user_id`。

### 2. 多用户隔离与数据持久化策略
*   **多租户/多用户隔离**：
    *   平台级配置（如 LLM Provider, SKILL 模板）为全局共享。
    *   用户级数据（Agent 实例配置、Session 会话、Memory 记忆）严格按 `user_id` 隔离。
    *   查询任何用户数据时，Gorm 须强制附加 `WHERE user_id = ?` 条件。
*   **飞书多端会话映射**：支持飞书单聊（按 `open_id` 映射用户）和群聊（按 `chat_id` 映射为独立的会话上下文，群内多人发言在消息体中标注发言人）。
*   **全量 PG 存储**：全量对话、向量记忆、结构化事实、执行日志均落盘 PG。
*   **Redis 缓存策略**：Key 设计必须包含用户维度，如 `session:{user_id}:{session_id}:messages`。

### 3. 单元测试要求
*   核心层单测覆盖率需 > 80%。使用 `gomock` 生成 Data 层 Mock，确保 BIZ 层测试脱离真实 DB/Redis。

---

## 三、 上下文管理与会话记忆算法选型 (多用户维度)

工程级 Agent 的上下文管理采用**分层记忆架构**，在多用户场景下，记忆的读写必须严格绑定用户与当前会话。

### 1. 算法选型与职责划分
| 算法层 | 核心算法 | 存储介质 | 多用户隔离说明 |
|--------|----------|----------|----------------|
| **工作记忆** | 滑动窗口 + Token 预算管理 | Redis | Key: `session:{user_id}:{session_id}:messages`。确保不同用户、不同会话的最近 N 轮原文互不干扰。 |
| **短期记忆** | 递归摘要压缩 | PG + Redis | 摘要按 `session_id` 归属，PG 表含 `user_id` 字段。群聊场景下，摘要需融合多发言人意图。 |
| **长期记忆** | 向量语义检索 + MMR | PG (pgvector) | 检索时 SQL 附加 `WHERE user_id = ? AND ...`，确保A用户无法召回B用户的隐私记忆。 |
| **结构化记忆** | LLM 事实提取 (JSON KV) | PG | 提取的用户画像绑定 `user_id`。在群聊中，若需提取特定用户偏好，需结合发言人 ID 提取。 |

### 2. 上下文组装流程
每次 LLM 调用前，Harness 按以下顺序组装上下文：
1.  **System Prompt**：Agent 角色设定 + SKILL 指令 + 当前用户画像（按 `user_id` 从 PG 查询）。
2.  **长期记忆检索**：将当前用户输入 embedding，在 PG 中检索 `user_id` 匹配且语义相似的 Top-K 记忆。
3.  **滚动摘要**：从 Redis 读取当前 `session_id` 的摘要背景。
4.  **最近 N 轮原文**：从 Redis 读取当前 `session_id` 的最近 6~12 条消息（群聊消息带发言人标识）。
5.  **当前用户输入**：本轮 Query（飞书群聊中为具体某用户的发言）。

---

## 四、 Agent 三种模式工程级实现细节

### 1. ReAct 模式 (默认)
*   **多轮上下文**：按 `user_id` + `session_id` 拉取历史组装进 Eino。
*   **防死循环与去重**：Redis Key 需区分会话，记录工具调用 Hash，防止跨会话误拦截。单轮 ReAct 结束后，明细全量落盘 PG。

### 2. Plan-and-Execute 模式
*   **跨多轮恢复**：Planner 生成的步骤列表 (`[]Step`) 全量存入 PG，关联 `user_id` 和 `session_id`。用户（如通过飞书）再次触发时，恢复未完成的 Plan 状态。

### 3. Supervisor 模式
*   **子 Agent 上下文隔离**：Supervisor 委派任务时，为子 Agent 生成独立的 Session ID，其上下文归属于主用户，但独立存储，避免主会话上下文爆炸。利用 Eino 并发分支同时调度多个 Worker。

---

## 五、 数据库核心表结构设计 (PostgreSQL)
*以下为关键实体设计，所有用户相关表均包含 `user_id` 字段。*

*   `users`: `id`, `username`, `password_hash`, `feishu_open_id` (用于飞书映射), `created_at`
*   `llm_providers`: `id`, `name`, `api_key_encrypted`, `base_url`, `created_at`
*   `agents`: `id`, `user_id`, `name`, `mode` (react/plan/super), `llm_provider_id`, `created_at`
*   `skills`: `id`, `name`, `description`, `prompt_template`, `parameters_json`
*   `mcps`: `id`, `name`, `endpoint`, `auth_type`, `auth_credentials`
*   `sessions`: `id`, `user_id`, `agent_id`, `title`, `feishu_chat_id` (若来自飞书群聊), `created_at`
*   `messages`: `id`, `session_id`, `user_id`, `role`, `content`, `tool_calls`, `token_count`, `created_at`
*   `agent_execution_logs`: `id`, `session_id`, `user_id`, `agent_mode`, `thought`, `action`, `observation`, `created_at`
*   `memory_vectors`: `id`, `user_id`, `session_id`, `content`, `embedding` (vector(1536)), `metadata` (jsonb), `created_at`
*   `session_summaries`: `id`, `session_id`, `user_id`, `summary_text`, `source_message_ids`, `created_at`

---

## 六、 详细开发计划 (预计 16 周)

### 阶段一：基础设施与多用户模型搭建 (第 1-2 周)
*   **任务 1**：初始化 Kratos v3 项目骨架。
*   **任务 2**：编写 `docker-compose.yml` (PG 启用 pgvector, Redis)。
*   **任务 3**：设计支持多用户的 PG 表结构，使用 Gorm 定义 Model。
*   **任务 4**：实现 JWT 鉴权中间件，从 Token 解析 `user_id` 并注入 Context。
*   **任务 5**：编写所有核心业务的 `.proto` 文件。

### 阶段二：Data 与 Biz 层开发及单测 (第 3-4 周)
*   **任务 1**：在 Data 层实现 Repo 接口，确保所有查询方法强制携带 `user_id`。
*   **任务 2**：在 Biz 层实现领域逻辑。
*   **任务 3**：使用 `gomock` 生成 Data 层 Mock，完成 Biz 层单测覆盖。

### 阶段三：记忆引擎与 Eino 适配 (第 5-6 周)
*   **任务 1**：开发 `/pkg/memory`，实现 Token 计数与带 `user_id` 隔离的滑动窗口管理。
*   **任务 2**：实现递归摘要压缩异步 goroutine。
*   **任务 3**：实现向量语义检索，确保检索范围限定在当前 `user_id`。
*   **任务 4**：开发 `/pkg/einoext`，实现上下文组装器及 DB 配置到 Eino 对象转化。

### 阶段四：多轮对话与 Agent 模式落地 (第 7-8 周)
*   **任务 1**：实现 ReAct、Plan-and-Execute、Supervisor 三种模式。
*   **任务 2**：处理 SSE 流式输出，打通多用户多会话并发链路。
*   **任务 3**：完善各模式及 Memory 层对应单测。

### 阶段五：前端 Web 界面开发 (第 9-11 周)
*   **任务 1**：初始化 React + Vite 项目，配置路由与状态管理。
*   **任务 2**：开发登录/注册界面（用于本地测试多用户隔离）。
*   **任务 3**：开发管理后台：LLM、SKILL、MCP、Agent 编排界面。
*   **任务 4**：开发对话界面：多会话列表管理、流式打字机效果渲染、执行历史树展示。

### 阶段六：飞书集成适配层开发 (第 12-13 周)
*   **任务 1**：开发 `/pkg/feishu` 包，集成飞书 SDK，配置事件订阅。
*   **任务 2**：实现飞书身份映射：飞书 `open_id` -> 系统自动查找或注册 `user_id`。
*   **任务 3**：实现飞书群聊支持：将群 `chat_id` 映射为系统 `session_id`，解析消息体提取发言人标识。
*   **任务 4**：打通飞书消息回调 -> 内部 Chat 接口 -> 流式生成 -> 飞书回复消息的闭环。

### 阶段七：前后端联调与单镜像部署 (第 14-15 周)
*   **任务 1**：前后端真实接口联调，多用户隔离测试。
*   **任务 2**：编写多阶段 `Dockerfile`，前端使用 Node 编译，后端使用 Go 编译并通过 `embed` 内嵌前端静态文件。
*   **任务 3**：本地 Docker 环境运行验证（Web 端 + 飞书端双通）。

### 阶段八：测试、优化与发布 (第 16 周)
*   **任务 1**：端到端功能测试，多用户并发对话压测。
*   **任务 2**：完善全链路 Trace，按 `user_id` 聚合追踪链路。
*   **任务 3**：编写部署文档与 README，完成交付。

---

## 七、 预期目录结构 (全栈单仓库)

```text
harness-agent/
├── api/ # Protobuf 定义与生成代码
│ └── agent/v1/ # Agent 相关的 gRPC/HTTP 接口定义
├── web/ # 前端 React 源码
│ ├── src/
│ │ ├── components/ # 通用组件 (Chat UI, 表单等)
│ │ ├── pages/ # 页面 (登录, 管理后台, 对话界面)
│ │ ├── services/ # 接口请求封装
│ │ └── App.tsx # 前端入口
│ ├── package.json
│ └── vite.config.ts
├── internal/ # 后端内部业务逻辑 (DDD)
│ ├── biz/ # 领域逻辑层 (含 user_id 隔离逻辑与单测)
│ ├── data/ # 基础设施层 (Gorm/Redis 强制附加 user_id)
│ ├── service/ # 应用服务层 (处理鉴权, user_id 提取, SSE流转发)
│ └── server/ # HTTP/gRPC Server 配置
│ └── static/ # 预留目录，用于存放 Go embed 的前端静态文件
├── pkg/ # 公共包
│ ├── memory/ # 记忆与上下文管理 (多用户维度, 滑动窗口, 摘要, 向量检索)
│ ├── einoext/ # Eino 适配层 (ReAct, Plan, Super, Context Builder)
│ └── feishu/ # 飞书集成适配层 (身份映射, 群聊解析, 事件回调)
├── Dockerfile # 多阶段构建：Node前端编译 + Go后端embed编译
├── go.mod # Go 依赖管理
└── docker-compose.yml # 编排 PG, Redis 及单镜像服务
```

---


## 八、 单镜像 Dockerfile 设计示例
以下 Dockerfile 采用多阶段构建，将前端静态文件通过 Go 1.16+ 的 `embed` 特性直接打包进二进制文件，实现真正的单容器部署：

``` dockerfile

===== Stage 1: 前端构建 =====
FROM node:18-alpine AS web-builder
WORKDIR /app/web
COPY web/package.json web/package-lock.json ./
RUN npm install
COPY web/ ./
RUN npm run build

构建产物将生成在 /app/web/dist 目录下
===== Stage 2: 后端构建 =====
FROM golang:1.21-alpine AS server-builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .

将前端构建产物拷贝至后端 embed 静态资源目录
COPY --from=web-builder /app/web/dist ./internal/server/static/dist

静态编译 Go 二进制
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags=“-s -w” -o /app/harness-agent ./cmd/server

===== Stage 3: 最终运行镜像 =====
FROM alpine:latest
WORKDIR /app

安装必要的 CA 证书，防止调外部 LLM API 时 TLS 失败
RUN apk --no-cache add ca-certificates tzdata
ENV TZ=Asia/Shanghai
COPY --from=server-builder /app/harness-agent .
EXPOSE 8080
CMD [“./harness-agent”]
```
**工程实现提示**：在 Kratos 的 HTTP Server 中，需配置一个静态文件路由（如 `/web/*`），使用 `http.FileServer(http.FS(embedStaticFiles))` 托管前端 `dist` 下的 `index.html` 及资源文件。前端路由需配置为 `BrowserRouter` 并设置 `basename`，确保刷新不 404。

---

## 九、 可观测性与安全要求

1. **全链路追踪**：集成 OpenTelemetry。在 Kratos HTTP Middleware 中提取或生成 TraceID，并通过 Eino 的 Context 传递至 LLM 调用和 Tool 执行环节，确保一个多轮对话请求的完整链路在 Jaeger 中可查。
2. **指标监控**：暴露 Prometheus Metrics（如 `agent_llm_tokens_total`, `agent_tool_call_duration_seconds`），按 `agent_mode` 打标签，便于监控不同模式下的 Token 消耗和工具延迟。
3. **安全与沙箱**：针对通过 MCP 接入的外部工具，尤其是涉及代码执行或系统命令的工具，必须在受限的 Docker 容器或 gVisor 沙箱中执行，限制网络出站和文件系统写权限。
4. **多租户限流**：基于 Kratos 的限流中间件 + Redis，针对不同 `user_id` 实现令牌桶限流，防止单一用户高频调用拖垮全局服务。