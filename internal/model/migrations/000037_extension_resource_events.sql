-- +goose Up
CREATE TABLE extension_resource_event_heads (
    org_id TEXT PRIMARY KEY REFERENCES orgs(id) ON DELETE RESTRICT,
    cursor BIGINT NOT NULL CHECK (cursor >= 0)
);
CREATE TABLE extension_resource_events (
    org_id TEXT NOT NULL REFERENCES orgs(id) ON DELETE RESTRICT,
    cursor BIGINT NOT NULL,
    event_id UUID NOT NULL UNIQUE,
    environment_uuid UUID,
    module_uuid UUID,
    event JSONB NOT NULL,
    PRIMARY KEY (org_id, cursor)
);
CREATE INDEX extension_resource_events_environment ON extension_resource_events(org_id, environment_uuid, cursor);
CREATE INDEX extension_resource_events_module ON extension_resource_events(org_id, module_uuid, cursor);
CREATE TABLE extension_published_events (
    org_id TEXT NOT NULL,
    plugin_id TEXT NOT NULL,
    operation_id UUID NOT NULL,
    event_id UUID NOT NULL,
    request JSONB NOT NULL,
    published_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, plugin_id, event_id),
    FOREIGN KEY (org_id, plugin_id, operation_id) REFERENCES external_operation_grants(org_id, plugin_id, operation_id) ON DELETE RESTRICT
);

-- +goose Down
-- Application rollback leaves this additive journal intact. A schema downgrade
-- cannot discard delivered cursor history or publication deduplication receipts.
-- +goose StatementBegin
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM extension_resource_events) OR EXISTS (SELECT 1 FROM extension_published_events) THEN
        RAISE EXCEPTION 'cannot discard extension event history';
    END IF;
END $$;
-- +goose StatementEnd
DROP TABLE extension_published_events;
DROP TABLE extension_resource_events;
DROP TABLE extension_resource_event_heads;
