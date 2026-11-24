-- +goose Up
ALTER TABLE runners
    ADD COLUMN uuid UUID NOT NULL DEFAULT gen_random_uuid(),
    ADD CONSTRAINT runners_uuid_key UNIQUE (uuid);

-- +goose Down
ALTER TABLE runners DROP COLUMN uuid;
