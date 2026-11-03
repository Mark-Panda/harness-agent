# GA Checklist（Release C）

版本：待填写镜像 digest / 版本号。评估日期：2026-11-03。

判定规则：任何一项未通过或证据不足即为阻断项；不得口头豁免。签字完成前不得承接生产流量。

## 1. 功能与数据

| 检查项 | 证据 | 结论 |
| --- | --- | --- |
| 注册/登录/刷新/注销/全端撤销 | `internal/service/auth_test.go`、`internal/platform/auth` 测试 | 通过 |
| 一次性改密/找回、禁用用户、邀请注册与管理员引导 | `internal/service/auth_password_test.go`、`internal/data/user_password_integration_test.go`、`cmd/admin` | 通过 |
| 多用户隔离与 RLS | `internal/data/domain_rls_integration_test.go` | 通过 |
| 三种 Agent 模式与恢复 | `internal/biz` 测试、`internal/data/*integration_test.go` | 通过 |
| Session/Memory 导出与异步删除 | `internal/server/account_test.go`、`internal/data/deletion_integration_test.go` | 通过 |
| 全账户导出/删除 | `internal/data/deletion_integration_test.go` | 通过 |
| 飞书单聊/群话题与幂等消费 | `internal/biz/feishu_unit_test.go`、`internal/data/feishu_repo_integration_test.go` | 通过 |

## 2. 质量门禁

| 检查项 | 命令 | 结论 |
| --- | --- | --- |
| 生成物一致 | `make generate-check` | 通过 |
| Lint / 类型检查 | `make lint` | 通过 |
| 单元测试 | `make test-unit` | 通过 |
| 竞态测试 | `make test-race` | 通过 |
| 集成/契约/前端 | `make test-integration test-contract test-web` | 通过 |
| Protobuf 兼容 | `make test-breaking` | 通过 |
| 迁移守卫与不可变性 | `make test-migration`、`internal/data/migration_integration_test.go` | 通过 |
| 许可证合规 | `make test-license` | 通过（Go 74 个模块、npm 351 个包） |
| 覆盖率门禁 | `make test-coverage` | 通过（biz 81.6%、service 95.0%、auth 91.7%、agent 83.4%、mcp 81.6%、einoext 90.7%、memory 92.2%） |
| Agent 质量评测 | `make test-quality`、`docs/quality/agent-eval-baseline.md` | 通过（确定性黄金集/对抗集；实时模型验收仍属 Release B） |

## 3. 安全与合规

| 检查项 | 证据 | 结论 |
| --- | --- | --- |
| SAST/依赖/镜像/IaC/Fuzz | `make test-security`、`docs/security/security-review.md` | 通过 |
| DAST 跨用户与权限 | `docs/security/security-review.md` | 通过 |
| 权限矩阵 | `docs/security/permission-matrix.md` | 通过 |
| 第三方数据边界 | `docs/security/third-party-compliance.md` | 通过 |
| 密钥吊销演练 | `docs/runbooks/key-revocation.md` | 通过 |
| 生产 TLS/网络分段/Redis 认证 | `docs/runbooks/deployment.md`、`deploy/network/README.md` | 通过 |
| 安全团队人工签字 | 独立审查记录 | 待签字（阻断项） |

## 4. 可靠性与容量

| 检查项 | 证据 | 结论 |
| --- | --- | --- |
| 故障注入与降级 | `docs/runbooks/failure-degradation.md`、`make test-chaos` | 通过 |
| 容量与 1.5 倍峰值 | `docs/capacity/README.md` | 通过 |
| 长时间稳定性 | `docs/capacity/soak-result.md` | 通过 |
| 备份恢复与 PITR | `deploy/backup/README.md`、演练记录 | 通过 |
| 备份过期清除 | `go test ./deploy/backup` | 通过 |
| 滚动发布与优雅退出 | `docs/runbooks/rolling-deployment.md` | 通过 |

## 5. 发布流程

| 检查项 | 证据 | 结论 |
| --- | --- | --- |
| SBOM/Provenance/签名/审批 digest | `scripts/sbom.sh`、`scripts/supply_chain.sh`、`deploy/signing/verify.sh` | 通过 |
| 灰度/Smoke/自动停止/回滚 | `scripts/acceptance.sh`、`scripts/canary_smoke.sh`、`docs/runbooks/release-acceptance.md` | 通过 |
| 发布兼容与加法迁移 | `internal/data/migration_guard_test.go` | 通过 |
| Release B 预发布演练 | 预发布环境双副本、全量回归、Agent 质量评测、迁移/回滚/恢复演练；本地发布链路演练已通过（canary 8083、签名审批、SBOM、备份恢复 RTO 1s） | **未完成（阻断项：缺预发布环境与实时模型验收）** |

## 6. 运营准备

| 检查项 | 证据 | 结论 |
| --- | --- | --- |
| 部署/配置/扩容 | `docs/runbooks/deployment.md` | 通过 |
| 备份恢复 | `docs/runbooks/backup-restore.md` | 通过 |
| 故障处理 | `docs/runbooks/failure-degradation.md`、`harness-agent-alerts.md` | 通过 |
| 安全事件 | `docs/runbooks/security-incident.md` | 通过 |
| 值班升级 | `docs/runbooks/oncall-escalation.md` | 通过 |
| 复盘模板 | `docs/runbooks/postmortem.md` | 通过 |

## 7. 阻断项与放行条件

| 编号 | 阻断项 | 负责人 | 放行条件 |
| --- | --- | --- | --- |
| BLK-01 | 计划第 359 项 Release B 未在预发布环境完成双副本、全量回归、Agent 实时模型评测与恢复演练 | 发布负责人 | 留存预发布验收记录与签字 |
| BLK-02 | 安全团队人工复核未签字 | 安全负责人 | 书面审查结论与风险接受清单 |

## 8. 签字

- 发布负责人：
- 技术负责人：
- 安全负责人：
- 合规负责人：
- 日期（UTC）：

未完成上述签字前，Release C 不得上线；灰度扩量必须逐档观察并保留自动停止与回滚能力。
