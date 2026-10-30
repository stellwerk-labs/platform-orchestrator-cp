-- +goose Up
CREATE TABLE resource_type_versions (
    uuid UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    resource_type_uuid UUID NOT NULL REFERENCES resource_types(uuid) ON DELETE CASCADE,
    version BIGINT NOT NULL CHECK (version > 0),
    output_schema JSONB NOT NULL,
    module_contract JSONB,
    created_at TIMESTAMP WITHOUT TIME ZONE NOT NULL,
    created_by UUID,
    reason TEXT,
    UNIQUE (resource_type_uuid, version)
);

-- Preserve the actual interface, including absent versus empty declarations.
-- Historical publication actors are unknown and must not be fabricated.
INSERT INTO resource_type_versions (resource_type_uuid, version, output_schema, module_contract, created_at)
SELECT uuid, 1, output_schema, module_contract, created_at FROM resource_types;

-- +goose StatementBegin
CREATE FUNCTION initialize_resource_type_version() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO resource_type_versions (resource_type_uuid, version, output_schema, module_contract, created_at)
    VALUES (NEW.uuid, 1, NEW.output_schema, NEW.module_contract, NEW.created_at);
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER resource_type_initial_version
AFTER INSERT ON resource_types FOR EACH ROW EXECUTE FUNCTION initialize_resource_type_version();

-- +goose StatementBegin
CREATE FUNCTION protect_resource_type_version() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    -- An unused legacy-compatible catalogue entry may still be deleted.
    -- Further published contracts must be retained even without consumers.
    IF TG_OP = 'DELETE' AND OLD.version = 1
       AND NOT EXISTS (SELECT 1 FROM resource_types WHERE uuid = OLD.resource_type_uuid) THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'Resource Type versions are immutable and retained' USING ERRCODE = '23514';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER resource_type_version_immutable
BEFORE UPDATE OR DELETE ON resource_type_versions
FOR EACH ROW EXECUTE FUNCTION protect_resource_type_version();

ALTER TABLE definitions ADD COLUMN resource_type_uuid UUID REFERENCES resource_types(uuid) ON DELETE RESTRICT;
UPDATE definitions m SET resource_type_uuid = (
    SELECT r.uuid FROM resource_types r
    WHERE r.id = m.resource_type AND (r.org_id = m.org_id OR r.org_id IS NULL)
    ORDER BY r.org_id NULLS LAST LIMIT 1
);
ALTER TABLE definitions ALTER COLUMN resource_type_uuid SET NOT NULL;

ALTER TABLE definition_versions ADD COLUMN resource_type_version_uuid UUID
    REFERENCES resource_type_versions(uuid) ON DELETE RESTRICT;
UPDATE definition_versions v SET resource_type_version_uuid = r.uuid
FROM definitions m JOIN resource_type_versions r ON r.resource_type_uuid = m.resource_type_uuid AND r.version = 1
WHERE v.org_id = m.org_id AND v.definition_id = m.id;
ALTER TABLE definition_versions ALTER COLUMN resource_type_version_uuid SET NOT NULL;

-- +goose StatementBegin
CREATE FUNCTION bind_module_resource_type() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE' AND (NEW.resource_type_uuid IS DISTINCT FROM OLD.resource_type_uuid
       OR NEW.resource_type IS DISTINCT FROM OLD.resource_type OR NEW.org_id IS DISTINCT FROM OLD.org_id) THEN
        RAISE EXCEPTION 'Module Resource Type identity is immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.resource_type_uuid IS NULL THEN
        SELECT r.uuid INTO NEW.resource_type_uuid FROM resource_types r
        WHERE r.id = NEW.resource_type AND (r.org_id = NEW.org_id OR r.org_id IS NULL)
        ORDER BY r.org_id NULLS LAST LIMIT 1;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM resource_types r WHERE r.uuid = NEW.resource_type_uuid
        AND r.id = NEW.resource_type AND (r.org_id = NEW.org_id OR r.org_id IS NULL)) THEN
        RAISE EXCEPTION 'Module Resource Type binding is outside its scope' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER module_resource_type_binding
BEFORE INSERT OR UPDATE OF resource_type_uuid, resource_type, org_id ON definitions
FOR EACH ROW EXECUTE FUNCTION bind_module_resource_type();

-- +goose StatementBegin
CREATE FUNCTION bind_module_resource_type_version() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE bound_type UUID;
BEGIN
    IF TG_OP = 'UPDATE' AND (NEW.resource_type_version_uuid IS DISTINCT FROM OLD.resource_type_version_uuid
       OR NEW.org_id IS DISTINCT FROM OLD.org_id OR NEW.definition_id IS DISTINCT FROM OLD.definition_id) THEN
        RAISE EXCEPTION 'Module Version Resource Type contract is immutable' USING ERRCODE = '23514';
    END IF;
    SELECT resource_type_uuid INTO bound_type FROM definitions
    WHERE org_id = NEW.org_id AND id = NEW.definition_id;
    IF NEW.resource_type_version_uuid IS NULL THEN
        SELECT uuid INTO NEW.resource_type_version_uuid FROM resource_type_versions
        WHERE resource_type_uuid = bound_type AND version = 1;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM resource_type_versions
        WHERE uuid = NEW.resource_type_version_uuid AND resource_type_uuid = bound_type) THEN
        RAISE EXCEPTION 'Module Version contract does not belong to its Resource Type' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER module_resource_type_version_binding
BEFORE INSERT OR UPDATE OF resource_type_version_uuid, org_id, definition_id ON definition_versions
FOR EACH ROW EXECUTE FUNCTION bind_module_resource_type_version();

-- +goose Down
-- Application recovery retains the schema. Removing snapshots would lose
-- their immutable identities even if their definitions could be reconstructed.
-- +goose StatementBegin
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM resource_type_versions) THEN
        RAISE EXCEPTION 'cannot downgrade while Resource Type version snapshots exist';
    END IF;
END $$;
-- +goose StatementEnd

DROP TRIGGER module_resource_type_version_binding ON definition_versions;
DROP FUNCTION bind_module_resource_type_version();
ALTER TABLE definition_versions DROP COLUMN resource_type_version_uuid;
DROP TRIGGER module_resource_type_binding ON definitions;
DROP FUNCTION bind_module_resource_type();
ALTER TABLE definitions DROP COLUMN resource_type_uuid;
DROP TRIGGER resource_type_initial_version ON resource_types;
DROP FUNCTION initialize_resource_type_version();
DROP TABLE resource_type_versions;
DROP FUNCTION protect_resource_type_version();
