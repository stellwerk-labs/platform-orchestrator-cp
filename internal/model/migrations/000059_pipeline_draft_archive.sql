-- +goose Up
-- +goose StatementBegin
DO $$ BEGIN
 IF to_regclass('project_pipeline_drafts') IS NULL THEN
  RAISE EXCEPTION 'pipeline archive upgrade requires the pipeline draft schema';
 END IF;
 IF EXISTS(SELECT 1 FROM pg_attribute WHERE attrelid='project_pipeline_drafts'::regclass AND NOT attisdropped
  AND ((attname='archived_at' AND (atttypid<>'timestamptz'::regtype OR attnotnull)) OR (attname='archived_by' AND (atttypid<>'uuid'::regtype OR attnotnull)))) THEN
  RAISE EXCEPTION 'unexpected existing pipeline archive column schema';
 END IF;
 IF EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='project_pipeline_drafts'::regclass AND conname='pipeline_archive_actor'
  AND regexp_replace(pg_get_expr(conbin,conrelid),'[[:space:]()]','','g')<>'archived_atISNULL=archived_byISNULL') THEN
  RAISE EXCEPTION 'unexpected existing pipeline archive constraint schema';
 END IF;
 IF to_regclass('project_pipeline_drafts_active_slug') IS NOT NULL AND NOT EXISTS(
  SELECT 1 FROM pg_index WHERE indexrelid=to_regclass('project_pipeline_drafts_active_slug')
  AND indrelid='project_pipeline_drafts'::regclass AND indisunique AND indnkeyatts=2
  AND pg_get_indexdef(indexrelid,1,true)='project_uuid' AND pg_get_indexdef(indexrelid,2,true)='slug'
  AND regexp_replace(pg_get_expr(indpred,indrelid),'[[:space:]()]','','g')='archived_atISNULL') THEN
  RAISE EXCEPTION 'unexpected existing pipeline archive index schema';
 END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE project_pipeline_drafts ADD COLUMN IF NOT EXISTS archived_at TIMESTAMPTZ;
ALTER TABLE project_pipeline_drafts ADD COLUMN IF NOT EXISTS archived_by UUID;
-- +goose StatementBegin
DO $$ BEGIN
 IF NOT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='project_pipeline_drafts'::regclass AND conname='pipeline_archive_actor') THEN
  ALTER TABLE project_pipeline_drafts ADD CONSTRAINT pipeline_archive_actor CHECK ((archived_at IS NULL)=(archived_by IS NULL));
 END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE project_pipeline_drafts DROP CONSTRAINT IF EXISTS project_pipeline_drafts_project_uuid_slug_key;
CREATE UNIQUE INDEX IF NOT EXISTS project_pipeline_drafts_active_slug ON project_pipeline_drafts(project_uuid,slug) WHERE archived_at IS NULL;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM project_pipeline_drafts WHERE archived_at IS NOT NULL) THEN
  RAISE EXCEPTION 'cannot downgrade while archived pipeline drafts exist';
 END IF;
END $$;
-- +goose StatementEnd
DROP INDEX project_pipeline_drafts_active_slug;
ALTER TABLE project_pipeline_drafts ADD UNIQUE(project_uuid,slug);
ALTER TABLE project_pipeline_drafts DROP CONSTRAINT pipeline_archive_actor;
ALTER TABLE project_pipeline_drafts DROP COLUMN archived_by;
ALTER TABLE project_pipeline_drafts DROP COLUMN archived_at;
