-- +goose Up
-- Whether Core currently journals an organization, and the cursor of the
-- latest gap. Changes made while journaling was paused are not in the journal,
-- so every cursor issued before gap_cursor predates them and expires.
CREATE TABLE extension_resource_event_journal_state (
    org_id TEXT PRIMARY KEY REFERENCES orgs(id) ON DELETE RESTRICT,
    journaling BOOLEAN NOT NULL,
    gap_cursor BIGINT NOT NULL DEFAULT 0 CHECK (gap_cursor >= 0),
    changed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Existing journals were written while Core journaled every organization.
-- Startup reconciliation pauses the ones without a subscriber.
INSERT INTO extension_resource_event_journal_state (org_id, journaling)
SELECT org_id, true FROM extension_resource_event_heads;

-- +goose Down
-- The previous application cannot report a pause or a gap. Once either was
-- recorded, a downgrade would let stale cursors silently skip changes.
-- +goose StatementBegin
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM extension_resource_event_journal_state WHERE NOT journaling OR gap_cursor > 0) THEN
        RAISE EXCEPTION 'cannot remove recorded extension journal pauses or gaps';
    END IF;
END $$;
-- +goose StatementEnd
DROP TABLE extension_resource_event_journal_state;
