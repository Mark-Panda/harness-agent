-- 助手版本归档：生命周期新增 archived，旧版本可从默认列表隐藏并保留历史引用。
ALTER TABLE agents DROP CONSTRAINT agents_status_check;
ALTER TABLE agents ADD CONSTRAINT agents_status_check
  CHECK (status IN ('draft', 'published', 'disabled', 'archived'));
COMMENT ON COLUMN agents.status IS '生命周期：draft 草稿、published 已发布、disabled 已停用、archived 已归档';
