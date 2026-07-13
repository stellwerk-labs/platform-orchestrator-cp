-- +goose Up
ALTER TABLE orgs ADD COLUMN IF NOT EXISTS source TEXT CHECK (source IN ('internal', 'public')) NOT NULL DEFAULT 'internal';
-- +goose Down

ALTER TABLE orgs DROP COLUMN IF EXISTS source;

