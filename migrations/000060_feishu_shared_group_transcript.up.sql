-- 共享群聊仅从已授权的飞书事件构建上下文。普通会话消息不能进入此表。
CREATE TABLE IF NOT EXISTS feishu_group_messages (
  id uuid PRIMARY KEY,
  binding_id uuid NOT NULL REFERENCES external_conversation_bindings(id) ON DELETE CASCADE,
  sender_user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  external_message_id text NOT NULL,
  speaker_open_id text NOT NULL,
  content text NOT NULL,
  occurred_at timestamptz NOT NULL,
  projection_session_id uuid REFERENCES sessions(id) ON DELETE SET NULL UNIQUE,
  reply_text text NOT NULL DEFAULT '',
  replied_at timestamptz,
  UNIQUE (binding_id, external_message_id)
);
ALTER TABLE feishu_group_messages ENABLE ROW LEVEL SECURITY;
ALTER TABLE feishu_group_messages FORCE ROW LEVEL SECURITY;
CREATE POLICY feishu_group_messages_system ON feishu_group_messages FOR ALL TO harness_app
  USING (nullif(current_setting('app.system', true), '') = 'on')
  WITH CHECK (nullif(current_setting('app.system', true), '') = 'on');
GRANT SELECT, INSERT, UPDATE, DELETE ON feishu_group_messages TO harness_app;

ALTER TABLE messages ADD COLUMN IF NOT EXISTS feishu_group_message_id uuid REFERENCES feishu_group_messages(id) ON DELETE CASCADE;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS feishu_group_projection boolean NOT NULL DEFAULT false;
ALTER TABLE sessions ADD CONSTRAINT sessions_feishu_group_projection_check
  CHECK (NOT feishu_group_projection OR (source = 'feishu' AND agent_id IS NULL AND memory_policy IN ('group', 'disabled'))) NOT VALID;
