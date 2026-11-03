DO $roles$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'harness_app') THEN
    CREATE ROLE harness_app NOLOGIN;
  END IF;
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'harness_auth') THEN
    CREATE ROLE harness_auth NOLOGIN;
  END IF;
END
$roles$;

CREATE TABLE users (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  username text NOT NULL,
  username_normalized text NOT NULL,
  password_hash text NOT NULL,
  role text NOT NULL DEFAULT 'user' CHECK (role IN ('user', 'admin')),
  status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
  credential_version bigint NOT NULL DEFAULT 1 CHECK (credential_version > 0),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (username_normalized),
  CHECK (username = btrim(username)),
  CHECK (username_normalized = lower(btrim(username_normalized))),
  CHECK (length(username_normalized) BETWEEN 3 AND 64)
);

CREATE TABLE user_identities (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  provider text NOT NULL,
  tenant_key text NOT NULL DEFAULT '',
  external_subject text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (provider, tenant_key, external_subject),
  UNIQUE (user_id, provider, tenant_key)
);

CREATE TABLE refresh_tokens (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  family_id uuid NOT NULL,
  token_hash bytea NOT NULL,
  parent_token_id uuid REFERENCES refresh_tokens(id),
  credential_version bigint NOT NULL,
  expires_at timestamptz NOT NULL,
  rotated_at timestamptz,
  revoked_at timestamptz,
  reuse_detected_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  last_used_at timestamptz,
  UNIQUE (token_hash),
  CHECK (octet_length(token_hash) = 32),
  CHECK (expires_at > created_at)
);

CREATE INDEX refresh_tokens_user_active_idx ON refresh_tokens (user_id, expires_at) WHERE revoked_at IS NULL;
CREATE INDEX refresh_tokens_family_idx ON refresh_tokens (family_id);

ALTER TABLE users ENABLE ROW LEVEL SECURITY;
ALTER TABLE users FORCE ROW LEVEL SECURITY;
ALTER TABLE user_identities ENABLE ROW LEVEL SECURITY;
ALTER TABLE user_identities FORCE ROW LEVEL SECURITY;
ALTER TABLE refresh_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE refresh_tokens FORCE ROW LEVEL SECURITY;

CREATE POLICY users_app_self ON users FOR ALL TO harness_app
  USING (id = nullif(current_setting('app.user_id', true), '')::uuid)
  WITH CHECK (id = nullif(current_setting('app.user_id', true), '')::uuid);
CREATE POLICY users_auth_login ON users FOR SELECT TO harness_auth
  USING (
    id = nullif(current_setting('app.user_id', true), '')::uuid
    OR username_normalized = nullif(current_setting('app.auth_user_name', true), '')
  );
CREATE POLICY users_auth_register ON users FOR INSERT TO harness_auth
  WITH CHECK (nullif(current_setting('app.registration_enabled', true), '') = 'on');
CREATE POLICY users_auth_self_update ON users FOR UPDATE TO harness_auth
  USING (id = nullif(current_setting('app.user_id', true), '')::uuid)
  WITH CHECK (id = nullif(current_setting('app.user_id', true), '')::uuid);

CREATE POLICY identities_app_self ON user_identities FOR ALL TO harness_app
  USING (user_id = nullif(current_setting('app.user_id', true), '')::uuid)
  WITH CHECK (user_id = nullif(current_setting('app.user_id', true), '')::uuid);
CREATE POLICY identities_auth_select ON user_identities FOR SELECT TO harness_auth
  USING (
    user_id = nullif(current_setting('app.user_id', true), '')::uuid
    OR (
      provider = nullif(current_setting('app.identity_provider', true), '')
      AND tenant_key = nullif(current_setting('app.identity_tenant', true), '')
      AND external_subject = nullif(current_setting('app.identity_subject', true), '')
    )
  );
CREATE POLICY identities_auth_insert ON user_identities FOR INSERT TO harness_auth
  WITH CHECK (user_id = nullif(current_setting('app.user_id', true), '')::uuid);
CREATE POLICY identities_auth_update ON user_identities FOR UPDATE TO harness_auth
  USING (user_id = nullif(current_setting('app.user_id', true), '')::uuid)
  WITH CHECK (user_id = nullif(current_setting('app.user_id', true), '')::uuid);
CREATE POLICY identities_auth_delete ON user_identities FOR DELETE TO harness_auth
  USING (user_id = nullif(current_setting('app.user_id', true), '')::uuid);

CREATE POLICY refresh_app_self ON refresh_tokens FOR ALL TO harness_app
  USING (user_id = nullif(current_setting('app.user_id', true), '')::uuid)
  WITH CHECK (user_id = nullif(current_setting('app.user_id', true), '')::uuid);
CREATE POLICY refresh_auth_select ON refresh_tokens FOR SELECT TO harness_auth
  USING (
    user_id = nullif(current_setting('app.user_id', true), '')::uuid
    OR token_hash = decode(nullif(current_setting('app.refresh_token_hash', true), ''), 'hex')
  );
CREATE POLICY refresh_auth_insert ON refresh_tokens FOR INSERT TO harness_auth
  WITH CHECK (user_id = nullif(current_setting('app.user_id', true), '')::uuid);
CREATE POLICY refresh_auth_update ON refresh_tokens FOR UPDATE TO harness_auth
  USING (
    user_id = nullif(current_setting('app.user_id', true), '')::uuid
    OR token_hash = decode(nullif(current_setting('app.refresh_token_hash', true), ''), 'hex')
  )
  WITH CHECK (user_id = nullif(current_setting('app.user_id', true), '')::uuid);
CREATE POLICY refresh_auth_delete ON refresh_tokens FOR DELETE TO harness_auth
  USING (user_id = nullif(current_setting('app.user_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON users, user_identities, refresh_tokens TO harness_app;
GRANT SELECT, INSERT, UPDATE ON users TO harness_auth;
GRANT SELECT, INSERT, UPDATE, DELETE ON user_identities, refresh_tokens TO harness_auth;
