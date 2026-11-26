-- 内置工具按 Agent 版本绑定：NULL 继承平台配置，空数组表示显式禁用全部。
ALTER TABLE agents ADD COLUMN builtin_tools jsonb;
COMMENT ON COLUMN agents.builtin_tools IS '内置工具名称列表；NULL 继承平台配置，空数组表示禁用全部';
