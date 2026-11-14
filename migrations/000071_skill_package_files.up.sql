-- 000071：标准 SKILL 技能包（多文件夹 + 脚本）存储。
-- 技能表增加包元数据列，文件内容保存到独立表；平台技能全局共享，用户技能按 user_id 隔离。

ALTER TABLE skills
  ADD COLUMN IF NOT EXISTS bundle_format text NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS bundle_entrypoint text NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS bundle_file_count integer NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS bundle_total_bytes bigint NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS bundle_has_scripts boolean NOT NULL DEFAULT false,
  ADD COLUMN IF NOT EXISTS bundle_execution_enabled boolean NOT NULL DEFAULT false,
  ADD COLUMN IF NOT EXISTS bundle_digest text NOT NULL DEFAULT '';

ALTER TABLE user_skills
  ADD COLUMN IF NOT EXISTS bundle_format text NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS bundle_entrypoint text NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS bundle_file_count integer NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS bundle_total_bytes bigint NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS bundle_has_scripts boolean NOT NULL DEFAULT false,
  ADD COLUMN IF NOT EXISTS bundle_execution_enabled boolean NOT NULL DEFAULT false,
  ADD COLUMN IF NOT EXISTS bundle_digest text NOT NULL DEFAULT '';

-- 平台技能文件：仅管理员导入，运行时只读。
CREATE TABLE IF NOT EXISTS skill_files (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  skill_id uuid NOT NULL REFERENCES skills(id) ON DELETE CASCADE,
  path text NOT NULL,
  content bytea NOT NULL,
  mime text NOT NULL DEFAULT 'application/octet-stream',
  size_bytes bigint NOT NULL DEFAULT 0 CHECK (size_bytes >= 0),
  sha256 text NOT NULL DEFAULT '',
  is_script boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (skill_id, path)
);

GRANT SELECT, INSERT, UPDATE, DELETE ON skill_files TO harness_app;

-- 用户技能文件：按 user_id 严格隔离，禁止跨用户读取脚本与参考资料。
CREATE TABLE IF NOT EXISTS user_skill_files (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  skill_id uuid NOT NULL REFERENCES user_skills(id) ON DELETE CASCADE,
  path text NOT NULL,
  content bytea NOT NULL,
  mime text NOT NULL DEFAULT 'application/octet-stream',
  size_bytes bigint NOT NULL DEFAULT 0 CHECK (size_bytes >= 0),
  sha256 text NOT NULL DEFAULT '',
  is_script boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (skill_id, path)
);

ALTER TABLE user_skill_files ENABLE ROW LEVEL SECURITY;
ALTER TABLE user_skill_files FORCE ROW LEVEL SECURITY;
CREATE POLICY user_isolation ON user_skill_files FOR ALL TO harness_app
  USING (user_id = nullif(current_setting('app.user_id', true), '')::uuid)
  WITH CHECK (user_id = nullif(current_setting('app.user_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON user_skill_files TO harness_app;

COMMENT ON TABLE skill_files IS '平台级标准 SKILL 技能包文件，管理员导入、运行时只读';
COMMENT ON TABLE user_skill_files IS '用户级标准 SKILL 技能包文件，按 user_id 隔离';
COMMENT ON COLUMN skills.bundle_execution_enabled IS '是否允许该技能包内的脚本被执行；默认关闭';
COMMENT ON COLUMN user_skills.bundle_execution_enabled IS '是否允许该技能包内的脚本被执行；默认关闭';
