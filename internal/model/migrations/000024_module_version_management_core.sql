-- +goose Up

ALTER TABLE resource_types
    ADD COLUMN catalogue_status TEXT NOT NULL DEFAULT 'active',
    ADD COLUMN resource_version BIGINT NOT NULL DEFAULT 1,
    ADD COLUMN archived_at TIMESTAMPTZ,
    ADD COLUMN archived_by UUID,
    ADD COLUMN archive_reason TEXT,
    ADD CONSTRAINT resource_types_catalogue_status_check CHECK (catalogue_status IN ('active', 'archived')),
    ADD CONSTRAINT resource_types_archive_projection_check CHECK (
        (catalogue_status = 'active' AND archived_at IS NULL AND archived_by IS NULL AND archive_reason IS NULL)
        OR
        (catalogue_status = 'archived' AND archived_at IS NOT NULL AND archived_by IS NOT NULL AND length(btrim(archive_reason)) > 0)
    );

CREATE TABLE resource_type_catalogue_events (
    sequence BIGSERIAL PRIMARY KEY,
    id UUID NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    org_id TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    resource_type_id TEXT NOT NULL,
    event_type TEXT NOT NULL CHECK (event_type IN ('resource_type.archived', 'resource_type.unarchived')),
    resource_version BIGINT NOT NULL,
    actor UUID NOT NULL,
    reason TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (org_id, resource_type_id) REFERENCES resource_types(org_id, id) ON DELETE RESTRICT
);

CREATE INDEX resource_type_catalogue_events_type_sequence
    ON resource_type_catalogue_events(org_id, resource_type_id, sequence);

ALTER TABLE definitions
    ADD COLUMN uuid UUID NOT NULL DEFAULT gen_random_uuid(),
    ADD COLUMN display_name TEXT NOT NULL DEFAULT '',
    ADD COLUMN description TEXT,
    ADD COLUMN tags JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN catalogue_status TEXT NOT NULL DEFAULT 'active',
    ADD COLUMN resource_version BIGINT NOT NULL DEFAULT 1,
    ADD COLUMN archived_at TIMESTAMPTZ,
    ADD COLUMN archived_by UUID,
    ADD COLUMN archive_reason TEXT,
    ADD COLUMN current_default_version_uuid UUID,
    ADD COLUMN previous_default_version_uuid UUID,
    ADD COLUMN managed_default_generation BIGINT NOT NULL DEFAULT 0,
    ADD CONSTRAINT definitions_uuid_unique UNIQUE (uuid),
    ADD CONSTRAINT definitions_catalogue_status_check CHECK (catalogue_status IN ('active', 'archived')),
    ADD CONSTRAINT definitions_archive_projection_check CHECK (
        (catalogue_status = 'active' AND archived_at IS NULL AND archived_by IS NULL AND archive_reason IS NULL)
        OR
        (catalogue_status = 'archived' AND archived_at IS NOT NULL AND archived_by IS NOT NULL AND length(btrim(archive_reason)) > 0)
    );

ALTER TABLE definition_versions
    ADD COLUMN uuid UUID NOT NULL DEFAULT gen_random_uuid(),
    ADD COLUMN module_uuid UUID NOT NULL DEFAULT gen_random_uuid(),
    ADD COLUMN semantic_version TEXT,
    ADD COLUMN artifact_digest TEXT,
    ADD COLUMN semantic_status TEXT NOT NULL DEFAULT 'deprecated',
    ADD COLUMN migration_generation TEXT NOT NULL DEFAULT 'managed',
    ADD COLUMN verification_status TEXT NOT NULL DEFAULT 'unverified',
    ADD COLUMN source_revision TEXT,
    ADD COLUMN release_notes TEXT,
    ADD COLUMN resource_version BIGINT NOT NULL DEFAULT 1,
    ADD COLUMN published_by UUID,
    ADD CONSTRAINT definition_versions_uuid_unique UNIQUE (uuid),
    ADD CONSTRAINT definition_versions_module_version_unique UNIQUE (module_uuid, uuid),
    ADD CONSTRAINT definition_versions_semver_unique UNIQUE (org_id, definition_id, semantic_version),
    ADD CONSTRAINT definition_versions_semantic_status_check CHECK (semantic_status IN ('proposed', 'default', 'deprecated', 'defective')),
    ADD CONSTRAINT definition_versions_migration_generation_check CHECK (migration_generation IN ('v0', 'v1', 'managed')),
    ADD CONSTRAINT definition_versions_verification_status_check CHECK (verification_status = 'unverified');

ALTER TABLE definitions
    ALTER COLUMN latest_version_id DROP NOT NULL,
    ADD CONSTRAINT definitions_current_default_version_fk
        FOREIGN KEY (uuid, current_default_version_uuid)
        REFERENCES definition_versions(module_uuid, uuid) INITIALLY DEFERRED,
    ADD CONSTRAINT definitions_previous_default_version_fk
        FOREIGN KEY (uuid, previous_default_version_uuid)
        REFERENCES definition_versions(module_uuid, uuid) INITIALLY DEFERRED;

