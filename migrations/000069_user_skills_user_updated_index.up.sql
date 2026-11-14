-- 用户技能列表按 (user_id, updated_at) 倒序；CONCURRENTLY 需独立迁移文件。
CREATE INDEX CONCURRENTLY IF NOT EXISTS user_skills_user_updated_idx ON user_skills (user_id, updated_at DESC);
