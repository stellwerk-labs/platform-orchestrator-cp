-- +goose Up

ALTER TABLE definition_rules DROP CONSTRAINT IF EXISTS definition_rules_org_id_project_id_fkey;
ALTER TABLE definition_rules DROP CONSTRAINT IF EXISTS definition_rules_org_id_project_id_env_id_fkey;

ALTER TABLE runner_rules DROP CONSTRAINT IF EXISTS runner_rules_org_id_project_id_fkey;

-- +goose Down

