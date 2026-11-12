-- +goose Up
ALTER TABLE providers ADD COLUMN deletion_requested_at timestamptz, ADD COLUMN retirement_checked_at timestamptz;
CREATE INDEX providers_pending_deletion ON providers (org_id, deletion_requested_at, provider_type, id)
    WHERE deletion_requested_at IS NOT NULL;
-- Migrated Environments require DP evidence. New Environments are explicitly
-- marked empty by CreateEnvironment until their first admitted state writer.
ALTER TABLE envs ADD COLUMN provider_state_required boolean NOT NULL DEFAULT true;
CREATE TABLE provider_lifecycle_epochs (
    org_id text PRIMARY KEY REFERENCES orgs(id) ON DELETE CASCADE,
    epoch bigint NOT NULL DEFAULT 0 CHECK (epoch >= 0)
);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM providers WHERE deletion_requested_at IS NOT NULL) THEN
        RAISE EXCEPTION 'cannot discard pending Provider deletion requests';
    END IF;
END $$;
-- +goose StatementEnd
DROP TABLE provider_lifecycle_epochs;
ALTER TABLE envs DROP COLUMN provider_state_required;
DROP INDEX providers_pending_deletion;
ALTER TABLE providers DROP COLUMN deletion_requested_at, DROP COLUMN retirement_checked_at;
