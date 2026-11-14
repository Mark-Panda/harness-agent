-- Agent 绑定按用户与 Agent 读取；CONCURRENTLY 需独立迁移文件。
CREATE INDEX CONCURRENTLY IF NOT EXISTS agent_user_skills_user_agent_idx ON agent_user_skills (user_id, agent_id);
