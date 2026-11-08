-- 子会话按父会话查找；CONCURRENTLY 需独立迁移文件，
-- 与 golang-migrate 单文件单语句的执行方式保持一致，避免在隐式事务中建索引阻塞写入。
CREATE INDEX CONCURRENTLY IF NOT EXISTS sessions_parent_session_idx ON sessions (parent_session_id)
  WHERE parent_session_id IS NOT NULL;
