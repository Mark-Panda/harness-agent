-- 放宽生成模型的上下文窗口硬下限：1M 会把大量主流模型排除在平台之外。
-- 实际可用性仍由模型策略的 min_context_tokens 与 Prompt 预算共同约束。
DO $$
DECLARE
  constraint_name text;
BEGIN
  FOR constraint_name IN
    SELECT conname
    FROM pg_constraint
    WHERE conrelid = 'llm_models'::regclass
      AND contype = 'c'
      AND pg_get_constraintdef(oid) LIKE '%context_window_tokens >= 1000000%'
  LOOP
    EXECUTE format('ALTER TABLE llm_models DROP CONSTRAINT %I', constraint_name);
  END LOOP;
  FOR constraint_name IN
    SELECT conname
    FROM pg_constraint
    WHERE conrelid = 'model_policies'::regclass
      AND contype = 'c'
      AND pg_get_constraintdef(oid) LIKE '%min_context_tokens >= 1000000%'
  LOOP
    EXECUTE format('ALTER TABLE model_policies DROP CONSTRAINT %I', constraint_name);
  END LOOP;
END $$;

ALTER TABLE llm_models
  ADD CONSTRAINT llm_models_context_window_tokens_min CHECK (context_window_tokens >= 32000);
ALTER TABLE model_policies
  ALTER COLUMN min_context_tokens SET DEFAULT 32000;
ALTER TABLE model_policies
  ADD CONSTRAINT model_policies_min_context_tokens_min CHECK (min_context_tokens >= 32000);

COMMENT ON COLUMN llm_models.context_window_tokens IS '上下文窗口 Token 数，chat 类型必须大于等于 32000';
COMMENT ON COLUMN model_policies.min_context_tokens IS '策略要求的最小上下文窗口，不得低于 32000';
