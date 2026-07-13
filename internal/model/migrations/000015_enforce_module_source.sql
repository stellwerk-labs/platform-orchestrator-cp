-- +goose Up

UPDATE definition_versions SET module_source = 'inline' WHERE module_source IS NULL;
ALTER TABLE definition_versions ALTER COLUMN module_source SET NOT NULL;

-- +goose Down

ALTER TABLE definition_versions ALTER COLUMN module_source SET NULL;
UPDATE definition_versions SET module_source = NULL WHERE module_source = 'inline';
