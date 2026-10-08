-- +goose Up
-- An installation serves either the organizations an operator enables one by
-- one (selected) or every organization (all). With all, Core enables every
-- existing organization when the mode is set and every organization created
-- later inside that organization's creation transaction, each with the
-- installation's default timezone and approval policies. An organization opts
-- out through a disabled enablement, which Core never changes on its own.
-- Only an installation Core serves can serve every organization, so an
-- uninstall returns it to selected.
ALTER TABLE plugin_installations
    ADD COLUMN organization_mode TEXT NOT NULL DEFAULT 'selected' CHECK (organization_mode IN ('selected', 'all')),
    ADD COLUMN default_timezone TEXT NOT NULL DEFAULT 'UTC',
    ADD COLUMN default_approval_policies JSONB NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(default_approval_policies) = 'object'),
    ADD CONSTRAINT plugin_installations_all_organizations_served CHECK (organization_mode = 'selected' OR state IN ('active', 'disabled'));

-- +goose Down
-- Below this migration Core enables no organization on its own; a downgrade
-- would silently drop the operator's instruction to serve every organization.
-- +goose StatementBegin
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM plugin_installations WHERE organization_mode = 'all') THEN
        RAISE EXCEPTION 'cannot downgrade while an installation serves all organizations';
    END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE plugin_installations DROP CONSTRAINT plugin_installations_all_organizations_served,
    DROP COLUMN default_approval_policies, DROP COLUMN default_timezone, DROP COLUMN organization_mode;
