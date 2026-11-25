-- 000085：记录用户手动编辑自动技能的时间，后续自动提炼不再覆盖用户修改。

ALTER TABLE user_skills
  ADD COLUMN IF NOT EXISTS user_edited_at timestamptz;

COMMENT ON COLUMN user_skills.user_edited_at IS '用户最后一次手动编辑时间；非空时自动提炼不再覆盖该技能';
