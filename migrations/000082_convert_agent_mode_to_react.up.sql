-- 服务仅保留 ReAct 编排模式：历史 Plan/Supervisor 助手统一迁移为 react。
-- 历史 Run 的 mode 保持不变，用于审计追溯。
UPDATE agents SET mode = 'react' WHERE mode <> 'react';
