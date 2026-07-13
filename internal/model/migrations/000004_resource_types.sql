-- +goose Up

CREATE TABLE resource_types (
    uuid UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id TEXT,
    id TEXT NOT NULL,
    name TEXT NOT NULL,
    output_schema jsonb NOT NULL,
    created_at TIMESTAMP WITHOUT TIME ZONE NOT NULL,
    is_developer_accessible BOOLEAN NOT NULL DEFAULT TRUE,
    CONSTRAINT resource_types_unq UNIQUE NULLS NOT DISTINCT (org_id, id),
    CONSTRAINT resource_types_org_fk FOREIGN KEY (org_id) REFERENCES orgs(id)
);

CREATE INDEX org_ids_idx ON resource_types(org_id);
CREATE INDEX ids_idx ON resource_types(id);

-- +goose Down

DROP TABLE IF EXISTS resource_types;
