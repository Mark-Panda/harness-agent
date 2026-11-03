-- 000019：会话记忆策略。
-- 说明：群聊话题禁止注入个人长期记忆与结构化画像，并停止写入个人事实；
-- disabled 用于用户主动关闭记忆；既有会话默认 private 保持原有行为。

ALTER TABLE sessions ADD COLUMN memory_policy text NOT NULL DEFAULT 'private'
  CHECK (memory_policy IN ('private', 'group', 'disabled'));

COMMENT ON COLUMN sessions.memory_policy IS '会话记忆策略：private 私有、group 群聊话题、disabled 禁用记忆';

-- 存量飞书群聊话题回填 group，避免升级后继续把个人画像与长期记忆注入群聊；
-- 单聊绑定使用 thread_id = 'p2p'，保持 private。
UPDATE sessions
SET memory_policy = 'group'
FROM external_conversation_bindings
WHERE external_conversation_bindings.session_id = sessions.id
  AND external_conversation_bindings.thread_id IS NOT NULL
  AND external_conversation_bindings.thread_id <> 'p2p';
