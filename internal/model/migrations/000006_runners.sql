-- +goose Up

CREATE TABLE runners (
    org_id TEXT NOT NULL REFERENCES orgs (id),
    id TEXT NOT NULL,
    created_at TIMESTAMP WITHOUT TIME ZONE NOT NULL,
    updated_at TIMESTAMP WITHOUT TIME ZONE NOT NULL,
    description TEXT,
    "type" TEXT NOT NULL,
    "configuration" JSONB NOT NULL,
    "config_secret" JSONB,
    state_type TEXT NOT NULL,
    state_configuration JSONB NOT NULL,
    PRIMARY KEY (org_id, id)
);

CREATE INDEX runner_types_idx ON runners("type");

-- +goose Down

DROP TABLE IF EXISTS runners;
