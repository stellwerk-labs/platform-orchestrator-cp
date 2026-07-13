-- +goose Up

CREATE TABLE runner_rules (
    org_id TEXT NOT NULL REFERENCES orgs (id),
    id UUID NOT NULL DEFAULT gen_random_uuid(),
    runner_id TEXT NOT NULL,
    created_at TIMESTAMP WITHOUT TIME ZONE NOT NULL,
    project_id TEXT,
    env_type_id TEXT,

    PRIMARY KEY (org_id, id),
    FOREIGN KEY (org_id, runner_id) REFERENCES runners (org_id, id) ON DELETE CASCADE,
    FOREIGN KEY (org_id, env_type_id) REFERENCES env_types (org_id, id) ON DELETE CASCADE,
    FOREIGN KEY (org_id, project_id) REFERENCES projects (org_id, id) ON DELETE CASCADE
);

-- The default sort order, only indexes on the non-null columns to make the query easier
CREATE INDEX ON runner_rules(org_id, runner_id, project_id, env_type_id, id);
CREATE INDEX ON runner_rules(org_id, project_id, env_type_id);
CREATE INDEX ON runner_rules(org_id, env_type_id);

-- +goose Down

DROP TABLE IF EXISTS runner_rules;
