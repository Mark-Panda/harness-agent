CREATE TABLE session_summaries (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  session_id uuid NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  version bigint NOT NULL CHECK (version > 0),
  summary_text text NOT NULL,
  source_message_ids uuid[] NOT NULL DEFAULT '{}',
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (session_id, version),
  CHECK (octet_length(summary_text) <= 1048576),
  CHECK (cardinality(source_message_ids) <= 10000)
);

CREATE TABLE memory_vectors (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  session_id uuid REFERENCES sessions(id) ON DELETE CASCADE,
  content text NOT NULL,
  embedding vector(1536) NOT NULL,
  metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
  embedding_provider text NOT NULL,
  embedding_model text NOT NULL,
  embedding_dimension integer NOT NULL CHECK (embedding_dimension = 1536),
  embedding_version text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  deleted_at timestamptz
);

CREATE INDEX memory_vectors_user_created_idx ON memory_vectors (user_id, created_at, id) WHERE deleted_at IS NULL;
CREATE INDEX memory_vectors_embedding_hnsw ON memory_vectors USING hnsw (embedding vector_cosine_ops);

CREATE TABLE memory_facts (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  session_id uuid REFERENCES sessions(id) ON DELETE CASCADE,
  fact_key text NOT NULL,
  fact_value jsonb NOT NULL,
  confidence numeric(4, 3) NOT NULL CHECK (confidence >= 0 AND confidence <= 1),
  source_message_id uuid REFERENCES messages(id),
  valid_from timestamptz NOT NULL DEFAULT now(),
  valid_to timestamptz,
  version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, fact_key, version),
  CHECK (valid_to IS NULL OR valid_to > valid_from)
);

CREATE TABLE external_conversation_bindings (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  provider text NOT NULL CHECK (provider = 'feishu'),
  tenant_key text NOT NULL,
  chat_id text NOT NULL,
  thread_id text,
  root_message_id text,
  session_id uuid NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  agent_id uuid REFERENCES agents(id),
  agent_version integer,
  status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'closed', 'archived')),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX external_binding_thread_key ON external_conversation_bindings (provider, tenant_key, chat_id, thread_id) WHERE thread_id IS NOT NULL;
CREATE UNIQUE INDEX external_binding_root_key ON external_conversation_bindings (provider, tenant_key, chat_id, root_message_id) WHERE root_message_id IS NOT NULL;
CREATE INDEX external_binding_session_idx ON external_conversation_bindings (session_id);

CREATE TABLE feishu_event_receipts (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  event_id text NOT NULL UNIQUE,
  event_type text NOT NULL,
  tenant_key text NOT NULL DEFAULT '',
  payload_redacted jsonb NOT NULL DEFAULT '{}'::jsonb,
  status text NOT NULL DEFAULT 'received' CHECK (status IN ('received', 'processed', 'failed')),
  last_error text NOT NULL DEFAULT '',
  received_at timestamptz NOT NULL DEFAULT now(),
  processed_at timestamptz
);

CREATE TABLE jobs (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id uuid REFERENCES users(id) ON DELETE CASCADE,
  job_type text NOT NULL,
  dedupe_key text,
  payload jsonb NOT NULL DEFAULT '{}'::jsonb,
  status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'running', 'completed', 'failed', 'dead')),
  attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  max_attempts integer NOT NULL DEFAULT 8 CHECK (max_attempts > 0),
  available_at timestamptz NOT NULL DEFAULT now(),
  lease_owner text NOT NULL DEFAULT '',
  lease_expires_at timestamptz,
  fencing_token bigint NOT NULL DEFAULT 0 CHECK (fencing_token >= 0),
  last_error text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  completed_at timestamptz,
  CHECK (attempts <= max_attempts)
);

CREATE UNIQUE INDEX jobs_dedupe_key ON jobs (dedupe_key) WHERE dedupe_key IS NOT NULL;
CREATE INDEX jobs_claim_idx ON jobs (status, available_at, lease_expires_at);

CREATE TABLE outbox_events (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id uuid REFERENCES users(id) ON DELETE CASCADE,
  event_type text NOT NULL,
  aggregate_type text NOT NULL,
  aggregate_id text NOT NULL,
  dedupe_key text NOT NULL UNIQUE,
  payload jsonb NOT NULL DEFAULT '{}'::jsonb,
  status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'published', 'failed')),
  attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  available_at timestamptz NOT NULL DEFAULT now(),
  lease_owner text NOT NULL DEFAULT '',
  lease_expires_at timestamptz,
  last_error text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  published_at timestamptz
);

CREATE INDEX outbox_events_pending_idx ON outbox_events (status, available_at, created_at);

CREATE TABLE idempotency_keys (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  key text NOT NULL,
  request_hash bytea NOT NULL,
  response_json jsonb,
  status text NOT NULL DEFAULT 'in_progress' CHECK (status IN ('in_progress', 'completed', 'failed')),
  resource_type text NOT NULL DEFAULT '',
  resource_id text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL,
  UNIQUE (user_id, key),
  CHECK (octet_length(request_hash) = 32),
  CHECK (expires_at > created_at)
);

CREATE TABLE deletion_requests (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  resource_type text NOT NULL CHECK (resource_type IN ('session', 'memory', 'account')),
  resource_id text NOT NULL,
  status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'running', 'completed', 'failed')),
  requested_by text NOT NULL,
  last_error text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  completed_at timestamptz
);

ALTER TABLE jobs ENABLE ROW LEVEL SECURITY;
ALTER TABLE jobs FORCE ROW LEVEL SECURITY;
CREATE POLICY jobs_user_or_system ON jobs FOR ALL TO harness_app
  USING (user_id = nullif(current_setting('app.user_id', true), '')::uuid OR nullif(current_setting('app.system', true), '') = 'on')
  WITH CHECK (user_id = nullif(current_setting('app.user_id', true), '')::uuid OR nullif(current_setting('app.system', true), '') = 'on');

ALTER TABLE outbox_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox_events FORCE ROW LEVEL SECURITY;
CREATE POLICY outbox_user_or_system ON outbox_events FOR ALL TO harness_app
  USING (user_id = nullif(current_setting('app.user_id', true), '')::uuid OR nullif(current_setting('app.system', true), '') = 'on')
  WITH CHECK (user_id = nullif(current_setting('app.user_id', true), '')::uuid OR nullif(current_setting('app.system', true), '') = 'on');
CREATE POLICY outbox_auth_user_or_system ON outbox_events FOR ALL TO harness_auth
  USING (user_id = nullif(current_setting('app.user_id', true), '')::uuid OR nullif(current_setting('app.system', true), '') = 'on')
  WITH CHECK (user_id = nullif(current_setting('app.user_id', true), '')::uuid OR nullif(current_setting('app.system', true), '') = 'on');

DO $rls$
DECLARE
  table_name text;
BEGIN
  FOR table_name IN SELECT unnest(ARRAY[
    'session_summaries', 'memory_vectors', 'memory_facts', 'external_conversation_bindings', 'idempotency_keys', 'deletion_requests'
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

GRANT SELECT, INSERT, UPDATE, DELETE ON session_summaries, memory_vectors, memory_facts,
  external_conversation_bindings, idempotency_keys, deletion_requests TO harness_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON jobs TO harness_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON outbox_events TO harness_app, harness_auth;
