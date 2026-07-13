-- +goose Up
CREATE TYPE org_plan AS ENUM ('custom');

ALTER TABLE orgs
    ADD COLUMN IF NOT EXISTS plan org_plan DEFAULT 'custom' NOT NULL,
    ADD COLUMN IF NOT EXISTS created_by UUID DEFAULT 'ffffffff-ffff-ffff-ffff-ffffffffffff' NOT NULL;

CREATE INDEX orgs_created_by_idx ON orgs (created_by);

-- +goose Down
ALTER TABLE orgs
    DROP COLUMN IF EXISTS plan,
    DROP COLUMN IF EXISTS created_by;

DROP INDEX IF EXISTS orgs_created_by_idx;
DROP TYPE org_plan;
