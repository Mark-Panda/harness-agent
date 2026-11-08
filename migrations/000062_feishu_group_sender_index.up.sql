CREATE INDEX CONCURRENTLY IF NOT EXISTS feishu_group_messages_sender_idx
  ON feishu_group_messages (sender_user_id);
