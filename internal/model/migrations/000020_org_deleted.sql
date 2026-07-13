-- +goose Up

ALTER TABLE orgs ADD COLUMN deleted_at TIMESTAMP WITHOUT TIME ZONE DEFAULT NULL;

-- +goose Down

ALTER TABLE orgs DROP COLUMN deleted_at;
