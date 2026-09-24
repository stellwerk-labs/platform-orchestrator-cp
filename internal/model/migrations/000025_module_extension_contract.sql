-- +goose Up

-- Existing installations of migration 24 already have this constraint. Fresh
-- installations add it here, after the populated backfill transaction has
-- committed, avoiding PostgreSQL's pending-trigger ALTER TABLE restriction.
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'definition_versions_module_uuid_fk') THEN
        ALTER TABLE definition_versions
            ADD CONSTRAINT definition_versions_module_uuid_fk
            FOREIGN KEY (module_uuid) REFERENCES definitions(uuid) ON DELETE RESTRICT NOT VALID;
    END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE definition_versions VALIDATE CONSTRAINT definition_versions_module_uuid_fk;

CREATE TABLE module_operation_reservations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    module_uuid UUID NOT NULL REFERENCES definitions(uuid) ON DELETE RESTRICT,
    namespace TEXT NOT NULL,
    operation_id UUID NOT NULL,
    related_resource_id TEXT NOT NULL,
    reason TEXT NOT NULL,
    resource_version BIGINT NOT NULL DEFAULT 1,
    acquired_by UUID NOT NULL,
    acquired_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    released_by UUID,
    released_at TIMESTAMPTZ,
    release_reason TEXT,
    CONSTRAINT module_operation_reservations_namespace_check CHECK (namespace ~ '^[a-z][a-z0-9.-]*$'),
    CONSTRAINT module_operation_reservations_reason_check CHECK (length(btrim(reason)) > 0),
    CONSTRAINT module_operation_reservations_release_check CHECK (
        (released_at IS NULL AND released_by IS NULL AND release_reason IS NULL)
        OR
        (released_at IS NOT NULL AND released_by IS NOT NULL AND length(btrim(release_reason)) > 0)
    ),
    UNIQUE (org_id, namespace, operation_id)
);

CREATE UNIQUE INDEX module_operation_reservations_exclusive
    ON module_operation_reservations(module_uuid) WHERE released_at IS NULL;
CREATE INDEX module_operation_reservations_module
    ON module_operation_reservations(org_id, module_uuid, acquired_at DESC);

-- Related resources and contextual actions share one namespaced, opaque extension
-- projection. Core owns identity and visibility; add-ons own the payload contract.
CREATE TABLE module_extension_contributions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    module_uuid UUID NOT NULL REFERENCES definitions(uuid) ON DELETE RESTRICT,
    version_uuid UUID,
    environment_uuid UUID,
    namespace TEXT NOT NULL,
    external_resource_id TEXT NOT NULL,
    contribution_kind TEXT NOT NULL CHECK (contribution_kind IN ('related_resource', 'contextual_action')),
    lifecycle_state TEXT NOT NULL CHECK (lifecycle_state IN ('draft', 'active', 'terminal')),
    label TEXT NOT NULL,
    target_url TEXT,
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_by UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, namespace, external_resource_id, contribution_kind),
    FOREIGN KEY (module_uuid, version_uuid) REFERENCES definition_versions(module_uuid, uuid) ON DELETE RESTRICT
);

CREATE INDEX module_extension_contributions_module
    ON module_extension_contributions(org_id, module_uuid, created_at DESC);
CREATE INDEX module_extension_contributions_environment
    ON module_extension_contributions(org_id, environment_uuid, created_at DESC)
    WHERE environment_uuid IS NOT NULL;

-- +goose Down
DROP TABLE IF EXISTS module_extension_contributions;
DROP INDEX IF EXISTS module_operation_reservations_module;
DROP INDEX IF EXISTS module_operation_reservations_exclusive;
DROP TABLE IF EXISTS module_operation_reservations;
