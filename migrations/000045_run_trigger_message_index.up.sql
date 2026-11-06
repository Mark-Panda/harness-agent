-- 触发消息按 (user_id, trigger_message_id) 查找；CONCURRENTLY 需独立迁移文件，
-- 与 golang-migrate 单文件单语句的执行方式保持一致，避免在隐式事务中建索引阻塞写入。
CREATE INDEX CONCURRENTLY IF NOT EXISTS agent_runs_trigger_message_idx ON agent_runs (user_id, trigger_message_id)
  WHERE trigger_message_id IS NOT NULL;
