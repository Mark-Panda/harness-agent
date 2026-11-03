-- 管理员用户目录：允许应用连接在显式 system 作用域下只读全量用户。
-- 写入仍受 users_app_self 限制，管理员只能通过既有接口修改单个用户的凭据状态。
CREATE POLICY users_app_system_read ON users FOR SELECT TO harness_app
  USING (nullif(current_setting('app.system', true), '') = 'on');
