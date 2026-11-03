-- 说明：将飞书话题唯一索引限定为非解绑绑定。
-- 关闭（closed）与归档（archived）绑定仍占用话题键，保证同一话题不会被并发创建出多个活跃会话；
-- 解绑（unbound）绑定不再参与匹配，允许同一话题的下一条 @ 消息创建全新会话，同时保留历史绑定用于审计追溯。
-- 关键步骤：先放宽状态检查约束，否则写入 unbound 会被 CHECK 拒绝。
ALTER TABLE external_conversation_bindings
  DROP CONSTRAINT IF EXISTS external_conversation_bindings_status_check;
ALTER TABLE external_conversation_bindings
  ADD CONSTRAINT external_conversation_bindings_status_check
  CHECK (status IN ('active', 'closed', 'archived', 'unbound'));

DROP INDEX IF EXISTS external_binding_thread_key;
DROP INDEX IF EXISTS external_binding_root_key;

CREATE UNIQUE INDEX external_binding_thread_key
  ON external_conversation_bindings (provider, tenant_key, chat_id, thread_id)
  WHERE thread_id IS NOT NULL AND status <> 'unbound';

CREATE UNIQUE INDEX external_binding_root_key
  ON external_conversation_bindings (provider, tenant_key, chat_id, root_message_id)
  WHERE root_message_id IS NOT NULL AND status <> 'unbound';

COMMENT ON COLUMN external_conversation_bindings.status IS '绑定状态：active 活跃、closed 已关闭、archived 已归档、unbound 已解绑';
