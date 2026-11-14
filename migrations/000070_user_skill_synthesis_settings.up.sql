-- 000070：用户级每日技能提炼设置；平台模型策略仅保存引用，不复制供应商凭证。
CREATE TABLE IF NOT EXISTS user_skill_synthesis_settings (
  user_id uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  enabled boolean NOT NULL DEFAULT false,
  model_policy_id uuid,
  model_policy_version integer,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  CHECK (model_policy_version IS NULL OR model_policy_version > 0),
  CHECK (
    (model_policy_id IS NULL AND model_policy_version IS NULL)
    OR (model_policy_id IS NOT NULL AND model_policy_version IS NOT NULL)
  ),
  FOREIGN KEY (model_policy_id, model_policy_version)
    REFERENCES model_policies(id, version)
);

ALTER TABLE user_skill_synthesis_settings ENABLE ROW LEVEL SECURITY;
ALTER TABLE user_skill_synthesis_settings FORCE ROW LEVEL SECURITY;
CREATE POLICY user_or_system ON user_skill_synthesis_settings FOR ALL TO harness_app
  USING (
    user_id = nullif(current_setting('app.user_id', true), '')::uuid
    OR nullif(current_setting('app.system', true), '') = 'on'
  )
  WITH CHECK (
    user_id = nullif(current_setting('app.user_id', true), '')::uuid
    OR nullif(current_setting('app.system', true), '') = 'on'
  );

GRANT SELECT, INSERT, UPDATE, DELETE ON user_skill_synthesis_settings TO harness_app;

COMMENT ON TABLE user_skill_synthesis_settings IS '用户级技能提炼开关与模型策略引用；未配置策略时使用平台默认策略';
COMMENT ON COLUMN user_skill_synthesis_settings.enabled IS '是否允许后台分析该用户对话并提炼技能，默认关闭';
COMMENT ON COLUMN user_skill_synthesis_settings.model_policy_version IS '与 model_policy_id 成对出现，必须引用已发布策略版本';
