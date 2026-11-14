-- 多租户 SaaS：feishu_bots 增加归属用户，绑定与列表按租户隔离。
-- 同时清理历史单聊消息的 speaker_open_id（此前误把单聊写入发言人导致上下文被标注为群聊）。

-- 1. 归属用户字段（先允许 NULL 以便回填）。
ALTER TABLE feishu_bots ADD COLUMN IF NOT EXISTS owner_user_id uuid REFERENCES users(id) ON DELETE CASCADE;

-- 2. 已绑定机器人优先归属原绑定用户，避免升级后原租户失去机器人。
UPDATE feishu_bots AS bot
SET owner_user_id = binding.user_id
FROM agent_feishu_bots AS binding
WHERE binding.bot_id = bot.id AND bot.owner_user_id IS NULL;

-- 未绑定机器人回填到最早的管理员账号，保持现有管理台可用。
UPDATE feishu_bots
SET owner_user_id = (
    SELECT id FROM users WHERE role = 'admin' ORDER BY created_at ASC LIMIT 1
)
WHERE owner_user_id IS NULL;

-- 若库中没有任何管理员，则归属到任意一个用户，避免 NOT NULL 迁移失败。
UPDATE feishu_bots
SET owner_user_id = (SELECT id FROM users ORDER BY created_at ASC LIMIT 1)
WHERE owner_user_id IS NULL;

ALTER TABLE feishu_bots ALTER COLUMN owner_user_id SET NOT NULL;

-- 3. 名称唯一性改为租户内唯一；app_id 仍全局唯一（飞书应用维度）。
ALTER TABLE feishu_bots DROP CONSTRAINT IF EXISTS feishu_bots_name_key;
ALTER TABLE feishu_bots ADD CONSTRAINT feishu_bots_owner_name_key UNIQUE (owner_user_id, name);

-- 4. RLS：归属用户可读写自己的机器人，系统作用域（管理/事件路由/长连接）仍可全量访问。
DROP POLICY IF EXISTS feishu_bots_system ON feishu_bots;
CREATE POLICY feishu_bots_owner ON feishu_bots FOR ALL TO harness_app
    USING (
        owner_user_id = nullif(current_setting('app.user_id', true), '')::uuid
        OR nullif(current_setting('app.system', true), '') = 'on'
    )
    WITH CHECK (
        owner_user_id = nullif(current_setting('app.user_id', true), '')::uuid
        OR nullif(current_setting('app.system', true), '') = 'on'
    );

-- 5. 历史单聊消息发言人清理：p2p 会话的发言人即会话所有者，不应带群聊发言人标识。
UPDATE messages
SET speaker_open_id = ''
WHERE speaker_open_id <> ''
  AND session_id IN (
      SELECT session_id
      FROM external_conversation_bindings
      WHERE provider = 'feishu' AND thread_id = 'p2p'
  );
