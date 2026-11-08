-- 保留期清理与飞书回执的访问控制加固。

-- 1) 幂等键：保留期任务以系统身份删除过期记录，策略需要允许 app.system 旁路。
DROP POLICY IF EXISTS user_isolation ON idempotency_keys;
CREATE POLICY user_isolation ON idempotency_keys FOR ALL TO harness_app
  USING (user_id = nullif(current_setting('app.user_id', true), '')::uuid OR nullif(current_setting('app.system', true), '') = 'on')
  WITH CHECK (user_id = nullif(current_setting('app.user_id', true), '')::uuid OR nullif(current_setting('app.system', true), '') = 'on');

-- 2) 飞书事件回执包含原始消息正文：启用 RLS，仅允许系统流程或事件归属用户访问。
ALTER TABLE feishu_event_receipts ENABLE ROW LEVEL SECURITY;
ALTER TABLE feishu_event_receipts FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS user_isolation ON feishu_event_receipts;
CREATE POLICY user_isolation ON feishu_event_receipts FOR ALL TO harness_app
  USING (user_id = nullif(current_setting('app.user_id', true), '')::uuid OR nullif(current_setting('app.system', true), '') = 'on')
  WITH CHECK (user_id = nullif(current_setting('app.user_id', true), '')::uuid OR nullif(current_setting('app.system', true), '') = 'on');
