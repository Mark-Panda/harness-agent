# 密钥轮换与吊销 Runbook

适用对象：JWT 访问令牌、刷新令牌、Provider/MCP 凭据、备份加密密钥、飞书事件加密密钥。所有操作必须两人复核并保留审计记录；紧急吊销可先执行止损，再补录审计。

## 1. 触发条件与分级

| 场景 | 级别 | 首选动作 |
| --- | --- | --- |
| 计划内季度轮换 | 常规 | 平滑轮换，保留旧密钥验证窗口 |
| 密钥疑似泄露 | 紧急 | 立即吊销旧密钥并使受影响会话失效，再评估影响范围 |
| 刷新令牌重用检测命中 | 自动 | 整族撤销并提示用户重新登录 |
| 备份介质遗失 | 紧急 | 轮换备份密钥，确认遗失介质上的备份已过期清除 |
| 飞书加密密钥泄露 | 紧急 | 移除旧加密密钥并重配事件订阅 |

## 2. JWT 访问令牌

### 2.1 平滑轮换

1. 在配置中把新密钥设为 `JWT_SIGNING_KEY_ID` / `JWT_SIGNING_KEY_SECRET`（新密钥至少 32 字节）。
2. 把旧密钥加入 `auth.jwt_verification_keys`（YAML/Secret Manager 注入），保留至少 `AccessTokenTTL`（15 分钟）加时钟偏差窗口。
3. 滚动重启 API 副本；新令牌使用新 `kid` 签发，旧 `kid` 仍可验证。
4. 观察 15 分钟后确认不再有旧 `kid` 令牌，再移除旧验证密钥并再次滚动。

### 2.2 紧急吊销

1. 从 `auth.jwt_verification_keys` 移除泄露密钥，并把新密钥设为唯一签发密钥。
2. 滚动重启全部 API 副本；旧 `kid` 令牌立即全部被拒绝（签发与验证均无该密钥）。
3. 如需同时吊销特定用户的访问令牌，由管理员在受控会话执行并记录审计：

```sql
-- 仅通过受控管理连接执行；执行后该用户全部访问令牌与刷新令牌失效。
UPDATE users SET credential_version = credential_version + 1, updated_at = now()
WHERE id = '<user_id>';
```

4. 通知受影响用户重新登录；检查审计中 `auth.refresh.issued`、`auth.logout_all` 与登录失败事件。

### 2.3 验证证据

- `internal/platform/auth/jwt_test.go` 的 `TestJWTKeyRotation`：新密钥可验证旧 `kid`，未知 `kid` 被拒绝。
- `internal/platform/auth/jwt_edges_test.go`：缺少 `kid`、非法算法、过期令牌均被拒绝。
- 账号删除会将 `credential_version` 加一并令用户状态变为 `deleted`，见 `internal/data/deletion_integration_test.go`。

## 3. 刷新令牌

1. 单用户全端退出：调用 `POST /api/v1/auth/logout-all`，撤销该用户全部令牌家族。
2. 重用检测：轮换时若旧令牌已被使用，存储层调用 `RevokeFamily(reuseDetected=true)`，整族撤销并记录审计。
3. 紧急吊销特定家族：由管理员通过受控管理连接撤销；生产环境优先使用全端退出，避免遗漏。

验证证据：`internal/data/user_repo_integration_test.go` 的 `TestRefreshTokenAtomicRotationAndRevocation` 覆盖原子轮换、整族撤销与重用标记；`internal/biz` 单测覆盖重用检测分支。

## 4. Provider / MCP 凭据加密密钥

凭据使用带 `key_version` 的 AES-256-GCM 密钥环，轮换流程：

1. 新增密钥并设为 `credentials.active_key_id` / `active_key_secret`。
2. 把旧密钥加入 `credentials.previous_keys`，保留解密能力。
3. 滚动重启；新写入与更新凭证使用新密钥。
4. 执行 `bin/harness-reencrypt`（默认 dry-run），确认待处理数量后加 `-apply` 逐行重加密；每行写入 `credential.reencrypted` 审计 Outbox，不记录明文。
5. 确认数据库中不再存在旧 `key_version` 记录后再移除旧密钥：

```sql
SELECT key_version, count(*) FROM llm_providers GROUP BY key_version;
SELECT key_version, count(*) FROM mcps GROUP BY key_version;
```

验证证据：`internal/platform/crypto/keyring_test.go` 的 `TestKeyringOpensCiphertextsAfterRotation` 验证旧密文在新密钥环下仍可解密，未知版本被拒绝。

## 5. 备份加密密钥

1. 生成新密钥：`openssl rand -base64 32 > backups/backup-<新版本>.key`，权限 `0600`。
2. 更新 `BACKUP_ENCRYPTION_KEY_FILE` 与 `BACKUP_KEY_ID`，执行一次完整备份并核对 Manifest 中的 `key_id`。
3. 保留旧密钥直到旧备份按 `BACKUP_RETENTION_DAYS` 过期清除；恢复旧备份时必须使用其 Manifest 对应的密钥。
4. 介质遗失或密钥泄露时：立即停止使用该密钥的新备份，执行过期清除，并对受影响时间窗内的备份按事件流程处置。

验证证据：`deploy/backup/backup_test.go` 的 `TestExpireScriptRemovesOnlyExpiredBackupArtifacts` 实际执行过期清除；`internal/platform/backup/crypto_test.go` 的 `TestDecryptRejectsTamperedTruncatedAndWrongKey` 验证错误密钥被拒绝。

## 6. 飞书事件加密密钥

1. 在飞书开放平台生成新 Encrypt Key。
2. 配置中把新密钥放在 `feishu.encrypt_keys` 首位，旧密钥保留在后；滚动重启。
3. 确认事件回调验签/解密成功后，移除旧密钥。
4. 泄露场景：立即移除旧密钥并重配订阅；检查 `event_id` 幂等与死信，确认没有遗漏事件。

验证证据：`pkg/feishu/feishu_test.go` 覆盖多密钥解密与旧密钥轮换；`pkg/feishu/fuzz_test.go` 覆盖畸形密文输入。

## 7. 演练记录

| 项目 | 方式 | 结果 |
| --- | --- | --- |
| JWT 平滑轮换与未知 `kid` 拒绝 | `go test ./internal/platform/auth -run TestJWT` | 通过 |
| 刷新令牌整族撤销与重用检测 | `REQUIRE_DATABASE=true go test -tags=integration ./internal/data -run TestRefreshTokenAtomicRotationAndRevocation` | 通过 |
| 凭据密钥轮换后旧密文可读 | `go test ./internal/platform/crypto -run TestKeyring` | 通过 |
| 备份错误密钥拒绝 | `go test ./internal/platform/backup -run TestDecrypt` | 通过 |
| 备份过期清除 | `go test ./deploy/backup -run TestExpire` | 通过 |
| 账号删除令令牌失效 | `REQUIRE_DATABASE=true go test -tags=integration ./internal/data -run TestDeletionExecutorDeletesAccount` | 通过 |
