-- +goose Up

CREATE TABLE projects (
  id TEXT NOT NULL,
  uuid UUID UNIQUE NOT NULL DEFAULT gen_random_uuid(),
  created_at TIMESTAMP WITHOUT TIME ZONE NOT NULL,
  updated_at TIMESTAMP WITHOUT TIME ZONE NOT NULL,
  display_name TEXT NOT NULL,
  org_id TEXT NOT NULL REFERENCES orgs (id),
  org_uuid UUID NOT NULL REFERENCES orgs (uuid),
  status TEXT NOT NULL,
  PRIMARY KEY (org_id, id)
);

CREATE INDEX ON projects (status);

-- +goose Down

DROP TABLE IF EXISTS projects;
