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

ALTER ROLE harness_app WITH LOGIN PASSWORD 'harness';
ALTER ROLE harness_auth WITH LOGIN PASSWORD 'harness';