-- Existing opaque versions are historical evidence. Do not invent SemVer or digests.
UPDATE definition_versions SET migration_generation = 'v0';
UPDATE definition_versions version
SET module_uuid = module.uuid
FROM definitions module
WHERE module.org_id = version.org_id AND module.id = version.definition_id;

-- Flush deferred foreign-key checks before creating partial indexes in the
-- same transaction. PostgreSQL otherwise rejects CREATE INDEX when a legacy
-- backfill produced pending constraint-trigger events.
SET CONSTRAINTS ALL IMMEDIATE;

UPDATE definitions module
SET current_default_version_uuid = version.uuid
FROM definition_versions version
WHERE version.org_id = module.org_id
  AND version.definition_id = module.id
  AND version.version_id = module.latest_version_id;

UPDATE definition_versions version
SET semantic_status = 'default'
FROM definitions module
WHERE module.org_id = version.org_id
  AND module.id = version.definition_id
  AND module.current_default_version_uuid = version.uuid;

CREATE UNIQUE INDEX definition_versions_one_default
    ON definition_versions (org_id, definition_id)
    WHERE semantic_status = 'default';

CREATE UNIQUE INDEX definition_versions_one_proposed
    ON definition_versions (org_id, definition_id)
    WHERE semantic_status = 'proposed';

ALTER TABLE envs
    ADD COLUMN labels JSONB NOT NULL DEFAULT '{}'::jsonb;

ALTER TABLE env_types
    ADD COLUMN is_production BOOLEAN NOT NULL DEFAULT FALSE;

CREATE TABLE module_catalogue_events (
    sequence BIGSERIAL PRIMARY KEY,
    id UUID NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    org_id TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    module_uuid UUID NOT NULL REFERENCES definitions(uuid) ON DELETE RESTRICT,
    event_type TEXT NOT NULL,
    module_resource_version BIGINT NOT NULL,
    actor UUID NOT NULL,
    reason TEXT NOT NULL,
    correlation_id UUID,
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX module_catalogue_events_module_sequence ON module_catalogue_events(module_uuid, sequence);

CREATE TABLE module_version_lifecycle_events (
    sequence BIGSERIAL PRIMARY KEY,
    id UUID NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    org_id TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    module_uuid UUID NOT NULL REFERENCES definitions(uuid) ON DELETE RESTRICT,
    version_uuid UUID NOT NULL,
    from_status TEXT,
    to_status TEXT NOT NULL,
    version_resource_version BIGINT NOT NULL,
    actor UUID NOT NULL,
    reason TEXT,
    correlation_id UUID,
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT module_version_lifecycle_events_status_check CHECK (
        (from_status IS NULL OR from_status IN ('proposed', 'default', 'deprecated', 'defective'))
        AND to_status IN ('proposed', 'default', 'deprecated', 'defective')
    ),
    CONSTRAINT module_version_lifecycle_events_reason_check CHECK (from_status IS NULL OR length(btrim(reason)) > 0)
);

ALTER TABLE module_version_lifecycle_events
    ADD CONSTRAINT module_version_lifecycle_events_version_fk
    FOREIGN KEY (module_uuid, version_uuid) REFERENCES definition_versions(module_uuid, uuid) ON DELETE RESTRICT;

CREATE INDEX module_version_lifecycle_events_version_sequence ON module_version_lifecycle_events(version_uuid, sequence);

CREATE TABLE module_core_commands (
    org_id TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    command_scope TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    command_fingerprint TEXT NOT NULL,
    actor UUID NOT NULL,
    response JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, command_scope, idempotency_key)
);

CREATE TABLE environment_module_version_pins (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    project_uuid UUID NOT NULL,
    project_id TEXT NOT NULL,
    environment_uuid UUID NOT NULL,
    environment_id TEXT NOT NULL,
    module_uuid UUID NOT NULL REFERENCES definitions(uuid) ON DELETE RESTRICT,
    version_uuid UUID NOT NULL,
    status TEXT NOT NULL DEFAULT 'active',
    resource_version BIGINT NOT NULL DEFAULT 1,
    activation_event_id UUID NOT NULL,
    bulk_operation_id UUID,
    override_operation_id UUID,
    override_target_version_uuid UUID,
    override_actor UUID,
    override_reason TEXT,
    override_deployment_id UUID,
    created_by UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    removed_at TIMESTAMPTZ,
    CONSTRAINT environment_module_version_pins_status_check CHECK (status IN ('active', 'override_pending', 'overridden', 'removed')),
    CONSTRAINT environment_module_version_pins_override_check CHECK (
        status NOT IN ('override_pending', 'overridden')
        OR (override_operation_id IS NOT NULL AND override_target_version_uuid IS NOT NULL AND override_actor IS NOT NULL AND length(btrim(override_reason)) > 0)
    )
);

