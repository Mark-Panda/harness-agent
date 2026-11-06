-- 000046：飞书群聊话题绑定增加应用维度。
-- 同一群聊可能同时安装多个机器人；仅以 tenant/chat/thread 作为唯一键会导致不同机器人
-- 复用同一 Session，甚至把消息路由到其他机器人的绑定。

ALTER TABLE external_conversation_bindings
  ADD COLUMN IF NOT EXISTS app_id text NOT NULL DEFAULT '';

COMMENT ON COLUMN external_conversation_bindings.app_id IS '飞书事件所属应用 App ID，用于隔离同一群聊中的多个机器人';
