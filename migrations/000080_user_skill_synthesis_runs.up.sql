-- 000080：用户级技能自动提炼执行记录；每个用户每天一条，重试只更新同一条记录。
CREATE TABLE IF NOT EXISTS user_skill_synthesis_runs (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  run_date date NOT NULL,
  job_id uuid,
  status text NOT NULL DEFAULT 'running',
  outcome text NOT NULL DEFAULT '',
  detail text NOT NULL DEFAULT '',
  sessions_scanned integer NOT NULL DEFAULT 0,
  sessions_used integer NOT NULL DEFAULT 0,
  messages_used integer NOT NULL DEFAULT 0,
  candidate_name text NOT NULL DEFAULT '',
  confidence double precision NOT NULL DEFAULT 0,
  skill_id uuid,
  attempts integer NOT NULL DEFAULT 1,
  started_at timestamptz NOT NULL DEFAULT now(),
  finished_at timestamptz,
  updated_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT user_skill_synthesis_runs_status_check CHECK (status IN ('running', 'completed', 'skipped', 'failed')),
  CONSTRAINT user_skill_synthesis_runs_user_date_key UNIQUE (user_id, run_date)
);

ALTER TABLE user_skill_synthesis_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE user_skill_synthesis_runs FORCE ROW LEVEL SECURITY;
CREATE POLICY user_or_system ON user_skill_synthesis_runs FOR ALL TO harness_app
  USING (
    user_id = nullif(current_setting('app.user_id', true), '')::uuid
    OR nullif(current_setting('app.system', true), '') = 'on'
  )
  WITH CHECK (
    user_id = nullif(current_setting('app.user_id', true), '')::uuid
    OR nullif(current_setting('app.system', true), '') = 'on'
  );

GRANT SELECT, INSERT, UPDATE, DELETE ON user_skill_synthesis_runs TO harness_app;

COMMENT ON TABLE user_skill_synthesis_runs IS '每日技能自动提炼执行记录；同一用户同一天只保留一条，重试更新最终结果';
COMMENT ON COLUMN user_skill_synthesis_runs.run_date IS '任务对应 UTC 日期';
COMMENT ON COLUMN user_skill_synthesis_runs.outcome IS '结果原因：created/not_worthwhile/low_confidence/no_activity/disabled/model_unavailable/failed 等';
COMMENT ON COLUMN user_skill_synthesis_runs.detail IS '脱敏后的执行说明，便于解释为什么没有生成技能';

-- 补录升级前已完成的每日提炼任务，至少让界面能看到历史执行事实。
INSERT INTO user_skill_synthesis_runs (
  id, user_id, run_date, job_id, status, outcome, detail, attempts, started_at, finished_at, updated_at
)
SELECT
  gen_random_uuid(), src.user_id, src.run_date, src.job_id, src.status, 'history',
  '升级前历史任务，仅保留执行事实', src.attempts, src.started_at, src.finished_at, src.updated_at
FROM (
  SELECT
    j.user_id,
    CASE WHEN (j.payload->>'date') ~ '^\d{4}-\d{2}-\d{2}$' THEN (j.payload->>'date')::date END AS run_date,
    j.id AS job_id, j.attempts, j.created_at AS started_at,
    COALESCE(j.completed_at, j.updated_at) AS finished_at, j.updated_at,
    CASE j.status
      WHEN 'completed' THEN 'completed'
      WHEN 'failed' THEN 'failed'
      WHEN 'dead' THEN 'failed'
      ELSE 'skipped'
    END AS status
  FROM jobs j
  JOIN users u ON u.id = j.user_id
  WHERE j.job_type = 'skill.synthesize' AND j.user_id IS NOT NULL
) src
WHERE src.run_date IS NOT NULL
ON CONFLICT (user_id, run_date) DO NOTHING;
