-- 000016：新增 plans 与 plan_steps，承载 Plan-and-Execute 的持久化计划、步骤状态机与恢复事实。
-- 说明：Plan 与 Run 一一对应但按版本演进；所有状态推进使用 revision 乐观锁，
-- 步骤执行使用 lease_owner + fence_token 阻止过期 Worker 回写。

CREATE TABLE plans (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  run_id uuid NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
  session_id uuid NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  version int NOT NULL DEFAULT 1,
  parent_plan_id uuid REFERENCES plans(id) ON DELETE SET NULL,
  status text NOT NULL DEFAULT 'planning'
    CHECK (status IN ('planning', 'executing', 'waiting_approval', 'paused', 'completed', 'failed', 'cancelled')),
  goal text NOT NULL DEFAULT '',
  config_snapshot jsonb NOT NULL DEFAULT '{}'::jsonb,
  max_steps int NOT NULL DEFAULT 50,
  total_input_tokens bigint NOT NULL DEFAULT 0,
  total_output_tokens bigint NOT NULL DEFAULT 0,
  estimated_cost double precision NOT NULL DEFAULT 0,
  deadline_at timestamptz,
  error_code text NOT NULL DEFAULT '',
  error_message text NOT NULL DEFAULT '',
  revision bigint NOT NULL DEFAULT 0,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  finished_at timestamptz,
  UNIQUE (run_id, version),
  CHECK (version >= 1),
  CHECK (max_steps BETWEEN 1 AND 50),
  CHECK (octet_length(goal) <= 8192),
  CHECK (octet_length(error_message) <= 4096),
  CHECK (total_input_tokens >= 0 AND total_output_tokens >= 0 AND estimated_cost >= 0),
  CHECK (jsonb_typeof(config_snapshot) = 'object')
);

CREATE INDEX plans_run_version_idx ON plans (run_id, version DESC);
CREATE INDEX plans_user_status_idx ON plans (user_id, status);
CREATE INDEX plans_status_created_idx ON plans (status, created_at);

CREATE TABLE plan_steps (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  plan_id uuid NOT NULL REFERENCES plans(id) ON DELETE CASCADE,
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  step_no int NOT NULL,
  title text NOT NULL,
  description text NOT NULL DEFAULT '',
  depends_on jsonb NOT NULL DEFAULT '[]'::jsonb,
  tool_name text NOT NULL DEFAULT '',
  arguments jsonb NOT NULL DEFAULT '{}'::jsonb,
  idempotency_key text NOT NULL DEFAULT '',
  status text NOT NULL DEFAULT 'pending'
    CHECK (status IN ('pending', 'running', 'waiting_approval', 'completed', 'failed', 'skipped', 'cancelled', 'unknown')),
  attempts int NOT NULL DEFAULT 0,
  max_attempts int NOT NULL DEFAULT 2,
  fence_token bigint NOT NULL DEFAULT 0,
  lease_owner text NOT NULL DEFAULT '',
  execution_step_id uuid REFERENCES execution_steps(id) ON DELETE SET NULL,
  result_json jsonb,
  error_code text NOT NULL DEFAULT '',
  error_message text NOT NULL DEFAULT '',
  started_at timestamptz,
  finished_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (plan_id, step_no),
  CHECK (step_no >= 1),
  CHECK (max_attempts BETWEEN 1 AND 5),
  CHECK (attempts >= 0 AND attempts <= max_attempts),
  CHECK (octet_length(title) BETWEEN 1 AND 256),
  CHECK (octet_length(description) <= 8192),
  CHECK (octet_length(tool_name) <= 256),
  CHECK (octet_length(idempotency_key) <= 256),
  CHECK (jsonb_typeof(depends_on) = 'array'),
  CHECK (jsonb_typeof(arguments) IN ('object', 'array')),
  CHECK (octet_length(arguments::text) <= 262144),
  CHECK (octet_length(error_message) <= 4096)
);

CREATE INDEX plan_steps_plan_status_idx ON plan_steps (plan_id, status);
CREATE INDEX plan_steps_plan_no_idx ON plan_steps (plan_id, step_no);

ALTER TABLE plans ENABLE ROW LEVEL SECURITY;
ALTER TABLE plans FORCE ROW LEVEL SECURITY;
CREATE POLICY user_isolation ON plans FOR ALL TO harness_app
  USING (user_id = nullif(current_setting('app.user_id', true), '')::uuid)
  WITH CHECK (user_id = nullif(current_setting('app.user_id', true), '')::uuid);

