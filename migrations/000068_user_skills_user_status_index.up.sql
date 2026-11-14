-- 用户技能按 (user_id, status) 过滤查询；CONCURRENTLY 需独立迁移文件。
CREATE INDEX CONCURRENTLY IF NOT EXISTS user_skills_user_status_idx ON user_skills (user_id, status);
