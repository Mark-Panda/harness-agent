-- 保留期清理按 received_at 分批删除；既有 (status, received_at) 复合索引无法支撑单列排序。
-- CONCURRENTLY 需独立迁移文件，避免在隐式事务中建索引阻塞写入。
CREATE INDEX CONCURRENTLY IF NOT EXISTS feishu_event_receipts_received_idx ON feishu_event_receipts (received_at);
