# 备份恢复 Runbook

完整命令与验收口径见 [deploy/backup/README.md](../../deploy/backup/README.md)。本 Runbook 只补充故障场景下的决策顺序。

## 1. 恢复决策

| 场景 | 首选恢复 | 说明 |
| --- | --- | --- |
| 单表/单用户误删 | 逻辑备份恢复到隔离库，按主键回填 | 禁止直接覆盖生产库 |
| 数据库损坏/实例丢失 | 最近逻辑备份 + `harness-backup verify` | Redis 空库即可，不参与事实恢复 |
| 需要任意时间点 | PITR 基础备份 + WAL 归档 | `recovery_target_time` 取故障前最近安全点 |
| 迁移失败 | 优先前滚修复 | 仓库不存在 down 迁移；禁止自动反向迁移 |

## 2. 逻辑恢复步骤

```bash
BACKUP_FILE=backups/harness-<UTC>.dump.enc \
MANIFEST_FILE=backups/harness-<UTC>.manifest.json \
TARGET_DB=harness_restore \
deploy/backup/restore.sh
```

1. 校验 Manifest 中的 `key_id` 与实际密钥一致；密钥已轮换时使用旧密钥解密。
2. 确认 `harness-backup verify` 输出表行数、RLS 表/策略数、迁移版本与孤儿检查全部通过。
3. 将恢复库切为生产前，执行登录、RLS 跨用户、历史 Run、向量检索与任务续跑 Smoke。
4. 记录实际 RPO（最后一次成功备份时间）与 RTO（脚本输出耗时）。

## 3. 失败处理

- 解密失败：检查密钥版本与 Manifest，禁止跳过哈希校验。
- 表行数或 RLS 不一致：视为恢复失败，保留现场并重新选择备份。
- PITR 无法达到目标时间：确认 WAL 归档完整性与 `archive_command` 错误日志；禁止用 `pg_resetwal` 强行启动。
- 恢复后队列任务重复：Worker 使用租约与幂等键；先确认重复副作用，再决定重放或丢弃。

## 4. 定期演练

- 每季度至少一次空环境恢复与一次 PITR 时间点恢复。
- 演练记录包含：备份时间、恢复目标时间、实际 RPO/RTO、验收命令输出、发现的缺陷与修复人。
- 演练失败即发布阻断项，不得以口头说明豁免。
