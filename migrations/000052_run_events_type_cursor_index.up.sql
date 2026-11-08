-- 恢复路径按事件类型分页读取本 Run 事件，避免扫描海量流式增量事件。
CREATE INDEX CONCURRENTLY IF NOT EXISTS run_events_type_cursor_idx ON run_events (run_id, event_type, seq);
