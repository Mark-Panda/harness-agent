-- 000074：Agent 与用户级技能绑定表。
-- 用户技能按 user_id 隔离，Agent 发布快照会把已发布技能冻结进 config_snapshot。

CREATE TABLE IF NOT EXISTS agent_user_skills (
  agent_id uuid NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
  user_skill_id uuid NOT NULL REFERENCES user_skills(id) ON DELETE CASCADE,
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (agent_id, user_skill_id)
);

ALTER TABLE agent_user_skills ENABLE ROW LEVEL SECURITY;
ALTER TABLE agent_user_skills FORCE ROW LEVEL SECURITY;
CREATE POLICY user_isolation ON agent_user_skills FOR ALL TO harness_app
  USING (user_id = nullif(current_setting('app.user_id', true), '')::uuid)
  WITH CHECK (user_id = nullif(current_setting('app.user_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON agent_user_skills TO harness_app;

COMMENT ON TABLE agent_user_skills IS 'Agent 与用户级 Skill 的绑定表，按 user_id 隔离';
