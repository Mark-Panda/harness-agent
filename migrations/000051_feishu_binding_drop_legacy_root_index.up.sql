-- 000051：新根消息别名唯一索引已上线，移除不含 app_id 的旧索引。

DROP INDEX CONCURRENTLY IF EXISTS external_binding_root_key;
