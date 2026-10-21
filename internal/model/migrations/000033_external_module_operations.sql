-- +goose Up
-- An earlier schema may lack these released Core columns; add them if missing.
ALTER TABLE resource_types ADD COLUMN IF NOT EXISTS module_contract JSONB;
ALTER TABLE definition_versions ADD COLUMN IF NOT EXISTS output_schema JSONB;

CREATE TABLE external_module_operations (
    org_id TEXT NOT NULL REFERENCES orgs(id) ON DELETE RESTRICT,
    plugin_id TEXT NOT NULL,
    operation_id UUID NOT NULL,
    fence BIGINT NOT NULL CHECK (fence > 0),
    state TEXT NOT NULL CHECK (state IN ('active', 'released')),
    module_uuids UUID[] NOT NULL CHECK (cardinality(module_uuids) > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, plugin_id, operation_id)
);

CREATE TABLE external_module_operation_receipts (
    org_id TEXT NOT NULL,
    plugin_id TEXT NOT NULL,
    operation_id UUID NOT NULL,
    request_id TEXT NOT NULL,
    request_digest TEXT NOT NULL,
    actor_id UUID NOT NULL,
    action TEXT NOT NULL,
    response JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, plugin_id, operation_id, request_id),
    FOREIGN KEY (org_id, plugin_id, operation_id)
        REFERENCES external_module_operations(org_id, plugin_id, operation_id)
        ON DELETE RESTRICT
);

-- +goose Down
DROP TABLE external_module_operation_receipts;
DROP TABLE external_module_operations;
-- Preserve additive Core declaration columns on application/schema rollback.
