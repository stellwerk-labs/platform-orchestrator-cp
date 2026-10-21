-- +goose Up
-- Existing entries receive the migration time, so they are kept for at least
-- one full retention window after the upgrade. New entries record their own
-- insertion time.
ALTER TABLE extension_resource_events ADD COLUMN recorded_at TIMESTAMPTZ NOT NULL DEFAULT now();
ALTER TABLE extension_resource_events ALTER COLUMN recorded_at SET DEFAULT clock_timestamp();

-- A consumer acknowledges a cursor by presenting it. Acknowledgements only
-- move forward and bound retention.
CREATE TABLE extension_resource_event_consumers (
    org_id TEXT NOT NULL,
    plugin_id TEXT NOT NULL,
    operation_id UUID NOT NULL,
    acknowledged_cursor BIGINT NOT NULL CHECK (acknowledged_cursor >= 0),
    acknowledged_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, plugin_id, operation_id),
    FOREIGN KEY (org_id, plugin_id, operation_id)
        REFERENCES external_operation_grants(org_id, plugin_id, operation_id) ON DELETE RESTRICT
);

-- Entries at or below pruned_through were removed by the retention policy.
-- Readers presenting an older cursor receive an explicit expiry, never a gap.
CREATE TABLE extension_resource_event_retention (
    org_id TEXT PRIMARY KEY REFERENCES orgs(id) ON DELETE RESTRICT,
    pruned_through BIGINT NOT NULL CHECK (pruned_through > 0),
    pruned_at TIMESTAMPTZ NOT NULL
);

-- +goose Down
-- Pruning is a runtime retention policy, never part of a schema downgrade.
-- After any pruning the previous application could not report expired
-- cursors, so this downgrade refuses instead of hiding the removed history.
-- +goose StatementBegin
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM extension_resource_event_retention) THEN
        RAISE EXCEPTION 'cannot remove extension event retention evidence after pruning';
    END IF;
END $$;
-- +goose StatementEnd
DROP TABLE extension_resource_event_retention;
DROP TABLE extension_resource_event_consumers;
ALTER TABLE extension_resource_events DROP COLUMN recorded_at;
