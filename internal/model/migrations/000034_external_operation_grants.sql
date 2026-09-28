-- +goose Up
CREATE TABLE external_operation_grants (
    org_id TEXT NOT NULL REFERENCES orgs(id) ON DELETE RESTRICT,
    plugin_id TEXT NOT NULL,
    operation_id UUID NOT NULL,
    actor_id UUID NOT NULL,
    source_operation TEXT NOT NULL,
    intent JSONB NOT NULL,
    baseline_defaults JSONB NOT NULL,
    revision BIGINT NOT NULL CHECK (revision > 0),
    state TEXT NOT NULL CHECK (state IN ('prepared', 'active', 'suspended', 'released')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, plugin_id, operation_id)
);

CREATE TABLE external_operation_grant_commands (
    org_id TEXT NOT NULL,
    plugin_id TEXT NOT NULL,
    operation_id UUID NOT NULL,
    request_id TEXT NOT NULL,
    actor_id UUID NOT NULL,
    request_digest TEXT NOT NULL,
    reason TEXT NOT NULL,
    response JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, plugin_id, operation_id, request_id),
    UNIQUE (org_id, plugin_id, actor_id, request_id),
    FOREIGN KEY (org_id, plugin_id, operation_id)
        REFERENCES external_operation_grants(org_id, plugin_id, operation_id) ON DELETE RESTRICT
);

CREATE TABLE external_operation_dispatches (
    org_id TEXT NOT NULL,
    plugin_id TEXT NOT NULL,
    operation_id UUID NOT NULL,
    dispatch_id TEXT NOT NULL,
    fence BIGINT NOT NULL CHECK (fence > 0),
    action TEXT NOT NULL,
    actor_id UUID NOT NULL,
    request_digest TEXT NOT NULL,
    intent JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, plugin_id, operation_id, dispatch_id),
    FOREIGN KEY (org_id, plugin_id, operation_id)
        REFERENCES external_operation_grants(org_id, plugin_id, operation_id) ON DELETE RESTRICT
);

-- +goose Down
-- Explicit metadata downgrade only; application recovery keeps these tables.
DROP TABLE external_operation_dispatches;
DROP TABLE external_operation_grant_commands;
DROP TABLE external_operation_grants;
