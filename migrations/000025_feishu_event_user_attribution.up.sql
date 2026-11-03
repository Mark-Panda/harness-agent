-- 飞书原始事件回执可能包含发言正文，账号删除必须按发言人清理。
-- 处理阶段解析身份后回填 user_id；未绑定用户的历史事件仍按运维保留策略处理。
ALTER TABLE feishu_event_receipts ADD COLUMN IF NOT EXISTS user_id uuid;
CREATE INDEX IF NOT EXISTS feishu_event_receipts_user_idx ON feishu_event_receipts (user_id);
ALTER TABLE feishu_event_receipts
  ADD CONSTRAINT feishu_event_receipts_user_id_fkey FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE;
GRANT DELETE ON feishu_event_receipts TO harness_app;
