-- 记录发起审批的会话，Supervisor 子任务审批结果需要回填到子会话而不是主会话。
ALTER TABLE tool_approvals ADD COLUMN IF NOT EXISTS session_id uuid;
