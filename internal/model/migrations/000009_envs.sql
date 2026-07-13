-- +goose Up

CREATE TABLE env_types (
    org_id TEXT NOT NULL REFERENCES orgs (id),
    org_uuid UUID NOT NULL REFERENCES orgs (uuid),
    id TEXT NOT NULL,
    uuid UUID UNIQUE NOT NULL DEFAULT gen_random_uuid(),
    display_name TEXT NOT NULL,
    created_at TIMESTAMP WITHOUT TIME ZONE NOT NULL,
    PRIMARY KEY (org_id, id)
);

CREATE TABLE envs (
  id TEXT NOT NULL,
  uuid UUID UNIQUE NOT NULL DEFAULT gen_random_uuid(),
  created_at TIMESTAMP WITHOUT TIME ZONE NOT NULL,
  updated_at TIMESTAMP WITHOUT TIME ZONE NOT NULL,
  display_name TEXT NOT NULL,
  org_id TEXT NOT NULL REFERENCES orgs (id),
  org_uuid UUID NOT NULL REFERENCES orgs (uuid),
  project_id TEXT NOT NULL,
  project_uuid UUID NOT NULL REFERENCES projects (uuid),
  env_type_id TEXT NOT NULL,
  env_type_uuid UUID NOT NULL REFERENCES env_types (uuid),
  runner_id TEXT NOT NULL,
  status TEXT NOT NULL,
  status_message TEXT,

  PRIMARY KEY (org_id, project_id, id),
    -- Deleting a project should be blocked if there are envs in it
  FOREIGN KEY (org_id, project_id) REFERENCES projects (org_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (org_id, runner_id) REFERENCES runners (org_id, id) ON DELETE RESTRICT,

  -- Deleting an env type should be blocked if there are envs that use this type
  FOREIGN KEY (org_id, env_type_id) REFERENCES env_types (org_id, id) ON DELETE RESTRICT
);

CREATE INDEX ON envs (org_id, env_type_id);
CREATE INDEX ON envs (org_id, runner_id);
CREATE INDEX ON envs (status);

-- +goose Down

DROP TABLE IF EXISTS envs;
DROP TABLE IF EXISTS env_types;
