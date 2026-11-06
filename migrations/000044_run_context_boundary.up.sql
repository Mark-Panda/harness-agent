-- 上下文右边界：记录触发 Run 的用户消息，同会话排队串行时，
-- 前序 Run 不得把创建之后写入的用户消息当成自己的输入，避免重复回答与语义错位。
ALTER TABLE agent_runs ADD COLUMN trigger_message_id uuid;
ALTER TABLE agent_runs ADD COLUMN trigger_message_at timestamptz;
COMMENT ON COLUMN agent_runs.trigger_message_id IS '触发本次 Run 的用户消息 ID，用于限定上下文历史右边界';
COMMENT ON COLUMN agent_runs.trigger_message_at IS '触发消息的写入时间，触发消息不在热窗口时仍可据此裁剪上下文';

-- 标记 Worker 写入的 assistant/tool 消息所属 Run：审批恢复时只回放本 Run 的产物，
-- 同时可排除其他 Run 在触发消息之后写入的回复。
ALTER TABLE messages ADD COLUMN run_id uuid;
COMMENT ON COLUMN messages.run_id IS '产生该消息的 Run ID；用户消息为空';
