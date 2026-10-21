-- +goose Up
-- What Core last did to bring IAM's capability catalogue of a plugin in one
-- organization in line with a package: the package whose declarations it
-- registered or confirmed, when, and why it failed. IAM keeps the catalogue
-- per organization and plugin, so this does too. It is a fact Core observed
-- about IAM, not operator state, so it lives outside the Plugin Manager
-- revision and audit, like plugin status observations.
CREATE TABLE plugin_capability_registrations (
    plugin_id TEXT NOT NULL,
    org_id TEXT NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    package_sha256 TEXT NOT NULL,
    reconciled_at TIMESTAMPTZ NOT NULL,
    failure TEXT CHECK (length(failure) BETWEEN 1 AND 500),
    PRIMARY KEY (plugin_id, org_id),
    FOREIGN KEY (plugin_id, package_sha256) REFERENCES plugin_packages (plugin_id, manifest_sha256) ON DELETE RESTRICT
);

-- +goose Down
DROP TABLE plugin_capability_registrations;
