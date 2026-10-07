-- +goose Up
-- What Core last read from the status endpoint of an installation's running
-- plugin: the reported installation, package digest and migration generation,
-- or why no report could be read. An observation is a fact the plugin
-- reports, not operator state, so it is recorded outside the Plugin Manager
-- revision and audit. Diagnostics are derived from it against what Core
-- recorded and never stored.
CREATE TABLE plugin_status_observations (
    installation_uuid UUID PRIMARY KEY REFERENCES plugin_installations (installation_uuid) ON DELETE RESTRICT,
    observed_at TIMESTAMPTZ NOT NULL,
    failure TEXT CHECK (failure IN ('plugin_status_unreachable', 'plugin_status_invalid')),
    detail TEXT CHECK (length(detail) BETWEEN 1 AND 500),
    reported_installation_uuid UUID,
    reported_package_sha256 TEXT CHECK (reported_package_sha256 ~ '^[0-9a-f]{64}$'),
    reported_migration_generation BIGINT CHECK (reported_migration_generation >= 0),
    CHECK ((failure IS NULL) = (detail IS NULL)),
    CHECK ((failure IS NULL) = (reported_installation_uuid IS NOT NULL)),
    CHECK ((failure IS NULL) = (reported_package_sha256 IS NOT NULL)),
    CHECK ((failure IS NULL) = (reported_migration_generation IS NOT NULL))
);

-- +goose Down
DROP TABLE plugin_status_observations;
