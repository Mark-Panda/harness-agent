-- 平台技能包文件按技能读取；CONCURRENTLY 需独立迁移文件。
CREATE INDEX CONCURRENTLY IF NOT EXISTS skill_files_skill_idx ON skill_files (skill_id);
