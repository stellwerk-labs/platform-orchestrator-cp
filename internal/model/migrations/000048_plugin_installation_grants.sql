-- +goose Up
-- A grant without an organization covers every organization of its
-- installation, including organizations created later. Its binding is the
-- plugin and installation alone, so the organization becomes nullable and
-- every uniqueness that names it treats NULL as one value: one current
-- installation-wide binding per plugin and installation, and one grant per
-- issuer, binding and sequence. Only an installation-wide grant may limit
-- the organizations an installation enables. Feature activations stay per
-- organization: an organization activates a feature under whichever grant
-- covers it.
ALTER TABLE plugin_entitlement_grants ALTER COLUMN organization_uuid DROP NOT NULL,
    DROP CONSTRAINT plugin_entitlement_grants_issuer_installation_uuid_plugin_i_key,
    ADD CONSTRAINT plugin_entitlement_grants_sequence_key UNIQUE NULLS NOT DISTINCT (issuer, installation_uuid, plugin_id, organization_uuid, grant_sequence),
    ADD CONSTRAINT plugin_entitlement_grants_limits_scope CHECK (organization_uuid IS NULL OR limits = '{}'::jsonb);
ALTER TABLE plugin_entitlement_bindings DROP CONSTRAINT plugin_entitlement_bindings_pkey,
    ALTER COLUMN organization_uuid DROP NOT NULL,
    ADD CONSTRAINT plugin_entitlement_bindings_key UNIQUE NULLS NOT DISTINCT (installation_uuid, plugin_id, organization_uuid);

-- +goose Down
-- The previous application cannot verify, decide on or keep installation-wide
-- grants; a downgrade would silently discard licence evidence.
-- +goose StatementBegin
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM plugin_entitlement_grants WHERE organization_uuid IS NULL)
       OR EXISTS (SELECT 1 FROM plugin_entitlement_bindings WHERE organization_uuid IS NULL) THEN
        RAISE EXCEPTION 'cannot downgrade while installation-wide entitlement grants exist';
    END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE plugin_entitlement_bindings DROP CONSTRAINT plugin_entitlement_bindings_key,
    ALTER COLUMN organization_uuid SET NOT NULL,
    ADD CONSTRAINT plugin_entitlement_bindings_pkey PRIMARY KEY (installation_uuid, plugin_id, organization_uuid);
ALTER TABLE plugin_entitlement_grants DROP CONSTRAINT plugin_entitlement_grants_limits_scope,
    DROP CONSTRAINT plugin_entitlement_grants_sequence_key,
    ALTER COLUMN organization_uuid SET NOT NULL,
    ADD CONSTRAINT plugin_entitlement_grants_issuer_installation_uuid_plugin_i_key UNIQUE (issuer, installation_uuid, plugin_id, organization_uuid, grant_sequence);
