-- +goose Up

CREATE TABLE definitions (
  org_id TEXT NOT NULL REFERENCES orgs (id),
  id TEXT NOT NULL,
  created_at TIMESTAMP WITHOUT TIME ZONE NOT NULL,
  resource_type TEXT NOT NULL,
  latest_version_id TEXT NOT NULL,
  PRIMARY KEY (org_id, id)
);

CREATE TABLE definition_versions (
  org_id TEXT NOT NULL REFERENCES orgs (id),
  definition_id TEXT NOT NULL,
  version_id TEXT NOT NULL,
  created_at TIMESTAMP WITHOUT TIME ZONE NOT NULL,
  description TEXT,
  module_source TEXT,
  module_source_code TEXT,
  module_inputs JSONB NOT NULL,
  dependencies JSONB NOT NULL,
  coprovisioned JSONB NOT NULL,
  provider_mapping JSONB NOT NULL,
  provider_values TEXT[] NOT NULL,
  PRIMARY KEY (org_id, definition_id, version_id),
  FOREIGN KEY (org_id, definition_id) REFERENCES definitions (org_id, id) ON DELETE CASCADE
);

ALTER TABLE definitions ADD CONSTRAINT definitions_latest_version_fk FOREIGN KEY (org_id, id, latest_version_id) REFERENCES definition_versions (org_id, definition_id, version_id) INITIALLY DEFERRED;

-- must be able to easily sort definitions by updated time
CREATE INDEX ON definition_versions(created_at);
CREATE INDEX ON definition_versions(org_id, definition_id, version_id, provider_values);

-- +goose Down

DROP TABLE IF EXISTS definition_version_prov;
ALTER TABLE definitions DROP CONSTRAINT definitions_latest_version_fk;
DROP TABLE IF EXISTS definition_versions;
DROP TABLE IF EXISTS definitions;
