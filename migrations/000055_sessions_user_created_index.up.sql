-- 会话列表按 (user_id, created_at) 分页；CONCURRENTLY 需独立迁移文件，
-- 与 golang-migrate 单文件单语句的执行方式保持一致，避免在隐式事务中建索引阻塞写入。
CREATE INDEX CONCURRENTLY IF NOT EXISTS sessions_user_created_idx ON sessions (user_id, created_at DESC, id DESC)
  WHERE parent_session_id IS NULL;
