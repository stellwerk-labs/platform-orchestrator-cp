-- +goose Up
ALTER TABLE resource_types ADD COLUMN module_contract JSONB;
ALTER TABLE definition_versions ADD COLUMN output_schema JSONB;

-- +goose Down
-- Application rollback should retain these additive columns. A schema downgrade
-- after publication requires exporting the immutable declaration metadata first.
ALTER TABLE definition_versions DROP COLUMN output_schema;
ALTER TABLE resource_types DROP COLUMN module_contract;
