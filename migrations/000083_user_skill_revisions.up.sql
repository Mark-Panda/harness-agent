-- 000083：用户技能原地更新前的历史快照，用于误覆盖恢复与变更审计。

CREATE TABLE IF NOT EXISTS user_skill_revisions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  skill_id uuid NOT NULL REFERENCES user_skills(id) ON DELETE CASCADE,
  version integer NOT NULL CHECK (version > 0),
  description text NOT NULL DEFAULT '',
  prompt_template text NOT NULL,
  parameters_json jsonb NOT NULL DEFAULT '{}'::jsonb,
  source_session_ids jsonb NOT NULL DEFAULT '[]'::jsonb,
  confidence numeric(4, 3) NOT NULL DEFAULT 0 CHECK (confidence >= 0 AND confidence <= 1),
  created_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE user_skill_revisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE user_skill_revisions FORCE ROW LEVEL SECURITY;
CREATE POLICY user_isolation ON user_skill_revisions FOR ALL TO harness_app
  USING (user_id = nullif(current_setting('app.user_id', true), '')::uuid)
  WITH CHECK (user_id = nullif(current_setting('app.user_id', true), '')::uuid);

GRANT SELECT, INSERT, DELETE ON user_skill_revisions TO harness_app;

COMMENT ON TABLE user_skill_revisions IS '用户技能原地更新前的历史内容快照，按 user_id 隔离';
COMMENT ON COLUMN user_skill_revisions.version IS '更新前技能版本号';
COMMENT ON COLUMN user_skill_revisions.prompt_template IS '更新前提示词正文';
COMMENT ON COLUMN user_skill_revisions.source_session_ids IS '更新前提炼来源会话 ID 列表';
