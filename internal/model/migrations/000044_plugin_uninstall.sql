-- +goose Up
-- Uninstall keeps an installation's UUID, package, identities and the
-- plugin's own data; reinstall reuses that UUID, so its licence grants match
-- again. Purge retires the UUID for good. A plugin keeps at most one
-- installation that is not purged: a retained installation is reinstalled or
-- purged, never shadowed by a new one with another UUID.
ALTER TABLE plugin_installations DROP CONSTRAINT plugin_installations_state_check,
    ADD CONSTRAINT plugin_installations_state_check CHECK (state IN ('installed', 'active', 'disabled', 'uninstalled', 'purged'));
DROP INDEX plugin_installations_one_per_plugin;
CREATE UNIQUE INDEX plugin_installations_one_per_plugin ON plugin_installations (plugin_id) WHERE state <> 'purged';

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM plugin_installations WHERE state = 'purged') THEN
        RAISE EXCEPTION 'cannot downgrade below purged plugin installations';
    END IF;
END $$;
-- +goose StatementEnd
DROP INDEX plugin_installations_one_per_plugin;
CREATE UNIQUE INDEX plugin_installations_one_per_plugin ON plugin_installations (plugin_id) WHERE state <> 'uninstalled';
ALTER TABLE plugin_installations DROP CONSTRAINT plugin_installations_state_check,
    ADD CONSTRAINT plugin_installations_state_check CHECK (state IN ('installed', 'active', 'disabled', 'uninstalled'));
