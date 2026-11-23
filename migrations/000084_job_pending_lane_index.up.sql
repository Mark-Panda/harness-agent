-- 队列深度指标与 HPA 按 lane 统计 pending；CONCURRENTLY 必须独立单语句迁移。
CREATE INDEX CONCURRENTLY IF NOT EXISTS jobs_pending_lane_idx
  ON jobs (lane, available_at)
  WHERE status = 'pending';
