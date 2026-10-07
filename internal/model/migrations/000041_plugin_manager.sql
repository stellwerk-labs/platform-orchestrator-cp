-- +goose Up
-- The Plugin Manager is the only source of plugin installation truth. Every
-- operator change runs in one transaction that bumps the revision, writes one
-- append-only audit row and stores the idempotent response. Control Plane
-- replicas poll the revision and reload their read snapshot when it moves.
CREATE TABLE plugin_manager_state (
    singleton BOOLEAN PRIMARY KEY DEFAULT true CHECK (singleton),
    revision BIGINT NOT NULL CHECK (revision >= 0)
);
INSERT INTO plugin_manager_state (singleton, revision) VALUES (true, 0);

-- A package is the registration manifest Core serves for one plugin version,
-- identified by the digest of its package manifest. Packages are immutable.
CREATE TABLE plugin_packages (
    manifest_sha256 TEXT PRIMARY KEY CHECK (manifest_sha256 ~ '^[0-9a-f]{64}$'),
    plugin_id TEXT NOT NULL,
    version TEXT NOT NULL,
    manifest JSONB NOT NULL CHECK (jsonb_typeof(manifest) = 'object'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by UUID NOT NULL,
    UNIQUE (plugin_id, version),
    UNIQUE (plugin_id, manifest_sha256)
);

-- Core assigns the installation UUID. Entitlement grants bind to it, so it
-- never changes and is never reused for another installation.
CREATE TABLE plugin_installations (
    installation_uuid UUID PRIMARY KEY,
    plugin_id TEXT NOT NULL,
    package_sha256 TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('installed', 'active', 'disabled', 'uninstalled')),
    backend_url TEXT NOT NULL,
    ui_origin TEXT,
    service_user_id UUID,
    admission_checker_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by UUID NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by UUID NOT NULL,
    UNIQUE (installation_uuid, plugin_id),
    FOREIGN KEY (plugin_id, package_sha256) REFERENCES plugin_packages (plugin_id, manifest_sha256) ON DELETE RESTRICT,
    CHECK ((service_user_id IS NULL) = (admission_checker_id IS NULL)),
    CHECK (service_user_id IS DISTINCT FROM admission_checker_id OR service_user_id IS NULL)
);
CREATE UNIQUE INDEX plugin_installations_one_per_plugin ON plugin_installations (plugin_id) WHERE state <> 'uninstalled';

-- In v1alpha an installation is enabled for exactly one organization.
CREATE TABLE plugin_enablements (
    plugin_id TEXT NOT NULL,
    org_id TEXT NOT NULL REFERENCES orgs (id) ON DELETE RESTRICT,
    installation_uuid UUID NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('enabled', 'disabled')),
    timezone TEXT NOT NULL,
    approval_policies JSONB NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(approval_policies) = 'object'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by UUID NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by UUID NOT NULL,
    PRIMARY KEY (plugin_id, org_id),
    UNIQUE (installation_uuid),
    FOREIGN KEY (installation_uuid, plugin_id) REFERENCES plugin_installations (installation_uuid, plugin_id) ON DELETE RESTRICT
);

-- The contributions an active installation serves, materialised from its
-- package on activation. A UI route prefix belongs to one installation.
CREATE TABLE plugin_extension_registrations (
    installation_uuid UUID NOT NULL REFERENCES plugin_installations (installation_uuid) ON DELETE RESTRICT,
    package_sha256 TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('capability', 'operation', 'service_grant', 'authorization_policy', 'ui')),
    key TEXT NOT NULL,
    definition JSONB NOT NULL,
    PRIMARY KEY (installation_uuid, kind, key)
);
CREATE UNIQUE INDEX plugin_extension_registrations_ui_route ON plugin_extension_registrations (key) WHERE kind = 'ui';

-- The exact response of every command, replayed byte for byte.
CREATE TABLE plugin_manager_commands (
    idempotency_key_sha256 BYTEA PRIMARY KEY CHECK (octet_length(idempotency_key_sha256) = 32),
    fingerprint TEXT NOT NULL,
    actor UUID NOT NULL,
    response TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One row per committed change, never updated or deleted.
CREATE TABLE plugin_manager_audit (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    revision BIGINT NOT NULL UNIQUE CHECK (revision > 0),
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    actor UUID NOT NULL,
    action TEXT NOT NULL,
    installation_uuid UUID,
    org_id TEXT,
    reason TEXT NOT NULL CHECK (length(btrim(reason)) BETWEEN 1 AND 1000),
    request_id TEXT,
    idempotency_key_sha256 BYTEA NOT NULL CHECK (octet_length(idempotency_key_sha256) = 32),
    before JSONB,
    after JSONB
);

-- +goose StatementBegin
CREATE FUNCTION plugin_manager_audit_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'plugin_manager_audit is append-only';
END
$$;
-- +goose StatementEnd
CREATE TRIGGER plugin_manager_audit_no_change BEFORE UPDATE OR DELETE ON plugin_manager_audit
    FOR EACH ROW EXECUTE FUNCTION plugin_manager_audit_append_only();
CREATE TRIGGER plugin_manager_audit_no_truncate BEFORE TRUNCATE ON plugin_manager_audit
    FOR EACH STATEMENT EXECUTE FUNCTION plugin_manager_audit_append_only();

-- +goose Down
-- A downgrade would silently drop installation truth and its audit trail.
-- +goose StatementBegin
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM plugin_manager_audit) THEN
        RAISE EXCEPTION 'cannot remove recorded Plugin Manager state and audit';
    END IF;
END $$;
-- +goose StatementEnd
DROP TABLE plugin_manager_audit;
DROP FUNCTION plugin_manager_audit_append_only();
DROP TABLE plugin_manager_commands;
DROP TABLE plugin_extension_registrations;
DROP TABLE plugin_enablements;
DROP TABLE plugin_installations;
DROP TABLE plugin_packages;
DROP TABLE plugin_manager_state;
