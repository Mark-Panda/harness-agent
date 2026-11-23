-- 认领查询按 job_type + lane + 可用时间过滤；CONCURRENTLY 必须独立单语句迁移。
CREATE INDEX CONCURRENTLY IF NOT EXISTS jobs_claim_lane_idx
  ON jobs (job_type, lane, status, available_at, lease_expires_at);
