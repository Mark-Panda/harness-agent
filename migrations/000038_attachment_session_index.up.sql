-- 会话内按时间读取附件列表与文本上下文。
CREATE INDEX CONCURRENTLY IF NOT EXISTS attachments_session_idx ON attachments (session_id, created_at);
