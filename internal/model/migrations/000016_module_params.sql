-- +goose Up

ALTER TABLE definition_versions ADD COLUMN module_params jsonb NOT NULL default '{}'::jsonb;

-- +goose Down

ALTER TABLE definition_versions DROP COLUMN IF EXISTS module_params;
