-- +goose Up
-- An installation serves every organization it is enabled for. Its identities
-- belong to no organization, Core confines every plugin effect to the
-- organization of the request in the effect's own transaction, and every piece
-- of Core-side plugin state is keyed by organization and plugin.
ALTER TABLE plugin_enablements DROP CONSTRAINT plugin_enablements_installation_uuid_key;
CREATE INDEX plugin_enablements_installation ON plugin_enablements (installation_uuid);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM plugin_enablements GROUP BY installation_uuid HAVING count(*) > 1) THEN
        RAISE EXCEPTION 'cannot downgrade while an installation is enabled for several organizations';
    END IF;
END $$;
-- +goose StatementEnd
DROP INDEX plugin_enablements_installation;
ALTER TABLE plugin_enablements ADD CONSTRAINT plugin_enablements_installation_uuid_key UNIQUE (installation_uuid);
