-- +goose Up
CREATE TABLE environment_observation_grants (
    org_id TEXT NOT NULL REFERENCES orgs(id) ON DELETE RESTRICT,
    plugin_id TEXT NOT NULL,
    grant_id UUID NOT NULL,
    actor_id UUID NOT NULL,
    source_operation TEXT NOT NULL,
    last_effect_request_id TEXT NOT NULL,
    revision BIGINT NOT NULL CHECK (revision > 0),
    state TEXT NOT NULL CHECK (state IN ('active','revoked')),
    targets JSONB NOT NULL,
    expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id,plugin_id,grant_id)
);
CREATE TABLE environment_observation_grant_commands (
    org_id TEXT NOT NULL,
    plugin_id TEXT NOT NULL,
    grant_id UUID NOT NULL,
    request_id TEXT NOT NULL,
    actor_id UUID NOT NULL,
    request_digest TEXT NOT NULL,
    reason TEXT NOT NULL,
    response JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id,plugin_id,grant_id,request_id),
    UNIQUE (org_id,plugin_id,actor_id,request_id),
    FOREIGN KEY (org_id,plugin_id,grant_id) REFERENCES environment_observation_grants(org_id,plugin_id,grant_id)
);
CREATE TABLE environment_observation_requests (
    org_id TEXT NOT NULL,
    plugin_id TEXT NOT NULL,
    request_id UUID NOT NULL,
    grant_id UUID NOT NULL,
    grant_revision BIGINT NOT NULL,
    project_uuid UUID NOT NULL,
    environment_uuid UUID NOT NULL,
    request_digest TEXT NOT NULL,
    actor_id UUID NOT NULL,
    request_body JSONB NOT NULL,
    response JSONB,
    response_status INTEGER,
    run_id UUID,
    admission_deadline TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id,plugin_id,request_id),
    FOREIGN KEY (org_id,plugin_id,grant_id) REFERENCES environment_observation_grants(org_id,plugin_id,grant_id)
);
CREATE UNIQUE INDEX environment_observation_run ON environment_observation_requests(org_id,plugin_id,run_id) WHERE run_id IS NOT NULL;
CREATE TABLE environment_observation_redeployments (
    org_id TEXT NOT NULL,
    plugin_id TEXT NOT NULL,
    request_id UUID NOT NULL,
    observation_request_id UUID NOT NULL,
    run_id UUID NOT NULL,
    actor_id UUID NOT NULL,
    request_digest TEXT NOT NULL,
    request_body JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id,plugin_id,request_id),
    FOREIGN KEY (org_id,plugin_id,observation_request_id) REFERENCES environment_observation_requests(org_id,plugin_id,request_id)
);
-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM environment_observation_redeployments)
       OR EXISTS (SELECT 1 FROM environment_observation_grants WHERE state='active')
       OR EXISTS (SELECT 1 FROM environment_observation_requests WHERE response IS NULL OR run_id IS NOT NULL) THEN
       RAISE EXCEPTION 'cannot downgrade while observation authority or retained runs exist';
    END IF;
END $$;
-- +goose StatementEnd
DROP TABLE environment_observation_redeployments;
DROP TABLE environment_observation_requests;
DROP TABLE environment_observation_grant_commands;
DROP TABLE environment_observation_grants;
