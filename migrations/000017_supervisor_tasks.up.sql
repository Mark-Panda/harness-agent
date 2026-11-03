-- 000017：新增 Supervisor 子任务与子会话关联，承载委派、权限交集、并发与部分失败事实。
-- 说明：子任务使用独立 Session 且保留原始 user_id；parent_run_id 关联主 Run，删除主 Run 时级联清理任务。

ALTER TABLE sessions ADD COLUMN parent_session_id uuid REFERENCES sessions(id) ON DELETE SET NULL;
ALTER TABLE sessions ADD COLUMN parent_run_id uuid REFERENCES agent_runs(id) ON DELETE SET NULL;
CREATE INDEX sessions_parent_run_idx ON sessions (parent_run_id) WHERE parent_run_id IS NOT NULL;

CREATE TABLE supervisor_tasks (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  parent_run_id uuid NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
  child_session_id uuid NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  worker_name text NOT NULL,
  worker_index int NOT NULL,
  task text NOT NULL,
  tool_scope jsonb NOT NULL DEFAULT '[]'::jsonb,
  status text NOT NULL DEFAULT 'pending'
    CHECK (status IN ('pending', 'running', 'completed', 'failed', 'cancelled', 'unknown')),
  attempts int NOT NULL DEFAULT 0,
  max_attempts int NOT NULL DEFAULT 1,
  result_json jsonb,
  error_code text NOT NULL DEFAULT '',
  error_message text NOT NULL DEFAULT '',
  depth int NOT NULL DEFAULT 1,
  started_at timestamptz,
  finished_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (parent_run_id, worker_index),
  CHECK (worker_index >= 1 AND worker_index <= 4),
  CHECK (depth BETWEEN 1 AND 2),
  CHECK (attempts BETWEEN 0 AND max_attempts),
  CHECK (octet_length(worker_name) BETWEEN 1 AND 256),
  CHECK (octet_length(task) BETWEEN 1 AND 8192),
  CHECK (jsonb_typeof(tool_scope) = 'array'),
  CHECK (octet_length(error_message) <= 4096)
);

CREATE INDEX supervisor_tasks_run_status_idx ON supervisor_tasks (parent_run_id, status);
CREATE INDEX supervisor_tasks_user_status_idx ON supervisor_tasks (user_id, status);

ALTER TABLE supervisor_tasks ENABLE ROW LEVEL SECURITY;
ALTER TABLE supervisor_tasks FORCE ROW LEVEL SECURITY;
CREATE POLICY user_isolation ON supervisor_tasks FOR ALL TO harness_app
  USING (user_id = nullif(current_setting('app.user_id', true), '')::uuid)
  WITH CHECK (user_id = nullif(current_setting('app.user_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON supervisor_tasks TO harness_app;

COMMENT ON COLUMN sessions.parent_session_id IS 'Supervisor 子会话关联的父会话；普通会话为空';
COMMENT ON COLUMN sessions.parent_run_id IS 'Supervisor 子会话关联的主 Run；普通会话为空';
COMMENT ON TABLE supervisor_tasks IS 'Supervisor 委派子任务：保存 Worker 能力交集、独立子会话、并发与部分失败事实';
COMMENT ON COLUMN supervisor_tasks.id IS '子任务主键';
COMMENT ON COLUMN supervisor_tasks.user_id IS '原始用户，RLS 隔离键；委派不得改变所属用户';
COMMENT ON COLUMN supervisor_tasks.parent_run_id IS '主 Run，子任务不得脱离主 Run 独立存在';
COMMENT ON COLUMN supervisor_tasks.child_session_id IS '子任务独立会话，上下文与主会话隔离';
COMMENT ON COLUMN supervisor_tasks.worker_name IS 'Worker 名称，同一主 Run 内唯一';
COMMENT ON COLUMN supervisor_tasks.worker_index IS 'Worker 序号，用于稳定排序和执行树展示';
COMMENT ON COLUMN supervisor_tasks.task IS '委派任务描述，受限正文';
COMMENT ON COLUMN supervisor_tasks.tool_scope IS '允许 Worker 使用的工具名交集，必须在主 Agent 可用工具范围内';
COMMENT ON COLUMN supervisor_tasks.status IS '任务状态：pending/running/completed/failed/cancelled/unknown';
COMMENT ON COLUMN supervisor_tasks.attempts IS '已领取次数，重复领取由状态 CAS 阻止';
COMMENT ON COLUMN supervisor_tasks.max_attempts IS '最大领取次数，副作用型 Worker 默认不重试';
COMMENT ON COLUMN supervisor_tasks.result_json IS 'Worker 结果快照，限制大小；失败任务不得伪装成功';
COMMENT ON COLUMN supervisor_tasks.error_code IS '失败业务码';
COMMENT ON COLUMN supervisor_tasks.error_message IS '脱敏失败原因';
COMMENT ON COLUMN supervisor_tasks.depth IS '委派深度，默认 1；超过深度上限必须拒绝';
COMMENT ON COLUMN supervisor_tasks.started_at IS '开始执行时间';
COMMENT ON COLUMN supervisor_tasks.finished_at IS '终态时间';
