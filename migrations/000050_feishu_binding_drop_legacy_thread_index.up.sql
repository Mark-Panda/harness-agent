-- 000050：新话题唯一索引已上线，移除不含 app_id 的旧索引。

DROP INDEX CONCURRENTLY IF EXISTS external_binding_thread_key;
