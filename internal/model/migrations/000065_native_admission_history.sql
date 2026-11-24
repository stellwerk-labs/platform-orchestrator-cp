-- +goose Up
CREATE INDEX external_operation_dispatches_settled_environment
    ON external_operation_dispatches (org_id,(intent->>'environment_uuid')) WHERE state='settled';
CREATE INDEX deployment_admission_external_history
    ON deployment_admission_guards (org_id,(intent->'external_dispatch'->>'plugin_id'),(intent->'external_dispatch'->>'operation_id'),(intent->'external_dispatch'->>'dispatch_id'))
    WHERE outcome IS DISTINCT FROM 'absent' AND intent->'external_dispatch' IS NOT NULL;

-- +goose Down
DROP INDEX deployment_admission_external_history;
DROP INDEX external_operation_dispatches_settled_environment;
