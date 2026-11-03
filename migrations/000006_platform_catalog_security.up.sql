DO $platform_tables$
DECLARE
  table_name text;
BEGIN
  FOREACH table_name IN ARRAY ARRAY[
    'llm_providers', 'llm_models', 'model_policies', 'model_policy_candidates', 'skills', 'mcps'
  ]
  LOOP
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', table_name);
    EXECUTE format(
      'CREATE POLICY platform_read ON %I FOR SELECT TO harness_app USING (true)',
      table_name
    );
    EXECUTE format(
      'CREATE POLICY platform_admin_write ON %I FOR ALL TO harness_app USING (current_setting(''app.system'', true) = ''on'') WITH CHECK (current_setting(''app.system'', true) = ''on'')',
      table_name
    );
    EXECUTE format(
      'CREATE POLICY platform_admin_all ON %I FOR ALL TO harness_admin USING (true) WITH CHECK (true)',
      table_name
    );
  END LOOP;
END
$platform_tables$;

GRANT SELECT, INSERT, UPDATE, DELETE ON llm_providers, llm_models, model_policies, model_policy_candidates, skills, mcps TO harness_app;

ALTER TABLE llm_models
  ADD CONSTRAINT llm_models_enabled_verified_check
  CHECK (status <> 'enabled' OR (verified_at IS NOT NULL AND btrim(verification_source) <> ''));

ALTER TABLE llm_models
  ADD CONSTRAINT llm_models_price_nonnegative_check
  CHECK (input_price_per_million >= 0 AND output_price_per_million >= 0);

ALTER TABLE model_policy_candidates
  ADD CONSTRAINT model_policy_candidates_constraints_object_check
  CHECK (jsonb_typeof(constraints) = 'object');

ALTER TABLE model_policies
  ADD CONSTRAINT model_policies_rules_object_check
  CHECK (jsonb_typeof(rules) = 'object');
