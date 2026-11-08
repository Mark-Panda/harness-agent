CREATE INDEX CONCURRENTLY IF NOT EXISTS feishu_group_messages_history_idx
  ON feishu_group_messages (binding_id, occurred_at DESC, external_message_id DESC);
