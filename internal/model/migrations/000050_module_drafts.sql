-- +goose Up
CREATE TABLE module_drafts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id TEXT NOT NULL REFERENCES orgs(id) ON DELETE RESTRICT,
    module_uuid UUID NOT NULL REFERENCES definitions(uuid) ON DELETE RESTRICT,
    resource_version BIGINT NOT NULL CHECK (resource_version > 0),
    content JSONB NOT NULL CHECK (jsonb_typeof(content) = 'object'),
    status TEXT NOT NULL CHECK (status IN ('open', 'published', 'discarded')),
    published_version_uuid UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by UUID NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by UUID NOT NULL,
    FOREIGN KEY (module_uuid, published_version_uuid) REFERENCES definition_versions(module_uuid, uuid) ON DELETE RESTRICT,
    CHECK ((status = 'published') = (published_version_uuid IS NOT NULL))
);
CREATE UNIQUE INDEX module_drafts_one_open ON module_drafts(module_uuid) WHERE status = 'open';
CREATE INDEX module_drafts_history ON module_drafts(org_id, module_uuid, id);

CREATE TABLE module_draft_revisions (
    draft_id UUID NOT NULL REFERENCES module_drafts(id) ON DELETE RESTRICT,
    resource_version BIGINT NOT NULL CHECK (resource_version > 0),
    content JSONB NOT NULL CHECK (jsonb_typeof(content) = 'object'),
    status TEXT NOT NULL CHECK (status IN ('open', 'published', 'discarded')),
    published_version_uuid UUID REFERENCES definition_versions(uuid) ON DELETE RESTRICT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    actor UUID NOT NULL,
    reason TEXT NOT NULL CHECK (length(btrim(reason)) > 0),
    PRIMARY KEY (draft_id, resource_version)
);

-- +goose StatementBegin
CREATE FUNCTION protect_module_draft_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE' AND (OLD.status <> 'open' OR NEW.id IS DISTINCT FROM OLD.id
        OR NEW.org_id IS DISTINCT FROM OLD.org_id OR NEW.module_uuid IS DISTINCT FROM OLD.module_uuid
        OR NEW.created_at IS DISTINCT FROM OLD.created_at OR NEW.created_by IS DISTINCT FROM OLD.created_by
        OR NEW.resource_version <> OLD.resource_version + 1) THEN
        RAISE EXCEPTION 'Draft identity and closed revisions are immutable' USING ERRCODE = '23514';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM definitions WHERE uuid = NEW.module_uuid AND org_id = NEW.org_id) THEN
        RAISE EXCEPTION 'Draft Module is outside its organization' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER module_draft_identity BEFORE INSERT OR UPDATE ON module_drafts
FOR EACH ROW EXECUTE FUNCTION protect_module_draft_identity();

-- +goose StatementBegin
CREATE FUNCTION protect_module_draft_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'Draft authoring history is immutable and retained' USING ERRCODE = '23514';
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER module_draft_revision_immutable BEFORE UPDATE OR DELETE ON module_draft_revisions
FOR EACH ROW EXECUTE FUNCTION protect_module_draft_revision();

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM module_drafts) THEN
        RAISE EXCEPTION 'cannot downgrade while Draft authoring history exists';
    END IF;
END $$;
-- +goose StatementEnd
DROP TABLE module_draft_revisions;
DROP FUNCTION protect_module_draft_revision();
DROP TABLE module_drafts;
DROP FUNCTION protect_module_draft_identity();
