-- 000011：新增 usage_counters 表，按用户/Agent/Provider 聚合每日 Token 与费用预算。
-- 说明：预算计数只保存聚合数值，不包含用户正文；应用角色可原子累加，供超限拒绝与阈值告警使用。

CREATE TABLE usage_counters (
  scope text NOT NULL CHECK (scope IN ('user', 'agent', 'provider')),
  scope_id uuid NOT NULL,
  window_start timestamptz NOT NULL,
  input_tokens bigint NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
  output_tokens bigint NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
  estimated_cost numeric(20, 8) NOT NULL DEFAULT 0 CHECK (estimated_cost >= 0),
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (scope, scope_id, window_start)
);

CREATE INDEX usage_counters_window_idx ON usage_counters (window_start);

ALTER TABLE usage_counters ENABLE ROW LEVEL SECURITY;
-- 表内仅为平台聚合计数，不含用户正文；应用角色需要跨用户累加 Provider 维度，因此策略允许读写聚合行。
CREATE POLICY usage_counter_access ON usage_counters FOR ALL TO harness_app
  USING (true) WITH CHECK (true);

GRANT SELECT, INSERT, UPDATE ON usage_counters TO harness_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON usage_counters TO harness_admin;

COMMENT ON TABLE usage_counters IS '模型调用每日预算聚合表：按用户、Agent、Provider 维度累计 Token 与估算费用';
COMMENT ON COLUMN usage_counters.scope IS '聚合维度：user 用户、agent Agent、provider 供应商';
COMMENT ON COLUMN usage_counters.scope_id IS '聚合维度对应的实体 ID，不指向用户私有内容';
COMMENT ON COLUMN usage_counters.window_start IS '统计窗口起始时间，当前为 UTC 自然日零点';
COMMENT ON COLUMN usage_counters.input_tokens IS '窗口内累计输入 Token';
COMMENT ON COLUMN usage_counters.output_tokens IS '窗口内累计输出 Token';
COMMENT ON COLUMN usage_counters.estimated_cost IS '窗口内累计估算费用，单位与价格配置一致';
COMMENT ON COLUMN usage_counters.updated_at IS '最近累加时间（UTC）';
