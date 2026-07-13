-- +goose Up

CREATE TABLE definition_rules (
    org_id TEXT NOT NULL REFERENCES orgs (id),
    id UUID NOT NULL DEFAULT gen_random_uuid(),
    definition_id TEXT NOT NULL,
    created_at TIMESTAMP WITHOUT TIME ZONE NOT NULL,
    -- denormalized resource type to make queries easier and more direct
    resource_type TEXT NOT NULL,
    resource_class TEXT NOT NULL,
    resource_id TEXT,

    project_id TEXT,
    env_type_id TEXT CHECK ( (env_type_id IS NULL) OR (env_id IS NULL) ),
    env_id TEXT CHECK ( (env_id IS NULL) OR (env_type_id IS NULL AND project_id IS NOT NULL) ),

    PRIMARY KEY (org_id, id),
    FOREIGN KEY (org_id, definition_id) REFERENCES definitions (org_id, id) ON DELETE CASCADE,
    FOREIGN KEY (org_id, env_type_id) REFERENCES env_types (org_id, id) ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id) REFERENCES projects (org_id, id) ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id, env_id) REFERENCES envs (org_id, project_id, id) ON DELETE CASCADE
);

-- The default sort order, only indexes on the non-null columns to make the query easier
CREATE INDEX ON definition_rules(org_id, resource_type, definition_id, resource_class, id);
CREATE INDEX ON definition_rules(org_id, project_id, env_id);
CREATE INDEX ON definition_rules(org_id, env_type_id);

-- +goose Down

DROP TABLE IF EXISTS definition_rules;
