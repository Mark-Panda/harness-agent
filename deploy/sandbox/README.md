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

内置高风险工具同样走 `/execute`，按 `tool` 名路由；`user_id` 与 `run_id`
为必填维度，Runner 必须按用户隔离工作区，禁止跨用户读取或复用文件：

```json
{"tool":"shell_exec","user_id":"<uuid>","run_id":"<uuid>","arguments":{"command":"ls -la","timeout_seconds":30},"risk_level":"high","timeout_ms":30000}
{"tool":"http_request","user_id":"<uuid>","run_id":"<uuid>","arguments":{"method":"GET","url":"https://example.com","headers":{"Accept":"application/json"}},"risk_level":"high","timeout_ms":30000}
{"tool":"fs_read_file","user_id":"<uuid>","run_id":"<uuid>","arguments":{"path":"README.md"},"risk_level":"low","timeout_ms":30000}
{"tool":"fs_write_file","user_id":"<uuid>","run_id":"<uuid>","arguments":{"path":"notes/a.txt","content":"hello"},"risk_level":"medium","timeout_ms":30000}
```

文件与检索工具固定为 `fs_read_file`、`fs_write_file`、`fs_edit_file`、
`fs_list_dir`、`fs_glob`、`fs_grep`；Runner 必须把它们限制在按 `user_id`
划分的工作目录内，并拒绝绝对路径与 `..` 越界。低风险工具（读取、列举、
glob、grep）不带审批，但同样不得在应用进程内执行。

Runner 对未注册的工具名必须返回 4xx，禁止回退到进程内执行。`shell_exec`
在 Runner 内以非 root、只读根文件系统执行，默认无网络；`http_request` 仅在
白名单出站策略下放行目标域名与端口，并限制重定向次数与响应大小。

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
