-- 000047：飞书话题绑定创建包含 app_id 的话题唯一索引。
-- CONCURRENTLY 需独立迁移文件，避免与事务型 DDL 混用。

CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS external_binding_thread_app_key
  ON external_conversation_bindings (provider, tenant_key, app_id, chat_id, thread_id)
  WHERE thread_id IS NOT NULL AND status <> 'unbound';
