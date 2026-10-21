-- +goose Up
ALTER TABLE external_operation_dispatches
    ADD COLUMN state TEXT NOT NULL DEFAULT 'active'
        CHECK (state IN ('active', 'withdrawn', 'settled')),
    ADD COLUMN settled_at TIMESTAMPTZ,
    ADD COLUMN withdrawn_at TIMESTAMPTZ,
    ADD COLUMN completion JSONB;
CREATE INDEX external_operation_dispatches_pending_environment
    ON external_operation_dispatches(org_id, (intent->>'environment_uuid'))
    WHERE state <> 'settled';

-- +goose Down
-- Application rollback keeps the additive schema. Explicit schema downgrade
-- must not discard unresolved dispatch protection.
-- +goose StatementBegin
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM external_operation_dispatches WHERE state <> 'settled') THEN
        RAISE EXCEPTION 'cannot remove unresolved external dispatch barriers';
    END IF;
END $$;
-- +goose StatementEnd
DROP INDEX external_operation_dispatches_pending_environment;
ALTER TABLE external_operation_dispatches DROP COLUMN completion, DROP COLUMN withdrawn_at, DROP COLUMN settled_at, DROP COLUMN state;
