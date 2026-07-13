-- +goose Up

CREATE TABLE orgs (
  id TEXT PRIMARY KEY NOT NULL,
  uuid UUID UNIQUE NOT NULL DEFAULT gen_random_uuid(),
  created_at TIMESTAMP WITHOUT TIME ZONE NOT NULL
);

-- +goose Down

DROP TABLE IF EXISTS orgs;
