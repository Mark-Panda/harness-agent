-- 账号删除由应用角色执行，需要清理该用户的一次性重置令牌。
GRANT DELETE ON password_reset_tokens TO harness_app;
