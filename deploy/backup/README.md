# Harness Agent 备份与恢复

本目录提供加密逻辑备份、跨故障域镜像、PITR 基础备份与恢复验收脚本。

## 目标

| 指标 | 目标 | 实现 |
| --- | --- | --- |
| RPO（逻辑备份） | ≤ 15 分钟 | 备份任务每 15 分钟执行一次；实际恢复点取决于最后一次成功备份 |
| RPO（PITR） | ≤ 60 秒 | `archive_mode=on` + `archive_timeout=60`，WAL 持续归档 |
| RTO | ≤ 30 分钟 | 空库逻辑恢复 + 自动验收；本仓库小数据集实测约 2 秒 |
| 保留期 | 逻辑备份 30 天；WAL 7 天 | `BACKUP_RETENTION_DAYS` 与 WAL 归档清理任务 |
| 跨故障域 | 至少 1 份异副本 | `BACKUP_MIRROR_DIR` 指向不同磁盘/可用域 |

说明：Redis 只作为缓存，不参与事实恢复。恢复后 Redis 为空即可，热会话会按需从 PostgreSQL 重建。

## 一次性准备

```bash
make build
mkdir -p backups
openssl rand -base64 32 > backups/backup.key
chmod 600 backups/backup.key
```

密钥文件已被 `.gitignore` 排除；生产环境应使用密钥管理服务注入，并按季度轮换。Manifest 只记录 `key_id`，不记录密钥。

## 加密逻辑备份

```bash
BACKUP_MIRROR_DIR=/mnt/backup-mirror deploy/backup/backup.sh
```

流程：`pg_dump -Fc` → AES-256-GCM 分块加密 → 采集迁移版本/表行数/RLS/业务一致性基线 → 生成 Manifest → 复制到镜像目录并逐字节比对 → 清理超过保留期的备份。

- 加密文件：`backups/harness-<UTC>.dump.enc`
- 验收基线：`backups/harness-<UTC>.manifest.json`
- 备份失败会删除半成品；Manifest 与加密文件必须同时存在才可用于恢复。

## 从空环境恢复

```bash
BACKUP_FILE=backups/harness-<UTC>.dump.enc \
MANIFEST_FILE=backups/harness-<UTC>.manifest.json \
TARGET_DB=harness_restore \
deploy/backup/restore.sh
```

脚本执行：

1. 解密并校验明文哈希与 Manifest 一致。
2. 目标库不存在时创建；已存在需显式 `RECREATE_TARGET=true` 才会删除重建。
3. `pg_restore` 导入数据、索引、RLS 策略与 ACL。
4. `harness-backup verify` 校验迁移版本、dirty 标记、全部表行数、RLS 表集合、策略数量与孤儿记录。
5. 输出实际 RTO。

### 验收口径

`harness-backup verify` 失败即恢复失败，禁止上线。检查项：

- `schema_migrations.version` 与 `dirty` 完全一致。
- `public` 下全部表行数完全一致，且表集合无缺失/多余。
- 启用 RLS 的表集合与策略总数完全一致。
- 孤儿记录必须为 0：消息/运行/步骤/向量/摘要/计划与会话或运行关联完整。

## PITR（时间点恢复）

启用 WAL 归档：

```bash
docker compose -f docker-compose.yml -f deploy/compose/pitr.yaml up -d postgres
```

执行基础备份：

```bash
deploy/backup/pitr-basebackup.sh
```

产物：`backups/pitr/base-<UTC>.tar.gz.enc`、`pg_wal-<UTC>.tar.gz.enc`、Manifest（含起止 LSN 与 timeline）。

恢复步骤（在隔离环境执行，禁止直接覆盖生产库）：

1. 解密基础备份与 WAL 包到临时目录。
2. 将 `base.tar.gz` 解压到新的 `PGDATA`，清空目标 `pg_wal` 后放入 WAL 包。
3. 写入 `postgresql.auto.conf`：
   ```
   restore_command = 'cp /var/lib/postgresql/wal-archive/%f %p'
   recovery_target_time = '<UTC 时间点>'
   recovery_target_action = 'promote'
   ```
4. 启动实例并等待 `pg_isready`，确认 `pg_last_wal_recovery_lsn` 与目标 LSN 接近。
5. 对恢复库执行 `harness-backup verify`，再切流。

WAL 归档清理：保留 7 天，由部署侧定时删除 `wal-archive` 中过期段；删除前确认基础备份仍覆盖保留窗口。

## 定时任务建议

```cron
*/15 * * * * cd /srv/harness-agent && BACKUP_MIRROR_DIR=/mnt/backup-mirror deploy/backup/backup.sh >> /var/log/harness-backup.log 2>&1
0 3 * * * cd /srv/harness-agent && deploy/backup/pitr-basebackup.sh >> /var/log/harness-pitr.log 2>&1
```

每季度至少执行一次空环境恢复演练，并记录实际 RPO/RTO；演练失败视为发布阻断项。
