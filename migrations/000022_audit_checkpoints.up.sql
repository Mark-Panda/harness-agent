-- 说明：审计检查点用于在不全量扫描分区的情况下验证 Hash Chain 锚点。
-- 关键步骤：检查点只允许插入，更新时间与删除走与 audit_logs 一致的保留期特权开关。
CREATE TABLE audit_checkpoints (
  partition_id text NOT NULL,
  partition_seq bigint NOT NULL,
  event_hash bytea NOT NULL,
  signature bytea NOT NULL,
  signature_version integer NOT NULL DEFAULT 1,
  created_at timestamptz NOT NULL,
  PRIMARY KEY (partition_id, partition_seq),
  CHECK (octet_length(event_hash) = 32),
  CHECK (octet_length(signature) = 32)
);

CREATE OR REPLACE FUNCTION prevent_audit_checkpoint_mutation() RETURNS trigger AS $function$
BEGIN
  IF TG_OP = 'UPDATE' THEN
    RAISE EXCEPTION 'audit checkpoints are append-only';
  END IF;
  IF nullif(current_setting('audit.retention', true), '') IS DISTINCT FROM 'on' THEN
    RAISE EXCEPTION 'audit checkpoint deletion requires the retention role';
  END IF;
  RETURN OLD;
END;
$function$ LANGUAGE plpgsql;

CREATE TRIGGER audit_checkpoints_append_only
  BEFORE UPDATE OR DELETE ON audit_checkpoints
  FOR EACH ROW EXECUTE FUNCTION prevent_audit_checkpoint_mutation();

GRANT SELECT, INSERT ON audit_checkpoints TO harness_app;
GRANT SELECT, INSERT ON audit_checkpoints TO harness_auth;