ALTER TABLE plan_steps ENABLE ROW LEVEL SECURITY;
ALTER TABLE plan_steps FORCE ROW LEVEL SECURITY;
CREATE POLICY user_isolation ON plan_steps FOR ALL TO harness_app
  USING (user_id = nullif(current_setting('app.user_id', true), '')::uuid)
  WITH CHECK (user_id = nullif(current_setting('app.user_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON plans TO harness_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON plan_steps TO harness_app;

COMMENT ON TABLE plans IS 'Plan-and-Execute 计划：按 Run 版本化保存目标、状态、预算与快照，revision 用于乐观锁';
COMMENT ON COLUMN plans.id IS '计划主键';
COMMENT ON COLUMN plans.user_id IS '所属用户，RLS 隔离键';
COMMENT ON COLUMN plans.run_id IS '所属 Run，Run 删除时级联清理计划';
COMMENT ON COLUMN plans.session_id IS '所属会话，用于审计关联';
COMMENT ON COLUMN plans.version IS '计划版本号，同一 Run 内从 1 递增；人工修改生成新版本';
COMMENT ON COLUMN plans.parent_plan_id IS '修订来源计划，继续执行沿用原版本，修改生成新版本';
COMMENT ON COLUMN plans.status IS '计划状态：planning/executing/waiting_approval/paused/completed/failed/cancelled，终态不可回写';
COMMENT ON COLUMN plans.goal IS '计划目标，来自 Planner 输出的受限正文';
COMMENT ON COLUMN plans.config_snapshot IS '计划生成时的 Agent 与策略快照，恢复时沿用';
COMMENT ON COLUMN plans.max_steps IS '最大步骤数，硬上限 50';
COMMENT ON COLUMN plans.total_input_tokens IS '计划累计输入 Token';
COMMENT ON COLUMN plans.total_output_tokens IS '计划累计输出 Token';
COMMENT ON COLUMN plans.estimated_cost IS '计划累计估算费用';
COMMENT ON COLUMN plans.deadline_at IS '计划执行截止时间，不得超过 Run 截止时间';
COMMENT ON COLUMN plans.error_code IS '失败业务码';
COMMENT ON COLUMN plans.error_message IS '脱敏失败原因';
COMMENT ON COLUMN plans.revision IS '乐观锁版本，每次状态推进递增；过期 Worker 写入被拒绝';
COMMENT ON COLUMN plans.finished_at IS '终态时间';
COMMENT ON TABLE plan_steps IS '计划步骤：保存依赖、工具参数、尝试次数、fencing 与结果，禁止终态回写';
COMMENT ON COLUMN plan_steps.id IS '步骤主键';
COMMENT ON COLUMN plan_steps.plan_id IS '所属计划，计划删除时级联清理';
COMMENT ON COLUMN plan_steps.user_id IS '所属用户，RLS 隔离键';
COMMENT ON COLUMN plan_steps.step_no IS '步骤序号，同一计划内唯一且连续';
COMMENT ON COLUMN plan_steps.title IS '步骤标题';
COMMENT ON COLUMN plan_steps.description IS '步骤说明，进入执行提示时受限';
COMMENT ON COLUMN plan_steps.depends_on IS '依赖的前置步骤序号数组，必须小于当前步骤';
COMMENT ON COLUMN plan_steps.tool_name IS '可选工具名；为空表示纯模型步骤';
COMMENT ON COLUMN plan_steps.arguments IS '工具参数快照，限制大小';
COMMENT ON COLUMN plan_steps.idempotency_key IS '副作用步骤的业务幂等键，崩溃恢复时避免重复执行';
COMMENT ON COLUMN plan_steps.status IS '步骤状态：pending/running/waiting_approval/completed/failed/skipped/cancelled/unknown';
COMMENT ON COLUMN plan_steps.attempts IS '已领取次数';
COMMENT ON COLUMN plan_steps.max_attempts IS '最大尝试次数，非幂等步骤失败不得自动盲重试';
COMMENT ON COLUMN plan_steps.fence_token IS '步骤执行栅栏令牌，与 Run 租约 fencing 绑定，过期 Worker 不得回写';
COMMENT ON COLUMN plan_steps.lease_owner IS '当前执行 Worker 标识';
COMMENT ON COLUMN plan_steps.execution_step_id IS '关联的通用执行步骤，用于复用载荷与审批审计';
COMMENT ON COLUMN plan_steps.result_json IS '步骤结果快照，限制大小';
COMMENT ON COLUMN plan_steps.error_code IS '失败业务码';
COMMENT ON COLUMN plan_steps.error_message IS '脱敏失败原因';
