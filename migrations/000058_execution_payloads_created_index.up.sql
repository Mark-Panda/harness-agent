-- 保留期清理按 created_at 分批删除；缺少单列索引时每批都会全表扫描并排序。
-- CONCURRENTLY 需独立迁移文件，避免在隐式事务中建索引阻塞写入。
CREATE INDEX CONCURRENTLY IF NOT EXISTS execution_payloads_created_idx ON execution_payloads (created_at);
