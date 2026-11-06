-- 000048：飞书话题绑定创建包含 app_id 的根消息别名唯一索引。

CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS external_binding_root_app_key
  ON external_conversation_bindings (provider, tenant_key, app_id, chat_id, root_message_id)
  WHERE root_message_id IS NOT NULL AND status <> 'unbound';
