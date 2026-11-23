-- 为持久化任务增加资源池维度：lane 决定由哪类 Worker 池认领，tenant_key 预留给租户级资源池，priority 预留给高优任务。
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS lane text NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS tenant_key text NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS priority integer NOT NULL DEFAULT 0;

-- 存量 Run 任务按 Agent 模式回填 lane；无法匹配的旧任务保留空 lane，由 Worker 作为兼容任务认领。
UPDATE jobs AS j
SET lane = ar.mode
FROM agent_runs AS ar
WHERE j.job_type = 'agent.run'
  AND j.lane = ''
  AND j.payload->>'run_id' = ar.id::text;

-- 后台维护任务统一进入 background 池，飞书事件进入 feishu 池。
UPDATE jobs
SET lane = 'background'
WHERE lane = ''
  AND job_type IN ('memory.summarize', 'memory.embed', 'memory.fact.extract', 'skill.synthesize');

UPDATE jobs
SET lane = 'feishu'
WHERE lane = ''
  AND job_type = 'feishu.event';
