-- 000049：飞书话题绑定按应用与群聊查询的辅助索引。

CREATE INDEX CONCURRENTLY IF NOT EXISTS external_binding_app_chat_idx
  ON external_conversation_bindings (provider, tenant_key, app_id, chat_id, created_at DESC);
