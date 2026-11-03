-- 000015：为删除请求增加租约、重试与并发去重字段，支撑异步删除编排。
-- 说明：删除请求只保存资源类型与 ID，不保存被删除内容。

ALTER TABLE deletion_requests
  -- 已领取次数，用于重试预算与审计。
  ADD COLUMN attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  -- 最大尝试次数，超过后进入 failed 等待人工处理。
  ADD COLUMN max_attempts integer NOT NULL DEFAULT 5 CHECK (max_attempts > 0),
  -- 下次可领取时间，用于失败退避。
  ADD COLUMN available_at timestamptz NOT NULL DEFAULT now(),
  -- 当前租约持有者，空表示未运行。
  ADD COLUMN lease_owner text NOT NULL DEFAULT '',
  -- 租约到期时间，过期后可被其他 Worker 恢复。
  ADD COLUMN lease_expires_at timestamptz,
  -- 租约栅栏令牌，防止过期 Worker 提交完成状态。
  ADD COLUMN fencing_token bigint NOT NULL DEFAULT 0 CHECK (fencing_token >= 0),
  -- 请求来源（web/api/admin），用于审计。
  ADD COLUMN requested_source text NOT NULL DEFAULT 'web',
  ADD CONSTRAINT deletion_attempts_check CHECK (attempts <= max_attempts);

CREATE INDEX deletion_requests_claim_idx ON deletion_requests (status, available_at, created_at);
-- 同一用户同一资源最多只有一个待处理请求，重复请求返回既有任务。
CREATE UNIQUE INDEX deletion_requests_active_resource
  ON deletion_requests (user_id, resource_type, resource_id)
  WHERE status IN ('pending', 'running');

-- 删除 Worker 需要跨用户领取任务；系统作用域仅用于后台任务，普通请求仍受用户隔离策略约束。
CREATE POLICY deletion_requests_system ON deletion_requests FOR ALL TO harness_app
  USING (nullif(current_setting('app.system', true), '') = 'on')
  WITH CHECK (nullif(current_setting('app.system', true), '') = 'on');

COMMENT ON COLUMN deletion_requests.attempts IS '删除任务已领取次数';
COMMENT ON COLUMN deletion_requests.max_attempts IS '删除任务最大尝试次数';
COMMENT ON COLUMN deletion_requests.available_at IS '删除任务下次可领取时间';
COMMENT ON COLUMN deletion_requests.lease_owner IS '当前租约持有者';
COMMENT ON COLUMN deletion_requests.lease_expires_at IS '租约到期时间，过期后允许恢复';
COMMENT ON COLUMN deletion_requests.fencing_token IS '租约栅栏令牌，防止过期 Worker 提交完成状态';
COMMENT ON COLUMN deletion_requests.requested_source IS '删除请求来源：web/api/admin';
