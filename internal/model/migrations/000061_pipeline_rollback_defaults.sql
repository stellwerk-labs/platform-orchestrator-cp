-- +goose Up
ALTER TABLE project_pipeline_policy ADD COLUMN rollback_defaults JSONB NOT NULL DEFAULT '{}'::jsonb;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM project_pipeline_policy WHERE rollback_defaults<>'{}'::jsonb) THEN
  RAISE EXCEPTION 'cannot discard rollback pipeline default preferences';
 END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE project_pipeline_policy DROP COLUMN rollback_defaults;
