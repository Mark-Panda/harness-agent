-- 按消息聚合附件，仅索引已绑定消息的行。
CREATE INDEX CONCURRENTLY IF NOT EXISTS attachments_message_idx ON attachments (message_id) WHERE message_id IS NOT NULL;
