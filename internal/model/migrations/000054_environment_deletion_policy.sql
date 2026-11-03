-- +goose Up
ALTER TABLE envs ADD COLUMN deletion_force boolean, ADD COLUMN deletion_delete_rules boolean;
ALTER TABLE envs ADD CONSTRAINT envs_deletion_policy_pair CHECK ((deletion_force IS NULL) = (deletion_delete_rules IS NULL));

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM envs WHERE deletion_force IS NOT NULL) THEN
        RAISE EXCEPTION 'cannot discard accepted Environment deletion policy';
    END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE envs DROP CONSTRAINT envs_deletion_policy_pair;
ALTER TABLE envs DROP COLUMN deletion_force, DROP COLUMN deletion_delete_rules;
