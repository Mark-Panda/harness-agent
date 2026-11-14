-- 并发索引必须独立迁移，不能与事务内 DDL 混用。
CREATE INDEX CONCURRENTLY IF NOT EXISTS feishu_bots_owner_idx ON feishu_bots(owner_user_id);
