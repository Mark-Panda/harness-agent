-- 一次性短期改密/找回凭据令牌：仅认证角色可读写，不向应用角色暴露。
-- 令牌只保存带 Pepper 的哈希，单次消费且 15 分钟过期；账号删除时随用户外键级联清理。
CREATE TABLE IF NOT EXISTS password_reset_tokens (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  token_hash bytea NOT NULL UNIQUE,
  expires_at timestamp with time zone NOT NULL,
  used_at timestamp with time zone,
  created_at timestamp with time zone NOT NULL DEFAULT now(),
  CONSTRAINT password_reset_tokens_hash_check CHECK (octet_length(token_hash) = 32),
  CONSTRAINT password_reset_tokens_expiry_check CHECK (expires_at > created_at)
);
CREATE INDEX IF NOT EXISTS password_reset_tokens_user_idx ON password_reset_tokens (user_id);
GRANT SELECT, INSERT, UPDATE, DELETE ON password_reset_tokens TO harness_auth;
