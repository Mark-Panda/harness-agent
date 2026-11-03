-- 账号删除需要清理用户级用量聚合；仅系统作用域允许删除，普通用户请求仍只能读写。
-- 拆分原 FOR ALL 策略，避免授予常规请求删除计费聚合的能力。
DROP POLICY usage_counter_access ON usage_counters;
CREATE POLICY usage_counter_read ON usage_counters FOR SELECT TO harness_app USING (true);
CREATE POLICY usage_counter_insert ON usage_counters FOR INSERT TO harness_app WITH CHECK (true);
CREATE POLICY usage_counter_update ON usage_counters FOR UPDATE TO harness_app USING (true) WITH CHECK (true);
CREATE POLICY usage_counter_delete ON usage_counters FOR DELETE TO harness_app USING (current_setting('app.system', true) = 'on');
GRANT DELETE ON usage_counters TO harness_app;
