-- 000087：助手发布快照原地更新前的历史，用于技能自动绑定后的配置回滚。

CREATE TABLE IF NOT EXISTS agent_snapshot_revisions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  agent_id uuid NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
  agent_version integer NOT NULL,
  config_snapshot jsonb NOT NULL,
  reason text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE agent_snapshot_revisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE agent_snapshot_revisions FORCE ROW LEVEL SECURITY;
CREATE POLICY user_isolation ON agent_snapshot_revisions FOR ALL TO harness_app
  USING (user_id = nullif(current_setting('app.user_id', true), '')::uuid)
  WITH CHECK (user_id = nullif(current_setting('app.user_id', true), '')::uuid);

GRANT SELECT, INSERT, DELETE ON agent_snapshot_revisions TO harness_app;

COMMENT ON TABLE agent_snapshot_revisions IS '助手发布快照原地更新前的历史，按 user_id 隔离';
COMMENT ON COLUMN agent_snapshot_revisions.reason IS '写入历史的原因，如技能绑定或快照恢复';
