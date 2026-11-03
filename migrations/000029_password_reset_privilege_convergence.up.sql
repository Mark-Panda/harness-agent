-- 000029：收敛 password_reset_tokens 的应用角色权限。
-- 背景：历史迁移曾向 harness_app 授予 ALL，后续版本改为仅需 SELECT/DELETE；
-- 已部署库与全新库因此产生权限漂移。本条前滚迁移先回收全部权限，再按最小权限重新授予。
REVOKE ALL PRIVILEGES ON password_reset_tokens FROM harness_app;
GRANT SELECT, DELETE ON password_reset_tokens TO harness_app;
