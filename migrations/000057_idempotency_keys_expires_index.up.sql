-- 过期幂等键清理按 expires_at 扫描；CONCURRENTLY 需独立迁移文件，
-- 与 golang-migrate 单文件单语句的执行方式保持一致，避免在隐式事务中建索引阻塞写入。
CREATE INDEX CONCURRENTLY IF NOT EXISTS idempotency_keys_expires_idx ON idempotency_keys (expires_at);
