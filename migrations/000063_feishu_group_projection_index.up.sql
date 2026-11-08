CREATE INDEX CONCURRENTLY IF NOT EXISTS messages_feishu_group_message_idx
  ON messages (feishu_group_message_id) WHERE feishu_group_message_id IS NOT NULL;
