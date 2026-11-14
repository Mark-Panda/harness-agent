-- 000076：用户记忆清理水位。
-- 记录最近一次“删除我的记忆”完成时间；向量回填与增量索引跳过该时间之前的消息，
-- 避免删除后后台维护任务把历史会话重新向量化，导致已删除记忆再次出现。

CREATE TABLE IF NOT EXISTS user_memory_state (
  user_id uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  memory_cleared_at timestamptz,
  updated_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE user_memory_state ENABLE ROW LEVEL SECURITY;
ALTER TABLE user_memory_state FORCE ROW LEVEL SECURITY;
CREATE POLICY user_isolation ON user_memory_state FOR ALL TO harness_app
  USING (user_id = nullif(current_setting('app.user_id', true), '')::uuid
    OR nullif(current_setting('app.system', true), '') = 'on')
  WITH CHECK (user_id = nullif(current_setting('app.user_id', true), '')::uuid
    OR nullif(current_setting('app.system', true), '') = 'on');

GRANT SELECT, INSERT, UPDATE, DELETE ON user_memory_state TO harness_app;

COMMENT ON TABLE user_memory_state IS '用户记忆清理水位，用于阻止清理后的历史消息被回填重新写入';
COMMENT ON COLUMN user_memory_state.memory_cleared_at IS '最近一次主动清理记忆的时间；该时间及之前的消息不再参与向量索引';
