-- +goose Up

CREATE TABLE providers (
    org_id TEXT NOT NULL REFERENCES orgs (id),
    provider_type TEXT NOT NULL,
    id TEXT NOT NULL,
    description TEXT,
    created_at TIMESTAMP WITHOUT TIME ZONE NOT NULL,
    source TEXT NOT NULL,
    version_constraint TEXT,
    configuration JSONB NOT NULL,
    PRIMARY KEY (org_id, provider_type, id)
);

-- +goose Down

DROP TABLE IF EXISTS providers;
