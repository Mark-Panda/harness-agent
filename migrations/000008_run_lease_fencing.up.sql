-- 000008：为 agent_runs 增加 Worker 租约与 fencing token。
-- 说明：Job 队列负责任务认领，Run 行保存执行租约；过期 Worker 的提交必须被 fencing token 拒绝。

ALTER TABLE agent_runs
  ADD COLUMN lease_owner text NOT NULL DEFAULT '',
  ADD COLUMN lease_expires_at timestamptz,
  ADD COLUMN fencing_token bigint NOT NULL DEFAULT 0 CHECK (fencing_token >= 0);

COMMENT ON COLUMN agent_runs.lease_owner IS '当前持有执行租约的 Worker 实例标识，空串表示无人持有';
COMMENT ON COLUMN agent_runs.lease_expires_at IS '执行租约到期时间，过期后允许其他 Worker 认领恢复';
COMMENT ON COLUMN agent_runs.fencing_token IS '租约栅栏令牌，每次认领递增，过期 Worker 不得提交结果';

CREATE INDEX agent_runs_lease_idx ON agent_runs (status, lease_expires_at) WHERE finished_at IS NULL;
