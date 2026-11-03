-- 账号删除的 DELETE 语句使用 WHERE user_id，需要同时授予 SELECT。
GRANT SELECT, DELETE ON password_reset_tokens TO harness_app;
