-- +goose Up
CREATE TABLE environment_observation_closures (
    org_id TEXT NOT NULL REFERENCES orgs(id) ON DELETE RESTRICT,
    plugin_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('observation','redeployment')),
    request_id UUID NOT NULL,
    request_digest TEXT NOT NULL,
    request_body JSONB NOT NULL,
    run_id UUID,
    original_actor_id UUID,
    service_actor_id UUID NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('close_requested','accepted','not_started')),
    response JSONB,
    closed_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    resolved_at TIMESTAMPTZ,
    PRIMARY KEY (org_id,plugin_id,kind,request_id),
    CHECK ((kind='observation' AND run_id IS NULL) OR (kind='redeployment' AND run_id IS NOT NULL)),
    CHECK ((state='close_requested' AND response IS NULL AND resolved_at IS NULL)
        OR (state<>'close_requested' AND response IS NOT NULL AND resolved_at IS NOT NULL))
);
-- The SQL guard also fences already-running older Core binaries whose journal
-- INSERT does not know about the closure table. The compact JSON key matches
-- lockExternalGrant's existing Go encoding, including its HTML/rune escaping.
-- +goose StatementBegin
CREATE FUNCTION environment_observation_admission_lock(target_org text, target_plugin text, admission_kind text, target_request uuid)
RETURNS void LANGUAGE plpgsql VOLATILE AS $$
DECLARE
    identity text;
BEGIN
    -- A frozen transaction snapshot could miss a closure committed while this
    -- statement waits for its advisory lock. Existing Core admission uses RC.
    IF current_setting('transaction_isolation') NOT IN ('read committed','read uncommitted') THEN
        RAISE EXCEPTION USING ERRCODE='23514',
            MESSAGE='observation admission requires read committed visibility',
            CONSTRAINT='environment_observation_admission_isolation';
    END IF;
    identity := array_to_json(ARRAY[target_org,'observation-admission:'||admission_kind||':'||target_plugin,target_request::text])::text;
    identity := replace(identity,'<','\u003c');
    identity := replace(identity,'>','\u003e');
    identity := replace(identity,'&','\u0026');
    identity := replace(identity,U&'\2028','\u2028');
    identity := replace(identity,U&'\2029','\u2029');
    PERFORM pg_advisory_xact_lock(hashtextextended(identity,1));
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION environment_observation_journal_insert_guard() RETURNS trigger LANGUAGE plpgsql VOLATILE AS $$
BEGIN
    PERFORM environment_observation_admission_lock(NEW.org_id,NEW.plugin_id,TG_ARGV[0],NEW.request_id);
    IF EXISTS (
        SELECT 1 FROM environment_observation_closures
        WHERE org_id=NEW.org_id AND plugin_id=NEW.plugin_id AND kind=TG_ARGV[0] AND request_id=NEW.request_id
    ) THEN
        RAISE EXCEPTION USING ERRCODE='23514',
            MESSAGE='observation admission is permanently closed',
            CONSTRAINT='environment_observation_admission_closed';
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER environment_observation_requests_no_closed_insert
BEFORE INSERT ON environment_observation_requests FOR EACH ROW
EXECUTE FUNCTION environment_observation_journal_insert_guard('observation');
CREATE TRIGGER environment_observation_redeployments_no_closed_insert
BEFORE INSERT ON environment_observation_redeployments FOR EACH ROW
EXECUTE FUNCTION environment_observation_journal_insert_guard('redeployment');
CREATE TABLE environment_observation_grant_targets (
    org_id TEXT NOT NULL,
    plugin_id TEXT NOT NULL,
    grant_id UUID NOT NULL,
    revision BIGINT NOT NULL CHECK (revision > 0),
    project_uuid UUID NOT NULL,
    environment_uuid UUID NOT NULL,
    PRIMARY KEY (org_id,plugin_id,grant_id,revision,project_uuid,environment_uuid),
    FOREIGN KEY (org_id,plugin_id,grant_id) REFERENCES environment_observation_grants(org_id,plugin_id,grant_id) ON DELETE RESTRICT
);
INSERT INTO environment_observation_grant_targets
SELECT c.org_id,c.plugin_id,c.grant_id,(c.response->>'revision')::bigint,
       (target->>'project_id')::uuid,(target->>'environment_id')::uuid
FROM environment_observation_grant_commands c
CROSS JOIN LATERAL jsonb_array_elements(c.response->'targets') target
WHERE c.response->>'state'='active'
ON CONFLICT DO NOTHING;
-- +goose StatementBegin
DO $$ BEGIN
    IF EXISTS (
        SELECT 1 FROM environment_observation_grants g
        WHERE g.state='active' AND NOT EXISTS (
            SELECT 1 FROM environment_observation_grant_commands c
            WHERE c.org_id=g.org_id AND c.plugin_id=g.plugin_id AND c.grant_id=g.grant_id
              AND (c.response->>'revision')::bigint=g.revision
              AND c.response->>'state'='active' AND c.response->'targets'=g.targets
        )
    ) THEN
        RAISE EXCEPTION 'active observation scope has no matching immutable issued receipt';
    END IF;
END $$;
-- +goose StatementEnd
-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM environment_observation_closures) THEN
        RAISE EXCEPTION 'cannot discard immutable observation no-forward tombstones';
    END IF;
END $$;
-- +goose StatementEnd
DROP TRIGGER environment_observation_redeployments_no_closed_insert ON environment_observation_redeployments;
DROP TRIGGER environment_observation_requests_no_closed_insert ON environment_observation_requests;
DROP FUNCTION environment_observation_journal_insert_guard();
DROP FUNCTION environment_observation_admission_lock(text,text,text,uuid);
DROP TABLE environment_observation_grant_targets;
DROP TABLE environment_observation_closures;
