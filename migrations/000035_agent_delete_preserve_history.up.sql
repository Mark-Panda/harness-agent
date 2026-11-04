-- 删除助手时保留会话、运行与外部会话绑定历史，只解除助手归属，避免外键阻塞删除。
-- 历史记录自身的消息、快照与执行事实不受影响。
ALTER TABLE sessions DROP CONSTRAINT IF EXISTS sessions_agent_id_fkey;
ALTER TABLE sessions ADD CONSTRAINT sessions_agent_id_fkey FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE SET NULL;

ALTER TABLE agent_runs DROP CONSTRAINT IF EXISTS agent_runs_agent_id_fkey;
ALTER TABLE agent_runs ADD CONSTRAINT agent_runs_agent_id_fkey FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE SET NULL;

ALTER TABLE external_conversation_bindings DROP CONSTRAINT IF EXISTS external_conversation_bindings_agent_id_fkey;
ALTER TABLE external_conversation_bindings ADD CONSTRAINT external_conversation_bindings_agent_id_fkey FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE SET NULL;
