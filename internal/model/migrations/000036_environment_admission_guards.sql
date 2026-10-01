-- +goose Up
CREATE TABLE environment_admission_epochs (
    org_id TEXT NOT NULL REFERENCES orgs(id) ON DELETE RESTRICT,
    environment_uuid UUID NOT NULL,
    epoch BIGINT NOT NULL CHECK (epoch > 0),
    PRIMARY KEY (org_id, environment_uuid)
);
CREATE TABLE deployment_admission_guards (
    org_id TEXT NOT NULL REFERENCES orgs(id) ON DELETE RESTRICT,
    admission_id UUID NOT NULL,
    environment_uuid UUID NOT NULL,
    project_uuid UUID NOT NULL,
    intent JSONB NOT NULL,
    epoch BIGINT NOT NULL CHECK (epoch > 0),
    outcome TEXT CHECK (outcome IN ('succeeded', 'failed', 'cancelled', 'terminated', 'absent')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    settled_at TIMESTAMPTZ,
    PRIMARY KEY (org_id, admission_id)
);
CREATE UNIQUE INDEX deployment_admission_guards_active_environment
    ON deployment_admission_guards(org_id, environment_uuid) WHERE outcome IS NULL;

-- +goose Down
-- Application rollback keeps the additive schema. Explicit metadata downgrade
-- cannot erase uncertain ordinary or external admission protection.
-- +goose StatementBegin
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM deployment_admission_guards WHERE outcome IS NULL)
       OR EXISTS (SELECT 1 FROM external_operation_dispatches WHERE state <> 'settled') THEN
        RAISE EXCEPTION 'cannot remove unresolved Environment admission protection';
    END IF;
END $$;
-- +goose StatementEnd
DROP TABLE deployment_admission_guards;
DROP TABLE environment_admission_epochs;
