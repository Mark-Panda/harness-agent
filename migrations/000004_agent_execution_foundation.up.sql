DO $roles$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'harness_admin') THEN
    CREATE ROLE harness_admin NOLOGIN;
  END IF;
END
$roles$;

CREATE TABLE llm_providers (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name text NOT NULL UNIQUE,
  provider_type text NOT NULL DEFAULT 'openai_compatible' CHECK (provider_type = 'openai_compatible'),
  base_url text NOT NULL,
  api_key_encrypted bytea NOT NULL,
  key_version text NOT NULL,
  status text NOT NULL DEFAULT 'enabled' CHECK (status IN ('enabled', 'disabled')),
  data_region text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE llm_models (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  provider_id uuid NOT NULL REFERENCES llm_providers(id),
  remote_model_name text NOT NULL,
  model_kind text NOT NULL CHECK (model_kind IN ('chat', 'embedding', 'rerank')),
  context_window_tokens bigint NOT NULL,
  max_output_tokens integer NOT NULL,
  capabilities jsonb NOT NULL DEFAULT '{}'::jsonb,
  input_price_per_million numeric(20, 8) NOT NULL DEFAULT 0,
  output_price_per_million numeric(20, 8) NOT NULL DEFAULT 0,
  data_region text NOT NULL DEFAULT '',
  verification_source text NOT NULL DEFAULT '',
  verified_at timestamptz,
  status text NOT NULL DEFAULT 'enabled' CHECK (status IN ('enabled', 'disabled')),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (provider_id, remote_model_name, model_kind),
  CHECK (context_window_tokens > 0),
  CHECK (max_output_tokens > 0),
  CHECK (model_kind IN ('embedding', 'rerank') OR context_window_tokens >= 1000000)
);

CREATE TABLE model_policies (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name text NOT NULL,
  version integer NOT NULL CHECK (version > 0),
  min_context_tokens bigint NOT NULL DEFAULT 1000000 CHECK (min_context_tokens >= 1000000),
  rules jsonb NOT NULL DEFAULT '{}'::jsonb,
  status text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'published', 'disabled')),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (name, version),
  UNIQUE (id, version)
);

CREATE TABLE model_policy_candidates (
  policy_id uuid NOT NULL REFERENCES model_policies(id) ON DELETE CASCADE,
  model_id uuid NOT NULL REFERENCES llm_models(id),
  priority integer NOT NULL CHECK (priority > 0),
  constraints jsonb NOT NULL DEFAULT '{}'::jsonb,
  PRIMARY KEY (policy_id, model_id),
  UNIQUE (policy_id, priority)
);

CREATE TABLE skills (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name text NOT NULL,
  version integer NOT NULL CHECK (version > 0),
  description text NOT NULL DEFAULT '',
  prompt_template text NOT NULL,
  parameters_json jsonb NOT NULL DEFAULT '{}'::jsonb,
  status text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'published', 'disabled')),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (name, version)
);

CREATE TABLE mcps (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name text NOT NULL UNIQUE,
  endpoint text NOT NULL,
  auth_type text NOT NULL DEFAULT 'none' CHECK (auth_type IN ('none', 'bearer', 'oauth', 'header')),
  auth_credentials_encrypted bytea,
  key_version text NOT NULL DEFAULT '',
  risk_level text NOT NULL DEFAULT 'low' CHECK (risk_level IN ('low', 'medium', 'high')),
  status text NOT NULL DEFAULT 'enabled' CHECK (status IN ('enabled', 'disabled')),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE agents (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name text NOT NULL,
  description text NOT NULL DEFAULT '',
  mode text NOT NULL CHECK (mode IN ('react', 'plan', 'super')),
  model_policy_id uuid,
  model_policy_version integer,
  version integer NOT NULL DEFAULT 1 CHECK (version > 0),
  config_snapshot jsonb NOT NULL DEFAULT '{}'::jsonb,
  status text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'published', 'disabled')),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, name, version),
  UNIQUE (id, user_id),
  FOREIGN KEY (model_policy_id, model_policy_version) REFERENCES model_policies(id, version)
);

