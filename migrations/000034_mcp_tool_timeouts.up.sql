-- MCP 工具超时覆盖：由管理界面按原始工具名或前缀配置，未配置时使用执行层默认超时。
ALTER TABLE mcps ADD COLUMN IF NOT EXISTS tool_timeouts jsonb NOT NULL DEFAULT '{}'::jsonb;
COMMENT ON COLUMN mcps.tool_timeouts IS '工具超时覆盖：原始工具名或前缀映射到秒数，空对象表示全部使用默认超时';
