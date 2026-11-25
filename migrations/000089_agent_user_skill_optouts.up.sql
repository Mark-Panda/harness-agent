-- 000089：助手版本显式移除的用户技能标记，用于尊重“按版本移除”语义。

CREATE TABLE IF NOT EXISTS agent_user_skill_optouts (
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  agent_id uuid NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
  user_skill_id uuid NOT NULL REFERENCES user_skills(id) ON DELETE CASCADE,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, agent_id, user_skill_id)
);

ALTER TABLE agent_user_skill_optouts ENABLE ROW LEVEL SECURITY;
ALTER TABLE agent_user_skill_optouts FORCE ROW LEVEL SECURITY;
CREATE POLICY user_isolation ON agent_user_skill_optouts FOR ALL TO harness_app
  USING (user_id = nullif(current_setting('app.user_id', true), '')::uuid)
  WITH CHECK (user_id = nullif(current_setting('app.user_id', true), '')::uuid);

GRANT SELECT, INSERT, DELETE ON agent_user_skill_optouts TO harness_app;

COMMENT ON TABLE agent_user_skill_optouts IS '助手版本显式移除的用户技能，自动挂载与发布继承时跳过，按 user_id 隔离';
COMMENT ON COLUMN agent_user_skill_optouts.user_skill_id IS '被该助手版本显式移除的用户技能 ID';
