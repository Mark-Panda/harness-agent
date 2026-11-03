-- 000010：新增 tool_approvals 表，承载中高风险工具的人工审批与幂等执行事实。
-- 说明：未获批准的工具调用不得产生任何副作用；审批记录与执行步骤、Run 事件共同构成可审计链路。

CREATE TABLE tool_approvals (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  run_id uuid NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
  step_id uuid REFERENCES execution_steps(id) ON DELETE SET NULL,
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  tool_call_id text NOT NULL,
  tool_name text NOT NULL,
  arguments jsonb NOT NULL DEFAULT '{}'::jsonb,
  risk_level text NOT NULL DEFAULT 'high' CHECK (risk_level IN ('low', 'medium', 'high')),
  status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'denied', 'executing', 'executed', 'failed')),
  idempotency_key text NOT NULL,
  result_json jsonb,
  error_code text NOT NULL DEFAULT '',
  error_message text NOT NULL DEFAULT '',
  decided_by uuid REFERENCES users(id),
  decided_at timestamptz,
  delivered_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (run_id, tool_call_id),
  CHECK (octet_length(tool_call_id) BETWEEN 1 AND 256),
  CHECK (octet_length(tool_name) BETWEEN 1 AND 256),
  CHECK (octet_length(idempotency_key) BETWEEN 1 AND 256),
  CHECK (jsonb_typeof(arguments) IN ('object', 'array')),
  CHECK (octet_length(arguments::text) <= 262144),
  CHECK (status IN ('executed', 'failed', 'denied') OR delivered_at IS NULL)
);

CREATE INDEX tool_approvals_run_status_idx ON tool_approvals (run_id, status);
CREATE INDEX tool_approvals_user_status_idx ON tool_approvals (user_id, status);
CREATE INDEX tool_approvals_run_created_idx ON tool_approvals (run_id, created_at, id);

ALTER TABLE tool_approvals ENABLE ROW LEVEL SECURITY;
ALTER TABLE tool_approvals FORCE ROW LEVEL SECURITY;
CREATE POLICY user_isolation ON tool_approvals FOR ALL TO harness_app
  USING (user_id = nullif(current_setting('app.user_id', true), '')::uuid)
  WITH CHECK (user_id = nullif(current_setting('app.user_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON tool_approvals TO harness_app;

COMMENT ON TABLE tool_approvals IS '中高风险工具人工审批表：保存待确认参数、决策、幂等键与执行结果，未获批准不得执行';
COMMENT ON COLUMN tool_approvals.id IS '审批主键，同一 Run 内配合 tool_call_id 唯一';
COMMENT ON COLUMN tool_approvals.run_id IS '所属 Run，Run 删除时级联清理审批记录';
COMMENT ON COLUMN tool_approvals.step_id IS '请求审批的工具执行步骤，步骤删除后保留审批事实并置空';
COMMENT ON COLUMN tool_approvals.user_id IS '所属用户，RLS 隔离键，非本人不得查看或决策';
COMMENT ON COLUMN tool_approvals.tool_call_id IS '模型产生的 tool_call_id，用于把审批结果回填为 tool 消息';
COMMENT ON COLUMN tool_approvals.tool_name IS '规范化工具名，与执行步骤保持一致';
COMMENT ON COLUMN tool_approvals.arguments IS '待确认的工具参数快照，仅保存脱敏或截断后的有限正文';
COMMENT ON COLUMN tool_approvals.risk_level IS '工具风险等级：low 自动执行，medium/high 必须人工审批';
COMMENT ON COLUMN tool_approvals.status IS '审批状态：pending 待决策，approved 已批准待执行，denied 已拒绝，executing 执行中，executed 已执行，failed 执行失败';
COMMENT ON COLUMN tool_approvals.idempotency_key IS '审批执行幂等键，通过 MCP _meta 传递，崩溃恢复时用于识别重复副作用';
COMMENT ON COLUMN tool_approvals.result_json IS '工具结果快照，仅在执行成功后写入并按大小上限截断';
COMMENT ON COLUMN tool_approvals.error_code IS '执行失败业务码，禁止保存凭证或隐藏推理内容';
COMMENT ON COLUMN tool_approvals.error_message IS '脱敏后的执行失败原因';
COMMENT ON COLUMN tool_approvals.decided_by IS '决策人用户 ID，必须与 Run 所属用户一致';
COMMENT ON COLUMN tool_approvals.decided_at IS '决策时间（UTC）';
COMMENT ON COLUMN tool_approvals.delivered_at IS '结果已回填为 tool 消息的时间，用于恢复时避免重复投递';
COMMENT ON COLUMN tool_approvals.created_at IS '创建时间（UTC）';
COMMENT ON COLUMN tool_approvals.updated_at IS '更新时间（UTC）';
