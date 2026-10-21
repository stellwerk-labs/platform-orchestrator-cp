-- +goose Up
-- A pending upgrade names the package an installation is to serve next. Core
-- keeps serving the current package and its registrations until an
-- activation reads the target digest from the running plugin, then switches
-- package and registrations in one transaction. The installation UUID, and
-- with it the licence binding, never changes.
ALTER TABLE plugin_installations
    ADD COLUMN target_package_sha256 TEXT,
    ADD CONSTRAINT plugin_installations_target_package FOREIGN KEY (plugin_id, target_package_sha256)
        REFERENCES plugin_packages (plugin_id, manifest_sha256) ON DELETE RESTRICT,
    ADD CONSTRAINT plugin_installations_target_differs CHECK (target_package_sha256 <> package_sha256);

-- +goose Down
ALTER TABLE plugin_installations DROP COLUMN target_package_sha256;
