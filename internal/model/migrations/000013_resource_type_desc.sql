-- +goose Up

ALTER TABLE resource_types
RENAME COLUMN name to description;

-- +goose Down

ALTER TABLE resource_types
RENAME COLUMN description to name;
