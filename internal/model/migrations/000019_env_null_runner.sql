-- +goose Up

ALTER TABLE envs ALTER COLUMN runner_id DROP NOT NULL;

-- +goose Down

ALTER TABLE envs ALTER COLUMN runner_id SET NOT NULL;
