-- 000018：飞书集成基础设施。
-- 说明：复用既有 feishu_event_receipts / external_conversation_bindings / user_identities，
-- 扩展事件处理状态与去重字段，新增外发回复表，并补齐跨群成员读取话题绑定的系统策略。

-- 事件回执增加处理中、忽略与死信状态，支持异步 Job 消费与死信重放。
ALTER TABLE feishu_event_receipts DROP CONSTRAINT feishu_event_receipts_status_check;
ALTER TABLE feishu_event_receipts ADD CONSTRAINT feishu_event_receipts_status_check
  CHECK (status IN ('received', 'processing', 'processed', 'failed', 'dead', 'ignored'));
-- app_id 记录事件所属应用，便于多应用接入时按应用隔离配置。
ALTER TABLE feishu_event_receipts ADD COLUMN app_id text NOT NULL DEFAULT '';
-- attempts 记录消费次数，超过预算的事件进入 dead 等待人工重放。
ALTER TABLE feishu_event_receipts ADD COLUMN attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0);
-- last_attempt_at 记录最近一次消费时间，便于排查重复投递。
ALTER TABLE feishu_event_receipts ADD COLUMN last_attempt_at timestamptz;
CREATE INDEX feishu_event_receipts_status_idx ON feishu_event_receipts (status, received_at);

-- 事件回执是系统级事实表，不启用行级隔离，但只授予应用角色最小读写权限。
GRANT SELECT, INSERT, UPDATE ON feishu_event_receipts TO harness_app;

-- 群聊话题可能由不同成员触发，Worker 需要以系统身份按 tenant/chat/thread 查找既有绑定。
-- 该策略只允许系统作用域读取，普通用户作用域仍由既有 user_isolation 策略限制。
CREATE POLICY external_binding_system ON external_conversation_bindings FOR ALL TO harness_app
  USING (nullif(current_setting('app.system', true), '') = 'on')
  WITH CHECK (nullif(current_setting('app.system', true), '') = 'on');

-- 同一会话内外部消息 ID 唯一，保证飞书重推不会重复写入用户消息。
CREATE UNIQUE INDEX messages_external_message_key ON messages (session_id, external_message_id)
  WHERE external_message_id <> '';

-- 外发回复表：保存卡片占位、增量更新与失败降级的至少一次投递状态。
CREATE TABLE feishu_outbound_messages (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  -- 归属用户；群聊话题使用会话所有者，实际发言人记录在消息中。
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  session_id uuid NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  -- 关联的 Run，可为空表示与 Run 无关的系统通知。
  run_id uuid REFERENCES agent_runs(id) ON DELETE CASCADE,
  chat_id text NOT NULL,
  thread_id text NOT NULL DEFAULT '',
  -- 触发回复的原始消息 ID，用于飞书 reply 接口保持话题上下文。
  reply_to_message_id text NOT NULL DEFAULT '',
  -- 飞书返回的回复消息 ID；卡片更新依赖该 ID。
  message_id text NOT NULL DEFAULT '',
  kind text NOT NULL DEFAULT 'card' CHECK (kind IN ('card', 'text')),
  status text NOT NULL DEFAULT 'pending'
    CHECK (status IN ('pending', 'sending', 'retrying', 'completed', 'dead')),
  attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  max_attempts integer NOT NULL DEFAULT 8 CHECK (max_attempts > 0),
  available_at timestamptz NOT NULL DEFAULT now(),
  lease_owner text NOT NULL DEFAULT '',
  lease_expires_at timestamptz,
  -- 业务去重键，保证同一 Run 只外发一次。
  dedupe_key text NOT NULL UNIQUE,
  last_error text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  completed_at timestamptz,
  CHECK (octet_length(last_error) <= 1024)
);

CREATE INDEX feishu_outbound_claim_idx ON feishu_outbound_messages (status, available_at, created_at);
CREATE INDEX feishu_outbound_chat_idx ON feishu_outbound_messages (chat_id, created_at);

ALTER TABLE feishu_outbound_messages ENABLE ROW LEVEL SECURITY;
ALTER TABLE feishu_outbound_messages FORCE ROW LEVEL SECURITY;
-- 外发消息由后台 Worker 统一处理，仅允许系统作用域读写，避免用户伪造外部回复。
CREATE POLICY feishu_outbound_system ON feishu_outbound_messages FOR ALL TO harness_app
  USING (nullif(current_setting('app.system', true), '') = 'on')
  WITH CHECK (nullif(current_setting('app.system', true), '') = 'on');

GRANT SELECT, INSERT, UPDATE, DELETE ON feishu_outbound_messages TO harness_app;

COMMENT ON TABLE feishu_outbound_messages IS '飞书外发回复表：卡片占位、增量更新与文本降级的至少一次投递';
COMMENT ON COLUMN feishu_outbound_messages.dedupe_key IS '业务去重键，避免同一 Run 重复回复';
COMMENT ON COLUMN feishu_outbound_messages.message_id IS '飞书回复消息 ID，卡片增量更新依赖';
COMMENT ON COLUMN feishu_outbound_messages.kind IS '回复形态：card 可更新卡片，text 降级文本';
COMMENT ON COLUMN feishu_event_receipts.attempts IS '事件消费次数，超过预算进入 dead';
