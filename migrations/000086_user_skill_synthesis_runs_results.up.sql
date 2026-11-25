-- 000086：每日提炼记录按助手保存结构化结果，便于界面展示多助手产出。

ALTER TABLE user_skill_synthesis_runs
  ADD COLUMN IF NOT EXISTS results_json jsonb NOT NULL DEFAULT '[]'::jsonb;

COMMENT ON COLUMN user_skill_synthesis_runs.results_json IS '按助手聚合的提炼结果列表';
