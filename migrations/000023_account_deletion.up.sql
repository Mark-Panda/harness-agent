-- 账号删除：通过状态 deleted + 匿名化保留审计外键，不物理删除 users 行。
ALTER TABLE users DROP CONSTRAINT users_status_check;
ALTER TABLE users ADD CONSTRAINT users_status_check CHECK (status IN ('active', 'disabled', 'deleted'));