CREATE TABLE agent_skills (
  agent_id uuid NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
  skill_id uuid NOT NULL REFERENCES skills(id),
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  config jsonb NOT NULL DEFAULT '{}'::jsonb,
  PRIMARY KEY (agent_id, skill_id)
);

CREATE TABLE agent_mcps (
  agent_id uuid NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
  mcp_id uuid NOT NULL REFERENCES mcps(id),
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  config jsonb NOT NULL DEFAULT '{}'::jsonb,
  PRIMARY KEY (agent_id, mcp_id)
);

CREATE TABLE sessions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  agent_id uuid REFERENCES agents(id),
  title text NOT NULL DEFAULT '',
  source text NOT NULL DEFAULT 'web' CHECK (source IN ('web', 'feishu', 'api')),
  status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'archived', 'deleted')),
  version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
  last_message_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (id, user_id)
);

CREATE TABLE session_members (
  session_id uuid NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  member_role text NOT NULL DEFAULT 'member' CHECK (member_role IN ('owner', 'member')),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (session_id, user_id)
);

CREATE TABLE messages (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  session_id uuid NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role text NOT NULL CHECK (role IN ('system', 'user', 'assistant', 'tool')),
  content text NOT NULL,
  tool_calls jsonb NOT NULL DEFAULT '[]'::jsonb,
  token_count integer NOT NULL DEFAULT 0 CHECK (token_count >= 0),
  external_message_id text NOT NULL DEFAULT '',
  speaker_open_id text NOT NULL DEFAULT '',
  edited_at timestamptz,
  deleted_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  CHECK (octet_length(content) <= 4194304),
  CHECK (jsonb_typeof(tool_calls) = 'array')
);

CREATE INDEX messages_session_cursor_idx ON messages (session_id, created_at, id);

CREATE TABLE agent_runs (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  session_id uuid NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  agent_id uuid REFERENCES agents(id),
  agent_version integer,
  mode text NOT NULL CHECK (mode IN ('react', 'plan', 'super')),
  status text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'running', 'waiting_approval', 'completed', 'failed', 'cancelled', 'reconciliation_required')),
  config_snapshot jsonb NOT NULL DEFAULT '{}'::jsonb,
  model_policy_snapshot jsonb NOT NULL DEFAULT '{}'::jsonb,
  current_step_seq bigint NOT NULL DEFAULT 0,
  attempt_no integer NOT NULL DEFAULT 0 CHECK (attempt_no >= 0),
  idempotency_key text,
  error_code text NOT NULL DEFAULT '',
  error_message text NOT NULL DEFAULT '',
  deadline_at timestamptz,
  started_at timestamptz,
  finished_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (id, user_id),
  UNIQUE (user_id, idempotency_key)
);

CREATE INDEX agent_runs_session_cursor_idx ON agent_runs (session_id, created_at, id);
CREATE INDEX agent_runs_active_idx ON agent_runs (status, updated_at) WHERE finished_at IS NULL;

CREATE TABLE execution_steps (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  run_id uuid NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  step_seq bigint NOT NULL,
  parent_step_id uuid REFERENCES execution_steps(id),
  kind text NOT NULL CHECK (kind IN ('model', 'tool', 'plan', 'worker', 'system')),
  name text NOT NULL DEFAULT '',
  status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'running', 'waiting_approval', 'completed', 'failed', 'cancelled', 'reconciliation_required')),
  tool_name text NOT NULL DEFAULT '',
  schema_version integer NOT NULL DEFAULT 1,
  risk_level text NOT NULL DEFAULT 'low' CHECK (risk_level IN ('low', 'medium', 'high')),
  attempt_no integer NOT NULL DEFAULT 1 CHECK (attempt_no > 0),
  idempotency_key text NOT NULL DEFAULT '',
  error_code text NOT NULL DEFAULT '',
  started_at timestamptz,
  finished_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (run_id, step_seq),
  UNIQUE (id, user_id)
);

