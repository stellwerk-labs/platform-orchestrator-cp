-- +goose Up
CREATE TABLE module_draft_test_snapshots (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id TEXT NOT NULL REFERENCES orgs(id) ON DELETE RESTRICT,
    module_uuid UUID NOT NULL REFERENCES definitions(uuid) ON DELETE RESTRICT,
    draft_id UUID NOT NULL,
    draft_revision BIGINT NOT NULL,
    resource_type_version_uuid UUID NOT NULL REFERENCES resource_type_versions(uuid) ON DELETE RESTRICT,
    project_uuid UUID NOT NULL,
    environment_uuid UUID NOT NULL,
    content JSONB NOT NULL CHECK (jsonb_typeof(content) = 'object'),
    captured_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    captured_by UUID NOT NULL,
    reason TEXT NOT NULL CHECK (length(btrim(reason)) BETWEEN 1 AND 2000),
    FOREIGN KEY (draft_id, draft_revision) REFERENCES module_draft_revisions(draft_id, resource_version) ON DELETE RESTRICT,
    CHECK (captured_by <> '00000000-0000-0000-0000-000000000000')
);
CREATE INDEX module_draft_test_snapshots_scope ON module_draft_test_snapshots(org_id, environment_uuid, module_uuid, id);

-- Environment coordinates are retained historical scope, not foreign keys that
-- would prevent ordinary Environment deletion. A recreated slug has a new UUID.
-- +goose StatementBegin
CREATE FUNCTION protect_module_draft_test_snapshot() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'Draft test snapshots are immutable and retained' USING ERRCODE = '23514';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM module_drafts d
        JOIN module_draft_revisions r ON r.draft_id=d.id AND r.resource_version=d.resource_version
        JOIN definitions m ON m.uuid=d.module_uuid
        JOIN resource_type_versions v ON v.resource_type_uuid=m.resource_type_uuid
        JOIN envs e ON e.uuid=NEW.environment_uuid AND e.org_id=d.org_id
        WHERE d.id=NEW.draft_id AND d.org_id=NEW.org_id AND d.module_uuid=NEW.module_uuid
          AND d.status='open' AND d.resource_version=NEW.draft_revision
          AND r.content=NEW.content AND e.project_uuid=NEW.project_uuid AND e.status='active'
          AND v.uuid=NEW.resource_type_version_uuid
          AND v.version=COALESCE((r.content->>'resource_type_version')::bigint,1)
    ) THEN
        RAISE EXCEPTION 'Draft test snapshot must match an open reviewed revision, exact contract and active Environment scope' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER module_draft_test_snapshot_immutable BEFORE INSERT OR UPDATE OR DELETE ON module_draft_test_snapshots
FOR EACH ROW EXECUTE FUNCTION protect_module_draft_test_snapshot();

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM module_draft_test_snapshots) THEN
        RAISE EXCEPTION 'cannot downgrade while Draft test snapshots exist';
    END IF;
END $$;
-- +goose StatementEnd
DROP TABLE module_draft_test_snapshots;
DROP FUNCTION protect_module_draft_test_snapshot();
