-- 000012：扩展会话摘要版本生命周期，支撑异步摘要任务的来源范围、版本幂等与失效。
-- 说明：摘要只保存模型生成的公开文本与来源元数据，不保存隐藏思维链或模型请求原文。

ALTER TABLE session_summaries
  -- 版本状态：active 当前生效，superseded 已被新版本替换，invalidated 来源失效。
  ADD COLUMN status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'superseded', 'invalidated')),
  -- 本版本新增来源范围的起始消息，便于审计与排序校验。
  ADD COLUMN source_start_message_id uuid,
  -- 本版本新增来源范围的结束消息，也是摘要覆盖到的边界消息。
  ADD COLUMN source_end_message_id uuid,
  -- 本版本新增来源范围的结束时间，与消息 ID 组合形成稳定排序边界。
  ADD COLUMN source_end_at timestamptz,
  -- 本版本新增来源消息的内容指纹，用于检测来源缺失或内容被修改。
  ADD COLUMN source_fingerprint text NOT NULL DEFAULT '',
  -- 本版本累计覆盖的消息总数，用于检测边界之前历史消息被删除。
  ADD COLUMN covered_message_count integer NOT NULL DEFAULT 0 CHECK (covered_message_count >= 0),
  -- 上一版本摘要 ID，形成可追溯的版本链。
  ADD COLUMN parent_summary_id uuid REFERENCES session_summaries(id),
  -- 生成摘要使用的供应商与模型，仅保存元数据。
  ADD COLUMN model_provider_id uuid,
  ADD COLUMN model_id text NOT NULL DEFAULT '',
  -- 本次摘要调用消耗的 Token，用于成本核算与审计。
  ADD COLUMN input_tokens integer NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
  ADD COLUMN output_tokens integer NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
  -- 最后更新时间，失效与替换时写入。
  ADD COLUMN updated_at timestamptz NOT NULL DEFAULT now();

-- 同一会话最多只有一个生效摘要，写入新版本时必须在同一事务内替换旧版本。
CREATE UNIQUE INDEX session_summaries_one_active ON session_summaries (session_id) WHERE status = 'active';
-- 按用户与会话读取版本历史，供审计、导出与失效排查。
CREATE INDEX session_summaries_user_version_idx ON session_summaries (user_id, session_id, version DESC);

COMMENT ON COLUMN session_summaries.status IS '摘要版本状态：active 生效、superseded 已替换、invalidated 来源失效';
COMMENT ON COLUMN session_summaries.source_start_message_id IS '本版本新增来源范围的起始消息 ID';
COMMENT ON COLUMN session_summaries.source_end_message_id IS '本版本新增来源范围的结束消息 ID，也是摘要覆盖边界';
COMMENT ON COLUMN session_summaries.source_end_at IS '来源范围结束消息的创建时间，与 ID 组成稳定排序';
COMMENT ON COLUMN session_summaries.source_fingerprint IS '本版本新增来源消息的内容指纹，检测来源缺失或篡改';
COMMENT ON COLUMN session_summaries.covered_message_count IS '摘要累计覆盖的非删除消息数，用于检测历史消息被删除';
COMMENT ON COLUMN session_summaries.parent_summary_id IS '上一版本摘要 ID，形成版本链';
COMMENT ON COLUMN session_summaries.model_provider_id IS '生成摘要所用供应商 ID，仅元数据';
COMMENT ON COLUMN session_summaries.model_id IS '生成摘要所用模型标识，仅元数据';
COMMENT ON COLUMN session_summaries.input_tokens IS '摘要模型调用输入 Token 数';
COMMENT ON COLUMN session_summaries.output_tokens IS '摘要模型调用输出 Token 数';
COMMENT ON COLUMN session_summaries.updated_at IS '摘要版本最后更新时间（UTC）';
