-- +goose Up
ALTER TABLE envs ADD COLUMN native_first_eligible boolean NOT NULL DEFAULT false;
ALTER TABLE envs ADD CONSTRAINT native_first_runner_required CHECK (NOT native_first_eligible OR runner_id IS NOT NULL);

CREATE INDEX deployment_admission_guard_custody ON deployment_admission_guards (org_id,environment_uuid,epoch)
    WHERE outcome IS DISTINCT FROM 'absent';

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM envs WHERE native_first_eligible) THEN
        RAISE EXCEPTION 'cannot discard Native-first Environment eligibility';
    END IF;
END $$;
-- +goose StatementEnd
DROP INDEX deployment_admission_guard_custody;
ALTER TABLE envs DROP CONSTRAINT native_first_runner_required;
ALTER TABLE envs DROP COLUMN native_first_eligible;
