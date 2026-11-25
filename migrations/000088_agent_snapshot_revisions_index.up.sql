-- 助手快照历史按用户、助手与时间读取；CONCURRENTLY 需独立迁移文件。
CREATE INDEX CONCURRENTLY IF NOT EXISTS agent_snapshot_revisions_agent_idx
  ON agent_snapshot_revisions (user_id, agent_id, created_at DESC);