ALTER TABLE environment_module_version_pins
    ADD CONSTRAINT environment_module_version_pins_version_fk
    FOREIGN KEY (module_uuid, version_uuid) REFERENCES definition_versions(module_uuid, uuid) ON DELETE RESTRICT;

CREATE UNIQUE INDEX environment_module_version_pins_protected
    ON environment_module_version_pins(environment_uuid, module_uuid)
    WHERE status IN ('active', 'override_pending');
CREATE INDEX environment_module_version_pins_version ON environment_module_version_pins(version_uuid, status);

CREATE TABLE environment_module_version_pin_events (
    sequence BIGSERIAL PRIMARY KEY,
    id UUID NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    pin_id UUID NOT NULL REFERENCES environment_module_version_pins(id) ON DELETE RESTRICT,
    revision BIGINT NOT NULL,
    event_type TEXT NOT NULL,
    from_status TEXT,
    to_status TEXT NOT NULL,
    activation_event_id UUID NOT NULL,
    actor UUID NOT NULL,
    actor_type TEXT NOT NULL,
    reason TEXT NOT NULL,
    operation_id UUID,
    deployment_id UUID,
    bulk_operation_id UUID,
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(pin_id, revision)
);

CREATE INDEX environment_module_version_pin_events_pin_sequence
    ON environment_module_version_pin_events(pin_id, sequence);

-- +goose Down
DROP TABLE IF EXISTS environment_module_version_pin_events;
DROP INDEX IF EXISTS environment_module_version_pins_version;
DROP INDEX IF EXISTS environment_module_version_pins_protected;
DROP TABLE IF EXISTS environment_module_version_pins;
DROP TABLE IF EXISTS module_core_commands;
DROP TABLE IF EXISTS module_version_lifecycle_events;
DROP TABLE IF EXISTS module_catalogue_events;
DROP TABLE IF EXISTS resource_type_catalogue_events;

ALTER TABLE env_types
    DROP COLUMN IF EXISTS is_production;

ALTER TABLE envs
    DROP COLUMN IF EXISTS labels;

ALTER TABLE definitions
    DROP CONSTRAINT IF EXISTS definitions_previous_default_version_fk,
    DROP CONSTRAINT IF EXISTS definitions_current_default_version_fk;

ALTER TABLE definition_versions
    DROP CONSTRAINT IF EXISTS definition_versions_verification_status_check,
    DROP CONSTRAINT IF EXISTS definition_versions_migration_generation_check,
    DROP CONSTRAINT IF EXISTS definition_versions_semver_unique,
    DROP CONSTRAINT IF EXISTS definition_versions_semantic_status_check,
    DROP CONSTRAINT IF EXISTS definition_versions_module_version_unique,
    DROP CONSTRAINT IF EXISTS definition_versions_module_uuid_fk,
    DROP CONSTRAINT IF EXISTS definition_versions_uuid_unique,
    DROP COLUMN IF EXISTS published_by,
    DROP COLUMN IF EXISTS resource_version,
    DROP COLUMN IF EXISTS release_notes,
    DROP COLUMN IF EXISTS source_revision,
    DROP COLUMN IF EXISTS verification_status,
    DROP COLUMN IF EXISTS migration_generation,
    DROP COLUMN IF EXISTS semantic_version,
    DROP COLUMN IF EXISTS semantic_status,
    DROP COLUMN IF EXISTS artifact_digest,
    DROP COLUMN IF EXISTS module_uuid,
    DROP COLUMN IF EXISTS uuid;

ALTER TABLE definitions
    DROP CONSTRAINT IF EXISTS definitions_archive_projection_check,
    DROP CONSTRAINT IF EXISTS definitions_catalogue_status_check,
    DROP CONSTRAINT IF EXISTS definitions_uuid_unique,
    DROP COLUMN IF EXISTS archive_reason,
    DROP COLUMN IF EXISTS managed_default_generation,
    DROP COLUMN IF EXISTS previous_default_version_uuid,
    DROP COLUMN IF EXISTS current_default_version_uuid,
    DROP COLUMN IF EXISTS archived_by,
    DROP COLUMN IF EXISTS archived_at,
    DROP COLUMN IF EXISTS resource_version,
    DROP COLUMN IF EXISTS catalogue_status,
    DROP COLUMN IF EXISTS tags,
    DROP COLUMN IF EXISTS description,
    DROP COLUMN IF EXISTS display_name,
    DROP COLUMN IF EXISTS uuid;

-- Empty Module shells introduced by Core have no legacy latest version. A
-- recovery deployment must preserve them without inventing or deleting data,
-- so the pre-Core application sees a nullable legacy projection.

ALTER TABLE resource_types
    DROP CONSTRAINT IF EXISTS resource_types_archive_projection_check,
    DROP CONSTRAINT IF EXISTS resource_types_catalogue_status_check,
    DROP COLUMN IF EXISTS archive_reason,
    DROP COLUMN IF EXISTS archived_by,
    DROP COLUMN IF EXISTS archived_at,
    DROP COLUMN IF EXISTS resource_version,
    DROP COLUMN IF EXISTS catalogue_status;
