-- 管理员全局运行审计：允许应用连接在显式 system 作用域下只读会话与运行记录。
-- 写入仍受用户作用域策略限制，审计接口只做查询与展示。
CREATE POLICY sessions_app_system_read ON sessions FOR SELECT TO harness_app
  USING (nullif(current_setting('app.system', true), '') = 'on');
CREATE POLICY agent_runs_app_system_read ON agent_runs FOR SELECT TO harness_app
  USING (nullif(current_setting('app.system', true), '') = 'on');
