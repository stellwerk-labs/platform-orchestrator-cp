-- +goose Up
ALTER TABLE project_pipeline_policy ADD COLUMN environment_default_version_ids JSONB NOT NULL DEFAULT '{}'::jsonb;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM project_pipeline_policy WHERE environment_default_version_ids<>'{}'::jsonb) THEN
  RAISE EXCEPTION 'cannot discard environment pipeline default preferences';
 END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE project_pipeline_policy DROP COLUMN environment_default_version_ids;
