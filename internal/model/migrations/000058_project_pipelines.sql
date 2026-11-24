-- +goose Up
CREATE TABLE project_pipeline_drafts (
 id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
 project_uuid UUID NOT NULL REFERENCES projects(uuid) ON DELETE RESTRICT,
 slug TEXT NOT NULL CHECK(length(slug) BETWEEN 1 AND 64),
 name TEXT NOT NULL CHECK(length(name) BETWEEN 1 AND 120),
 resource_version BIGINT NOT NULL DEFAULT 1 CHECK(resource_version > 0),
 definition JSONB NOT NULL CHECK(jsonb_typeof(definition)='object'),
 updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 updated_by UUID NOT NULL,
 UNIQUE(project_uuid,slug), UNIQUE(project_uuid,id)
);
CREATE TABLE project_pipeline_versions (
 id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
 project_uuid UUID NOT NULL REFERENCES projects(uuid) ON DELETE RESTRICT,
 pipeline_id UUID NOT NULL,
 version INTEGER NOT NULL CHECK(version > 0),
 draft_resource_version BIGINT NOT NULL,
 name TEXT NOT NULL,
 digest TEXT NOT NULL CHECK(length(digest)=64),
 definition JSONB NOT NULL CHECK(jsonb_typeof(definition)='object'),
 published_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 published_by UUID NOT NULL,
 retired_at TIMESTAMPTZ,
 FOREIGN KEY(project_uuid,pipeline_id) REFERENCES project_pipeline_drafts(project_uuid,id),
 UNIQUE(pipeline_id,version), UNIQUE(pipeline_id,draft_resource_version), UNIQUE(project_uuid,id)
);
CREATE TABLE project_pipeline_policy (
 project_uuid UUID PRIMARY KEY REFERENCES projects(uuid) ON DELETE RESTRICT,
 resource_version BIGINT NOT NULL CHECK(resource_version > 0),
 enabled BOOLEAN NOT NULL,
 default_version_id UUID,
 allowed_version_ids UUID[] NOT NULL DEFAULT '{}',
 updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 updated_by UUID NOT NULL,
 FOREIGN KEY(project_uuid,default_version_id) REFERENCES project_pipeline_versions(project_uuid,id),
 CHECK((enabled AND default_version_id IS NOT NULL AND default_version_id=ANY(allowed_version_ids)) OR
       (NOT enabled AND default_version_id IS NULL AND cardinality(allowed_version_ids)=0))
);
CREATE TABLE project_pipeline_publications (
 project_uuid UUID NOT NULL REFERENCES projects(uuid) ON DELETE RESTRICT,
 request_key TEXT NOT NULL CHECK(length(request_key) BETWEEN 1 AND 128),
 fingerprint TEXT NOT NULL,
 actor UUID NOT NULL,
 response JSONB NOT NULL,
 PRIMARY KEY(project_uuid,request_key)
);
CREATE TABLE project_pipeline_history (
 project_uuid UUID NOT NULL REFERENCES projects(uuid) ON DELETE RESTRICT,
 id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 kind TEXT NOT NULL,
 subject UUID,
 resource_version BIGINT NOT NULL,
 content JSONB NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 actor UUID NOT NULL
);
-- +goose StatementBegin
CREATE FUNCTION protect_project_pipeline_version() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' OR NEW.id IS DISTINCT FROM OLD.id OR NEW.project_uuid IS DISTINCT FROM OLD.project_uuid
  OR NEW.pipeline_id IS DISTINCT FROM OLD.pipeline_id OR NEW.version IS DISTINCT FROM OLD.version
  OR NEW.draft_resource_version IS DISTINCT FROM OLD.draft_resource_version
  OR NEW.name IS DISTINCT FROM OLD.name OR NEW.digest IS DISTINCT FROM OLD.digest
  OR NEW.definition IS DISTINCT FROM OLD.definition OR NEW.published_at IS DISTINCT FROM OLD.published_at
  OR NEW.published_by IS DISTINCT FROM OLD.published_by OR OLD.retired_at IS NOT NULL OR NEW.retired_at IS NULL THEN
  RAISE EXCEPTION 'published pipeline definitions are immutable and retained' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER project_pipeline_version_immutable BEFORE UPDATE OR DELETE ON project_pipeline_versions
 FOR EACH ROW EXECUTE FUNCTION protect_project_pipeline_version();
-- +goose StatementBegin
CREATE FUNCTION protect_project_pipeline_history() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 RAISE EXCEPTION 'pipeline history is immutable and retained' USING ERRCODE='23514';
END $$;
-- +goose StatementEnd
CREATE TRIGGER project_pipeline_history_immutable BEFORE UPDATE OR DELETE ON project_pipeline_history
 FOR EACH ROW EXECUTE FUNCTION protect_project_pipeline_history();
CREATE TRIGGER project_pipeline_publication_immutable BEFORE UPDATE OR DELETE ON project_pipeline_publications
 FOR EACH ROW EXECUTE FUNCTION protect_project_pipeline_history();
-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM project_pipeline_history) THEN
  RAISE EXCEPTION 'cannot downgrade while pipeline history exists';
 END IF;
END $$;
-- +goose StatementEnd
DROP TABLE project_pipeline_history;
DROP TABLE project_pipeline_publications;
DROP FUNCTION protect_project_pipeline_history();
DROP TABLE project_pipeline_policy;
DROP TABLE project_pipeline_versions;
DROP FUNCTION protect_project_pipeline_version();
DROP TABLE project_pipeline_drafts;
