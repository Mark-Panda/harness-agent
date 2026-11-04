-- 飞书机器人注册表：管理员登记多个飞书应用机器人，助手可绑定其中一个。
-- 应用密钥、Verification Token 与 Encrypt Key 以加密凭证保存，任何查询接口都不返回明文。
CREATE TABLE IF NOT EXISTS feishu_bots (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name text NOT NULL,
  app_id text NOT NULL,
  bot_open_id text NOT NULL DEFAULT '',
  key_version text NOT NULL,
  credentials bytea NOT NULL,
  status text NOT NULL DEFAULT 'enabled' CHECK (status IN ('enabled', 'disabled')),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (name),
  UNIQUE (app_id),
  CHECK (octet_length(credentials) > 0 AND octet_length(credentials) <= 65536)
);

ALTER TABLE feishu_bots ENABLE ROW LEVEL SECURITY;
ALTER TABLE feishu_bots FORCE ROW LEVEL SECURITY;
-- 机器人配置由管理员通过系统作用域维护，Worker 与事件接入也以系统身份读取。
CREATE POLICY feishu_bots_system ON feishu_bots FOR ALL TO harness_app
  USING (nullif(current_setting('app.system', true), '') = 'on')
  WITH CHECK (nullif(current_setting('app.system', true), '') = 'on');
GRANT SELECT, INSERT, UPDATE, DELETE ON feishu_bots TO harness_app;

-- 助手与机器人绑定：bot_id 主键保证一个机器人只能被一个助手绑定；
-- agent_id 唯一保证一个助手最多绑定一个机器人。
CREATE TABLE IF NOT EXISTS agent_feishu_bots (
  bot_id uuid PRIMARY KEY REFERENCES feishu_bots(id) ON DELETE CASCADE,
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  agent_id uuid NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (agent_id)
);

ALTER TABLE agent_feishu_bots ENABLE ROW LEVEL SECURITY;
ALTER TABLE agent_feishu_bots FORCE ROW LEVEL SECURITY;
CREATE POLICY user_isolation ON agent_feishu_bots FOR ALL TO harness_app
  USING (user_id = nullif(current_setting('app.user_id', true), '')::uuid)
  WITH CHECK (user_id = nullif(current_setting('app.user_id', true), '')::uuid);
-- Worker 按机器人查找绑定时需要跨用户系统读取。
CREATE POLICY agent_feishu_bots_system ON agent_feishu_bots FOR SELECT TO harness_app
  USING (nullif(current_setting('app.system', true), '') = 'on');
GRANT SELECT, INSERT, UPDATE, DELETE ON agent_feishu_bots TO harness_app;

-- 外发回复记录所属机器人应用，回复 Worker 据此选择正确的飞书客户端。
ALTER TABLE feishu_outbound_messages ADD COLUMN app_id text NOT NULL DEFAULT '';

-- 机器人列表与事件路由需要在系统作用域读取助手名称、版本与状态。
CREATE POLICY agents_app_system_read ON agents FOR SELECT TO harness_app
  USING (nullif(current_setting('app.system', true), '') = 'on');


COMMENT ON TABLE feishu_bots IS '飞书机器人注册表：管理员登记的飞书应用机器人';
COMMENT ON COLUMN feishu_bots.app_id IS '飞书应用 App ID，事件回执按此路由到机器人';
COMMENT ON COLUMN feishu_bots.bot_open_id IS '机器人 open_id，用于群聊 @ 判断';
COMMENT ON COLUMN feishu_bots.credentials IS '应用密钥、Verification Token 与 Encrypt Key 的 AES-256-GCM 密文';
COMMENT ON TABLE agent_feishu_bots IS '助手与飞书机器人绑定：一个机器人只能被一个助手绑定';
COMMENT ON COLUMN feishu_outbound_messages.app_id IS '回复使用的飞书应用 App ID';
