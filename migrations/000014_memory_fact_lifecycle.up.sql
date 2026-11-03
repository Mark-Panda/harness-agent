-- 000014：结构化事实的来源类型、显式纠正标记与单活跃版本约束。
-- 说明：事实只保存用户可公开的偏好与稳定信息，不保存隐藏思维链或临时对话内容。

ALTER TABLE memory_facts
  -- 来源类型：user 用户陈述、assistant 助手归纳、correction 用户显式纠正。
  ADD COLUMN source_kind text NOT NULL DEFAULT 'assistant' CHECK (source_kind IN ('user', 'assistant', 'correction')),
  -- 是否为用户显式纠正；低置信度候选不得静默覆盖该事实。
  ADD COLUMN corrected boolean NOT NULL DEFAULT false,
  -- 提取元数据（模型、版本、来源索引），禁止包含凭证或隐藏推理。
  ADD COLUMN metadata jsonb NOT NULL DEFAULT '{}'::jsonb;

-- 同一用户的同一事实键最多只有一个生效版本。
CREATE UNIQUE INDEX memory_facts_one_active ON memory_facts (user_id, fact_key) WHERE valid_to IS NULL;
CREATE INDEX memory_facts_user_active_idx ON memory_facts (user_id, valid_from DESC) WHERE valid_to IS NULL;

COMMENT ON COLUMN memory_facts.source_kind IS '事实来源类型：user/assistant/correction';
COMMENT ON COLUMN memory_facts.corrected IS '是否用户显式纠正，低置信度事实不得静默覆盖';
COMMENT ON COLUMN memory_facts.metadata IS '事实提取元数据，禁止包含凭证或隐藏推理';
COMMENT ON COLUMN memory_facts.valid_from IS '事实版本生效时间';
COMMENT ON COLUMN memory_facts.valid_to IS '事实版本失效时间，空表示当前生效';
COMMENT ON COLUMN memory_facts.version IS '同一事实键的版本号，单调递增';
