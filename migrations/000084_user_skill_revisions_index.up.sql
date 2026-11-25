-- 用户技能历史快照按用户、技能与时间读取；CONCURRENTLY 需独立迁移文件。
CREATE INDEX CONCURRENTLY IF NOT EXISTS user_skill_revisions_user_skill_idx
  ON user_skill_revisions (user_id, skill_id, created_at DESC);