CREATE TABLE execution_payloads (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  run_id uuid NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
  step_id uuid REFERENCES execution_steps(id) ON DELETE CASCADE,
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  payload_type text NOT NULL CHECK (payload_type IN ('model_request', 'model_response', 'tool_input', 'tool_output', 'tool_error', 'external_request', 'external_response')),
  content_json jsonb,
  content_text text,
  content_type text NOT NULL DEFAULT 'application/json',
  capture_mode text NOT NULL CHECK (capture_mode IN ('metadata_only', 'redacted')),
  schema_version integer NOT NULL DEFAULT 1,
  redaction_version integer NOT NULL DEFAULT 1,
  original_size_bytes bigint NOT NULL DEFAULT 0 CHECK (original_size_bytes >= 0),
  saved_size_bytes bigint NOT NULL DEFAULT 0 CHECK (saved_size_bytes >= 0),
  truncated boolean NOT NULL DEFAULT false,
  partial boolean NOT NULL DEFAULT false,
  capture_error text NOT NULL DEFAULT '',
  payload_hash bytea,
  created_at timestamptz NOT NULL DEFAULT now(),
  CHECK (content_json IS NULL OR content_text IS NULL),
  CHECK (payload_hash IS NULL OR octet_length(payload_hash) = 32)
);

CREATE INDEX execution_payloads_run_step_idx ON execution_payloads (run_id, step_id, created_at);

CREATE TABLE run_events (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  run_id uuid NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  seq bigint NOT NULL,
  event_id uuid NOT NULL UNIQUE,
  event_type text NOT NULL,
  schema_version integer NOT NULL DEFAULT 1,
  payload jsonb NOT NULL DEFAULT '{}'::jsonb,
  occurred_at timestamptz NOT NULL,
  UNIQUE (run_id, seq)
);

CREATE INDEX run_events_run_cursor_idx ON run_events (run_id, seq);

CREATE TABLE model_calls (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  run_id uuid NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
  step_id uuid REFERENCES execution_steps(id) ON DELETE CASCADE,
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  attempt_no integer NOT NULL CHECK (attempt_no > 0),
  provider_id uuid REFERENCES llm_providers(id),
  model_id uuid REFERENCES llm_models(id),
  policy_version integer,
  request_manifest jsonb NOT NULL DEFAULT '{}'::jsonb,
  request_payload_id uuid REFERENCES execution_payloads(id),
  response_payload_id uuid REFERENCES execution_payloads(id),
  provider_request_id text NOT NULL DEFAULT '',
  status text NOT NULL DEFAULT 'running' CHECK (status IN ('running', 'succeeded', 'failed', 'cancelled')),
  request_hash bytea,
  response_hash bytea,
  ttft_ms bigint,
  duration_ms bigint,
  input_tokens bigint NOT NULL DEFAULT 0,
  output_tokens bigint NOT NULL DEFAULT 0,
  estimated_cost numeric(20, 8) NOT NULL DEFAULT 0,
  finish_reason text NOT NULL DEFAULT '',
  switch_reason text NOT NULL DEFAULT '',
  error_class text NOT NULL DEFAULT '',
  error_message text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  completed_at timestamptz,
  UNIQUE (run_id, attempt_no)
);

-- plans 与 plan_steps 由 000016 负责建表；此处不再重复创建，避免全新数据库升级失败。

DO $rls$
DECLARE
  table_name text;
BEGIN
  FOR table_name IN SELECT unnest(ARRAY[
    'agents', 'agent_skills', 'agent_mcps', 'sessions', 'session_members', 'messages',
    'agent_runs', 'execution_steps', 'execution_payloads', 'run_events', 'model_calls'
  ]) LOOP
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', table_name);
    EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', table_name);
    EXECUTE format(
      'CREATE POLICY user_isolation ON %I FOR ALL TO harness_app USING (user_id = nullif(current_setting(''app.user_id'', true), '''')::uuid) WITH CHECK (user_id = nullif(current_setting(''app.user_id'', true), '''')::uuid)',
      table_name
    );
  END LOOP;
END
$rls$;

GRANT SELECT ON llm_providers, llm_models, model_policies, model_policy_candidates, skills, mcps TO harness_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON llm_providers, llm_models, model_policies, model_policy_candidates, skills, mcps TO harness_admin;
GRANT SELECT, INSERT, UPDATE, DELETE ON agents, agent_skills, agent_mcps, sessions, session_members, messages,
  agent_runs, execution_steps, execution_payloads, run_events, model_calls TO harness_app;
