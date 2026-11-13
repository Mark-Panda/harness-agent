-- 保存外发回复的 POST 请求意图，跨 Worker 重启恢复时可复用。
-- group_text 暂存群聊共享回复正文，避免多次查询历史消息。

ALTER TABLE feishu_outbound_messages
  ADD COLUMN IF NOT EXISTS post_request_json jsonb,
  ADD COLUMN IF NOT EXISTS group_text text;
