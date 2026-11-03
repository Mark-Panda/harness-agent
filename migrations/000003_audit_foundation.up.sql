CREATE TABLE audit_logs (
  partition_id text NOT NULL,
  partition_seq bigint NOT NULL,
  event_id uuid NOT NULL,
  schema_version integer NOT NULL DEFAULT 1,
  occurred_at timestamptz NOT NULL,
  actor_type text NOT NULL CHECK (actor_type IN ('user', 'admin', 'agent', 'worker', 'system', 'anonymous')),
  actor_ref text NOT NULL DEFAULT '',
  on_behalf_user_ref text NOT NULL DEFAULT '',
  action text NOT NULL,
  resource_type text NOT NULL,
  resource_id text NOT NULL,
  outcome text NOT NULL CHECK (outcome IN ('success', 'failure', 'denied')),
  reason text NOT NULL DEFAULT '',
  request_id text NOT NULL DEFAULT '',
  trace_id text NOT NULL DEFAULT '',
  run_id uuid,
  source text NOT NULL,
  metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
  prev_event_hash bytea,
  event_hash bytea NOT NULL,
  signature_version integer NOT NULL DEFAULT 1,
  PRIMARY KEY (partition_id, partition_seq),
  UNIQUE (event_id),
  CHECK (octet_length(event_hash) = 32),
  CHECK (prev_event_hash IS NULL OR octet_length(prev_event_hash) = 32),
  CHECK (jsonb_typeof(metadata) = 'object'),
  CHECK (pg_column_size(metadata) <= 16384),
  CHECK (length(reason) <= 512)
);

CREATE INDEX audit_logs_resource_idx ON audit_logs (resource_type, resource_id, occurred_at);
CREATE INDEX audit_logs_actor_idx ON audit_logs (actor_ref, occurred_at);

CREATE OR REPLACE FUNCTION prevent_audit_mutation() RETURNS trigger AS $function$
BEGIN
  IF TG_OP = 'UPDATE' THEN
    RAISE EXCEPTION 'audit logs are append-only';
  END IF;
  IF nullif(current_setting('audit.retention', true), '') IS DISTINCT FROM 'on' THEN
    RAISE EXCEPTION 'audit deletion requires the retention role';
  END IF;
  RETURN OLD;
END;
$function$ LANGUAGE plpgsql;

CREATE TRIGGER audit_logs_append_only
  BEFORE UPDATE OR DELETE ON audit_logs
  FOR EACH ROW EXECUTE FUNCTION prevent_audit_mutation();

GRANT SELECT, INSERT ON audit_logs TO harness_app;
GRANT SELECT, INSERT ON audit_logs TO harness_auth;
