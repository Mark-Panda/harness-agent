-- 恢复 embedding/rerank 模型的上下文窗口豁免：1M 下限只应约束生成类（chat）模型。
-- 平台代码仅在 model_kind = 'chat' 时校验 MinGenerationContextTokens，
-- 000040/000041 却把下限应用到了所有类型，会导致向量模型无法保存。
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
END $$;

ALTER TABLE llm_models
  ADD CONSTRAINT llm_models_context_window_tokens_min
  CHECK (model_kind <> 'chat' OR context_window_tokens >= 1000000);

COMMENT ON COLUMN llm_models.context_window_tokens IS '上下文窗口 Token 数，chat 类型必须大于等于 1000000';
