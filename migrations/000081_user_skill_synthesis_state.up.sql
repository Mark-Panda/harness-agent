-- 000081：技能提炼覆盖水位；只处理水位之后的新消息，避免重复提炼同一批对话。
CREATE TABLE IF NOT EXISTS user_skill_synthesis_state (
  user_id uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  covered_at timestamptz NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE user_skill_synthesis_state ENABLE ROW LEVEL SECURITY;
ALTER TABLE user_skill_synthesis_state FORCE ROW LEVEL SECURITY;
CREATE POLICY user_or_system ON user_skill_synthesis_state FOR ALL TO harness_app
  USING (
    user_id = nullif(current_setting('app.user_id', true), '')::uuid
    OR nullif(current_setting('app.system', true), '') = 'on'
  )
  WITH CHECK (
    user_id = nullif(current_setting('app.user_id', true), '')::uuid
    OR nullif(current_setting('app.system', true), '') = 'on'
  );

GRANT SELECT, INSERT, UPDATE, DELETE ON user_skill_synthesis_state TO harness_app;

COMMENT ON TABLE user_skill_synthesis_state IS '用户技能提炼覆盖水位；水位之后的消息才会进入下一次提炼';
COMMENT ON COLUMN user_skill_synthesis_state.covered_at IS '最近一次成功分析到的消息时间（UTC）';

-- 用已有成功记录的完成时间初始化水位，避免升级后立刻重复分析历史对话。
INSERT INTO user_skill_synthesis_state (user_id, covered_at, updated_at)
SELECT user_id, MAX(COALESCE(finished_at, updated_at)), now()
FROM user_skill_synthesis_runs
WHERE status IN ('completed', 'skipped')
  AND outcome NOT IN ('disabled', 'no_activity', 'model_unavailable', 'settings_unavailable')
GROUP BY user_id
ON CONFLICT (user_id) DO NOTHING;
