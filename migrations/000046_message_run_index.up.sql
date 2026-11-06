-- 按 (user_id, run_id) 回放本 Run 产物；与触发消息索引一样独立成文件在线创建。
CREATE INDEX CONCURRENTLY IF NOT EXISTS messages_run_idx ON messages (user_id, run_id)
  WHERE run_id IS NOT NULL;
