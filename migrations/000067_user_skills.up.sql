-- 000067：用户级技能表，用于从每日有效对话中自动提炼可复用技能。
-- 与平台级 skills 表分离，按 user_id 严格隔离，确保对话隐私不跨用户泄露。

CREATE TABLE IF NOT EXISTS user_skills (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name text NOT NULL,
  version integer NOT NULL DEFAULT 1 CHECK (version > 0),
  description text NOT NULL DEFAULT '',
  prompt_template text NOT NULL,
  parameters_json jsonb NOT NULL DEFAULT '{}'::jsonb,
  -- 生命周期：auto_generated（LLM 自动产出草稿）→ published（可被 Agent 引用）→ disabled。
  status text NOT NULL DEFAULT 'auto_generated' CHECK (status IN ('auto_generated', 'published', 'disabled')),
  -- 来源：auto 由每日对话提炼生成，manual 由用户手动创建。
  origin text NOT NULL DEFAULT 'auto' CHECK (origin IN ('auto', 'manual')),
  -- 提炼来源会话 ID 列表，用于审计与回溯。
  source_session_ids jsonb NOT NULL DEFAULT '[]'::jsonb,
  -- LLM 判定的置信度 [0,1]，仅 auto 来源有意义。
  confidence numeric(4, 3) NOT NULL DEFAULT 0 CHECK (confidence >= 0 AND confidence <= 1),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, name, version)
);

-- 索引见独立迁移 000068、000069（CONCURRENTLY 需脱离事务执行）。

-- 用户级隔离：仅当前 app.user_id 可读写自己的技能。
ALTER TABLE user_skills ENABLE ROW LEVEL SECURITY;
ALTER TABLE user_skills FORCE ROW LEVEL SECURITY;
CREATE POLICY user_isolation ON user_skills FOR ALL TO harness_app
  USING (user_id = nullif(current_setting('app.user_id', true), '')::uuid)
  WITH CHECK (user_id = nullif(current_setting('app.user_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON user_skills TO harness_app;

COMMENT ON TABLE user_skills IS '用户级技能模板，从每日有效对话自动提炼或用户手动创建，按 user_id 隔离';
COMMENT ON COLUMN user_skills.status IS '生命周期：auto_generated/published/disabled';
COMMENT ON COLUMN user_skills.origin IS '来源：auto 自动提炼 / manual 手动创建';
COMMENT ON COLUMN user_skills.source_session_ids IS '提炼来源会话 ID 列表';
COMMENT ON COLUMN user_skills.confidence IS 'LLM 判定置信度 [0,1]';
