# Sandbox Runner 部署基线

高风险工具（`high` / `critical`）在生产环境禁止在应用进程内执行。应用通过
`pkg/sandbox` 客户端调用独立 Sandbox Runner，契约如下：

```http
POST /execute
Authorization: Bearer <token>
Content-Type: application/json

{"tool":"<tool>","arguments":{...},"risk_level":"high","timeout_ms":30000}
```

经审批的 MCP 工具使用独立接口，鉴权与响应格式相同。

```http
POST /execute/mcp
Authorization: Bearer <token>
Content-Type: application/json

{"mcp":{"user_id":"<uuid>","server_id":"<uuid>","original_name":"<tool>","endpoint_fingerprint":"<sha256-hex>"},"arguments":{...},"risk_level":"high","timeout_ms":30000}
```

Runner 必须在该用户可用的 MCP 注册中查找 `server_id`，校验配置端点的 SHA-256 指纹，并仅调用匹配的 `original_name`。目标无法核验时返回 4xx。部署新版应用前需为 Runner 提供此接口，应用收到接口错误时会停止调用。

成功响应：

```json
{"output":{...}}
```

业务失败：

```json
{"output":{...},"error_code":"tool_business_error","message":"脱敏摘要"}
```

Runner 必须使用独立容器执行，并至少满足以下隔离约束：

```bash
docker run --rm \
  --user 65534:65534 \
  --read-only \
  --tmpfs /tmp:rw,noexec,nosuid,size=64m \
  --cap-drop ALL \
  --security-opt no-new-privileges:true \
  --security-opt seccomp=deploy/sandbox/seccomp.json \
  --pids-limit 64 \
  --memory 256m --memory-swap 256m \
  --cpus 1 \
  --network none \
  --mount type=bind,src=/tmp/sandbox-work,dst=/work,readonly \
  sandbox-runner:latest
```

关键要求：

1. 非 root（固定 UID/GID 65534），禁止 privileged。
2. 根文件系统只读，只允许临时工作目录且 `noexec,nosuid`。
3. `cap-drop ALL` 与 `no-new-privileges`，禁止提权。
4. 使用最小 seccomp allowlist；不加载 Docker Socket，不挂载宿主敏感目录。
5. 默认 `--network none`；仅对明确需要网络的白名单工具开放出站，并限制目标域名与端口。
6. 强制 CPU、内存、PID 与墙钟时长限制，超时后由 Runner 杀死进程。
7. Runner 响应大小受应用侧 `max_response_bytes` 限制，错误只返回脱敏摘要。

应用配置：

```yaml
agent:
  sandbox:
    require_for_high_risk: true
    endpoint: https://sandbox.internal:8443
    token: ${SANDBOX_TOKEN}
    allow_insecure: false
    timeout: 30s
    max_response_bytes: 1048576
```

`require_for_high_risk=true` 且未配置 Runner 时，高风险工具调用会以
`ErrSandboxRequired` 失败关闭，绝不会回退到进程内执行。
