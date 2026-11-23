-- 会话压缩步骤的载荷类型：记录摘要版本、覆盖消息数与上下文裁剪指标。
-- 只放宽既有 CHECK 约束，历史数据不受影响。
ALTER TABLE execution_payloads DROP CONSTRAINT IF EXISTS execution_payloads_payload_type_check;
ALTER TABLE execution_payloads ADD CONSTRAINT execution_payloads_payload_type_check
    CHECK (payload_type = ANY (ARRAY['model_request'::text, 'model_response'::text, 'tool_input'::text, 'tool_output'::text, 'tool_error'::text, 'external_request'::text, 'external_response'::text, 'context_compression'::text]));
