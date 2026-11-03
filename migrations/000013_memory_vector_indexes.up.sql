-- 000013：长期记忆向量索引元数据、来源列与系统维护读取策略。
-- 说明：向量只保存用户消息的公开内容与数值向量，不保存隐藏思维链或模型原始请求。

ALTER TABLE memory_vectors
  -- 来源类型：message 消息、summary 摘要、fact 结构化事实。
  ADD COLUMN source_type text NOT NULL DEFAULT 'message' CHECK (source_type IN ('message', 'summary', 'fact')),
  -- 来源实体 ID，用于幂等写入、删除与回溯。
  ADD COLUMN source_id uuid,
  -- 来源内容哈希，用于避免重复 embedding 调用并在内容变更时重建。
  ADD COLUMN content_hash text NOT NULL DEFAULT '';

-- 同一用户、同一来源、同一索引版本只保留一条有效向量，重复任务不会产生重复记忆。
CREATE UNIQUE INDEX memory_vectors_source_version
  ON memory_vectors (user_id, source_type, source_id, embedding_version)
  WHERE deleted_at IS NULL AND source_id IS NOT NULL;
-- 按用户与索引版本检索，配合 HNSW cosine 索引缩小扫描范围。
CREATE INDEX memory_vectors_user_version_idx ON memory_vectors (user_id, embedding_version) WHERE deleted_at IS NULL;
CREATE INDEX memory_vectors_session_idx ON memory_vectors (session_id) WHERE deleted_at IS NULL;

-- 平台级向量索引版本：记录模型、维度、状态与双写开关，支撑不停机重建与切换。
CREATE TABLE embedding_indexes (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  -- 索引名称，平台内唯一。
  name text NOT NULL UNIQUE,
  -- 生成向量使用的供应商。
  provider_id uuid NOT NULL REFERENCES llm_providers(id) ON DELETE RESTRICT,
  -- 供应商侧 embedding 模型名。
  embedding_model text NOT NULL,
  -- 向量维度，当前固定 1536。
  dimension integer NOT NULL DEFAULT 1536 CHECK (dimension = 1536),
  -- 索引版本，写入 memory_vectors.embedding_version；平台内唯一，便于按版本切换。
  version text NOT NULL UNIQUE,
  -- 索引生命周期：staging 回填中、active 当前检索、retired 已退役。
  status text NOT NULL DEFAULT 'staging' CHECK (status IN ('staging', 'active', 'retired')),
  -- 是否存在 staging 版本时是否同时写入 active 与 staging。
  dual_write boolean NOT NULL DEFAULT true,
  -- 索引配置与校验信息，禁止写入凭证。
  metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (provider_id, embedding_model, dimension, version)
);

-- 同一维度最多一个 active 索引，切换必须在事务内完成。
CREATE UNIQUE INDEX embedding_indexes_one_active ON embedding_indexes (dimension) WHERE status = 'active';
CREATE INDEX embedding_indexes_status_idx ON embedding_indexes (status, updated_at);

-- 维护任务需要跨用户统计覆盖度与扫描缺失向量；仅开放 SELECT，写入仍受用户隔离约束。
CREATE POLICY memory_vectors_system_read ON memory_vectors FOR SELECT TO harness_app
  USING (nullif(current_setting('app.system', true), '') = 'on');
CREATE POLICY messages_system_read ON messages FOR SELECT TO harness_app
  USING (nullif(current_setting('app.system', true), '') = 'on');

GRANT SELECT, INSERT, UPDATE, DELETE ON embedding_indexes TO harness_app;
GRANT SELECT ON embedding_indexes TO harness_admin;

COMMENT ON TABLE embedding_indexes IS '平台级 embedding 索引版本表：记录模型、维度、状态与双写配置';
COMMENT ON COLUMN embedding_indexes.name IS '索引名称，平台内唯一';
COMMENT ON COLUMN embedding_indexes.provider_id IS '生成向量使用的供应商 ID';
COMMENT ON COLUMN embedding_indexes.embedding_model IS '供应商侧 embedding 模型名';
COMMENT ON COLUMN embedding_indexes.dimension IS '向量维度，当前固定 1536';
COMMENT ON COLUMN embedding_indexes.version IS '索引版本，写入 memory_vectors.embedding_version';
COMMENT ON COLUMN embedding_indexes.status IS '索引状态：staging 回填中、active 当前检索、retired 已退役';
COMMENT ON COLUMN embedding_indexes.dual_write IS '存在 staging 版本时是否双写 active 与 staging';
COMMENT ON COLUMN embedding_indexes.metadata IS '索引配置与校验信息，禁止包含凭证';
COMMENT ON COLUMN memory_vectors.source_type IS '来源类型：message 消息、summary 摘要、fact 结构化事实';
COMMENT ON COLUMN memory_vectors.source_id IS '来源实体 ID，用于幂等写入与删除';
COMMENT ON COLUMN memory_vectors.content_hash IS '来源内容哈希，内容变更时触发重建向量';
