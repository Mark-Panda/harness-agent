-- Preserve the MCP target independently of the model-facing tool alias.
ALTER TABLE tool_approvals
    ADD COLUMN IF NOT EXISTS mcp_server_id uuid,
    ADD COLUMN IF NOT EXISTS mcp_tool_name text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS mcp_endpoint_fingerprint text NOT NULL DEFAULT '';
