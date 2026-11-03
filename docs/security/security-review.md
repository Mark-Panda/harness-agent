# 安全审查与扫描报告

日期：2026-11-03。范围：当前主线工作区。审查方式：独立只读代码审查 + SAST/依赖/镜像/IaC/密钥扫描 + API Fuzz + DAST 权限探测。

## 结论

- 未发现可证实的高危或中危安全问题。
- 依赖与标准库的可达漏洞为 0；容器镜像无 HIGH/CRITICAL 漏洞、无密钥泄露、无高危 IaC 配置。
- 跨用户隔离经 RLS 集成测试与 DAST 探测验证；跨用户泄露不豁免。
- 剩余风险：镜像基础包存在 2 个 LOW、6 个 UNKNOWN 漏洞（无修复版本或与运行路径无关），记录为书面接受，随基础镜像更新复查。

## 执行命令

```bash
make test-security                       # gofmt/vet/staticcheck/gosec/govulncheck/Trivy/Fuzz
FUZZ_TIME=5s SECURITY_IMAGE=harness-agent-app:latest make test-security
REQUIRE_DATABASE=true go test -tags=integration ./internal/data -run 'TestDomainRLS|Test.*Isolation'
```

## 扫描结果

| 检查 | 工具 | 结果 |
| --- | --- | --- |
| SAST | gosec（排除生成代码） | 0 个问题 |
| 静态分析 | staticcheck | 0 个问题 |
| 依赖/标准库漏洞 | govulncheck | 0 个可达漏洞 |
| 依赖/密钥/IaC | Trivy fs | go.mod、npm、Dockerfile 均为 0 |
| 容器镜像 | Trivy image | HIGH/CRITICAL = 0；LOW = 2；UNKNOWN = 6 |
| API Fuzz | Go fuzz（JSON/SSE/飞书解密） | 通过；修复 1 个真实问题 |

## 修复与风险接受

| 编号 | 级别 | 问题 | 处置 |
| --- | --- | --- | --- |
| SEC-01 | High | 依赖与标准库存在 21 个 HIGH CVE（crypto/net/text/grpc 等） | 升级到无漏洞版本并固定 Go 1.26.10；govulncheck 归零 |
| SEC-02 | High | Prometheus 标签接受非法 UTF-8 导致 panic；SSE 事件名可注入 CRLF | `safeLabel` 统一校验 UTF-8/长度；`safeSSEEventType` 仅允许白名单并用 `message` 兜底；Fuzz 复测通过 |
| SEC-03 | High（误报） | G115 整数转换告警 | 增加 `clampInt32` 饱和转换与长度校验；其余有界转换以 `#nosec` 附理由 |
| SEC-04 | High（误报） | G404 math/rand、G407 固定 IV、G101 指标键 | 仅用于退避抖动/测试辅助/指标常量，代码注释说明并保留审计记录 |
| SEC-05 | Medium | G304 文件路径 | 仅运维显式传入的配置/密钥/备份路径，已注释说明 |
| SEC-06 | Low | 基础镜像 LOW/UNKNOWN 漏洞 | 无修复版本或不在运行路径；记录接受，季度复扫 |

## 跨用户与权限验证

DAST 实测（Bearer Token）：

| 场景 | 结果 |
| --- | --- |
| 未认证访问 `/api/v1/sessions` | 401 |
| 用户 A 访问用户 B 的 Session | 404（RLS 隐藏，不泄露存在性） |
| 未认证访问 Admin Break-Glass | 401 |
| 普通用户访问 Admin Break-Glass | 403 |
| 篡改 JWT 签名 | 401 |

RLS 集成测试覆盖用户数据、Agent、Session、消息、Run、记忆、审计与 Outbox 的跨用户不可见性。

## 独立审查说明

独立只读审查重点检查了鉴权/刷新令牌、RLS 作用域、导出与 Break-Glass、Sandbox/审批、SSE 资源限制、飞书验签解密、备份密钥与审计链。审查未发现可证实高危问题；所有自动化扫描与 DAST 证据均随本报告归档。发布前仍需由安全团队完成一次人工签字复核，但当前不存在阻断级发现。
