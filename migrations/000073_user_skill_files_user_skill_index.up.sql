-- 用户技能包文件按用户与技能读取；CONCURRENTLY 需独立迁移文件。
CREATE INDEX CONCURRENTLY IF NOT EXISTS user_skill_files_user_skill_idx ON user_skill_files (user_id, skill_id);
