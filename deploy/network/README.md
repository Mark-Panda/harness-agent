# 生产网络分段与最小访问

## 分区

| 分区 | 组件 | 入站 | 出站 |
| --- | --- | --- | --- |
| 公网入口 | 负载均衡 / Ingress | 443 | 仅到 app 的 HTTPS |
| 应用区 | harness-agent app | 仅来自入口的 8080/HTTPS | PostgreSQL、Redis、LLM Provider、飞书 |
| 数据区 | PostgreSQL、Redis | 仅应用区与受控运维跳板 | 无公网出站 |
| 管理区 | 运维跳板 / VPN | 管理员 | 应用区、数据区 |

## 端口与访问控制

- 公网只暴露 443；应用容器不直接映射 5432、6379、9090。
- PostgreSQL/Redis 使用私有子网与安全组，仅允许 app 副本与迁移任务访问。
- Redis 必须启用 `requirepass`/ACL，禁止无认证访问；TLS 可用时优先 `rediss://`。
- `/health/*` 允许入口探测。
- `/api/v1/admin/*` 与 Break-Glass 仅允许管理区/VPN；入口层配置 IP 允许列表或 mTLS。
- 飞书回调域名单独放行，启用验签、时间窗与 event_id 幂等。

## 出站最小化

应用出站白名单：

| 目标 | 用途 | 端口 |
| --- | --- | --- |
| LLM Provider BaseURL 域名 | 模型调用 | 443 |
| `open.feishu.cn` | 飞书 Token/消息 | 443 |
| PostgreSQL/Redis 私网地址 | 数据访问 | 5432/6379 |

禁止应用访问云元数据地址（169.254.169.254）、Docker Socket 与公网任意地址。Sandbox Runner 单独部署在无出网网络，仅接收应用调用。

## 部署校验

```bash
docker compose up -d
curl -fsS https://<domain>/health/ready
```

- 应用容器在 `docker-compose.yml` 中已启用 `read_only`、`tmpfs /tmp`、`cap_drop: ALL`、`no-new-privileges`、内存/CPU 限制。
- TLS 启用时 `server.http.tls.enabled=true` 且证书由密钥管理挂载，最低 TLS 1.3。
- 数据库/备份静态加密由托管存储加密或卷加密提供；备份文件本身使用 AES-256-GCM。
