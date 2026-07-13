-- +goose Up

ALTER TABLE orgs ADD COLUMN updated_at TIMESTAMP WITHOUT TIME ZONE DEFAULT NULL;
ALTER TABLE orgs ADD COLUMN status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'deleting', 'delete_failed', 'deleted'));
ALTER TABLE orgs ADD COLUMN status_message TEXT;

UPDATE orgs SET updated_at = deleted_at WHERE deleted_at IS NOT NULL;
UPDATE orgs SET status = 'deleted' WHERE deleted_at IS NOT NULL;

ALTER TABLE orgs DROP COLUMN deleted_at;

-- +goose Down

ALTER TABLE orgs ADD COLUMN deleted_at TIMESTAMP WITHOUT TIME ZONE DEFAULT NULL;

UPDATE orgs SET deleted_at = updated_at WHERE status = 'deleted';

ALTER TABLE orgs DROP COLUMN status_message;
ALTER TABLE orgs DROP COLUMN status;
ALTER TABLE orgs DROP COLUMN updated_at;
