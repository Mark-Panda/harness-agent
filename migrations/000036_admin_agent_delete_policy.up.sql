-- 管理员全局助手视图允许在 system scope 下删除任意用户的助手版本。
-- 应用层 AdminDelete 仍强制管理员校验，普通用户请求不会进入 system scope。
DROP POLICY IF EXISTS agents_app_system_delete ON agents;
CREATE POLICY agents_app_system_delete ON agents FOR DELETE
  USING (NULLIF(current_setting('app.system', true), '') = 'on');
