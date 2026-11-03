-- 000009：为 messages 增加工具调用关联字段。
-- 说明：ReAct 恢复与协议回放需要 tool_call_id 与工具名，不能只保存在 tool_calls JSON 中。

ALTER TABLE messages
  ADD COLUMN tool_call_id text NOT NULL DEFAULT '',
  ADD COLUMN tool_name text NOT NULL DEFAULT '';

COMMENT ON COLUMN messages.tool_call_id IS '工具结果对应的模型 tool_call_id，用于上下文协议回放与恢复';
COMMENT ON COLUMN messages.tool_name IS '工具名称，用于执行树与循环检测展示';
