-- +goose Up
CREATE TABLE plugin_entitlement_grants (
    issuer TEXT NOT NULL,
    issuance_id UUID NOT NULL,
    grant_sequence BIGINT NOT NULL CHECK (grant_sequence > 0),
    installation_uuid UUID NOT NULL,
    plugin_id TEXT NOT NULL,
    organization_uuid UUID NOT NULL REFERENCES orgs(uuid) ON DELETE RESTRICT,
    cose BYTEA NOT NULL CHECK (octet_length(cose) BETWEEN 1 AND 65536),
    digest BYTEA NOT NULL CHECK (octet_length(digest) = 32),
    key_id BYTEA NOT NULL CHECK (octet_length(key_id) BETWEEN 1 AND 64),
    features TEXT[] NOT NULL,
    limits JSONB NOT NULL,
    issued_at TIMESTAMPTZ NOT NULL,
    not_before TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    grace_until TIMESTAMPTZ NOT NULL,
    trust_class TEXT NOT NULL CHECK (trust_class IN ('production', 'development')),
    importer TEXT NOT NULL,
    source_digest BYTEA NOT NULL CHECK (octet_length(source_digest) = 32),
    imported_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (issuer, issuance_id),
    UNIQUE (issuer, installation_uuid, plugin_id, organization_uuid, grant_sequence),
    CHECK (issued_at <= not_before AND not_before < expires_at AND expires_at <= grace_until),
    CHECK (cardinality(features) BETWEEN 1 AND 64),
    CHECK (jsonb_typeof(limits) = 'object')
);

CREATE TABLE plugin_entitlement_bindings (
    installation_uuid UUID NOT NULL,
    plugin_id TEXT NOT NULL,
    organization_uuid UUID NOT NULL REFERENCES orgs(uuid) ON DELETE RESTRICT,
    issuer TEXT NOT NULL,
    issuance_id UUID NOT NULL,
    grant_sequence BIGINT NOT NULL CHECK (grant_sequence > 0),
    evaluated_through TIMESTAMPTZ,
    observed_grace_at TIMESTAMPTZ,
    observed_expired_at TIMESTAMPTZ,
    revision BIGINT NOT NULL CHECK (revision > 0),
    PRIMARY KEY (installation_uuid, plugin_id, organization_uuid),
    FOREIGN KEY (issuer, issuance_id) REFERENCES plugin_entitlement_grants(issuer, issuance_id) ON DELETE RESTRICT
);

CREATE TABLE plugin_entitlement_revocation_snapshots (
    issuer TEXT NOT NULL,
    revocation_sequence BIGINT NOT NULL CHECK (revocation_sequence > 0),
    cose BYTEA NOT NULL CHECK (octet_length(cose) BETWEEN 1 AND 65536),
    digest BYTEA NOT NULL CHECK (octet_length(digest) = 32),
    key_id BYTEA NOT NULL CHECK (octet_length(key_id) BETWEEN 1 AND 64),
    issued_at TIMESTAMPTZ NOT NULL,
    trust_class TEXT NOT NULL CHECK (trust_class IN ('production', 'development')),
    importer TEXT NOT NULL,
    source_digest BYTEA NOT NULL CHECK (octet_length(source_digest) = 32),
    imported_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (issuer, revocation_sequence)
);

CREATE TABLE plugin_entitlement_revocations (
    issuer TEXT NOT NULL,
    issuance_id UUID NOT NULL,
    first_snapshot_sequence BIGINT NOT NULL,
    revoked_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (issuer, issuance_id),
    FOREIGN KEY (issuer, first_snapshot_sequence) REFERENCES plugin_entitlement_revocation_snapshots(issuer, revocation_sequence) ON DELETE RESTRICT
);

CREATE TABLE plugin_entitlement_feature_activations (
    installation_uuid UUID NOT NULL,
    plugin_id TEXT NOT NULL,
    organization_uuid UUID NOT NULL REFERENCES orgs(uuid) ON DELETE RESTRICT,
    feature TEXT NOT NULL,
    first_issuer TEXT NOT NULL,
    first_issuance_id UUID NOT NULL,
    last_issuer TEXT NOT NULL,
    last_issuance_id UUID NOT NULL,
    first_activated_at TIMESTAMPTZ NOT NULL,
    last_activated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (installation_uuid, plugin_id, organization_uuid, feature),
    FOREIGN KEY (first_issuer, first_issuance_id) REFERENCES plugin_entitlement_grants(issuer, issuance_id) ON DELETE RESTRICT,
    FOREIGN KEY (last_issuer, last_issuance_id) REFERENCES plugin_entitlement_grants(issuer, issuance_id) ON DELETE RESTRICT
);

CREATE TABLE plugin_entitlement_audit_events (
    event_id UUID PRIMARY KEY,
    issuer TEXT NOT NULL,
    installation_uuid UUID,
    plugin_id TEXT,
    organization_uuid UUID,
    kind TEXT NOT NULL CHECK (kind IN ('grant_imported', 'revocations_imported', 'feature_activated', 'grace_observed', 'expiry_observed')),
    sequence BIGINT NOT NULL CHECK (sequence > 0),
    trust_class TEXT NOT NULL CHECK (trust_class IN ('production', 'development')),
    importer TEXT NOT NULL,
    source_digest BYTEA NOT NULL CHECK (octet_length(source_digest) = 32),
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM plugin_entitlement_grants)
       OR EXISTS (SELECT 1 FROM plugin_entitlement_bindings)
       OR EXISTS (SELECT 1 FROM plugin_entitlement_revocation_snapshots)
       OR EXISTS (SELECT 1 FROM plugin_entitlement_revocations)
       OR EXISTS (SELECT 1 FROM plugin_entitlement_feature_activations)
       OR EXISTS (SELECT 1 FROM plugin_entitlement_audit_events) THEN
        RAISE EXCEPTION 'cannot discard plugin entitlement evidence';
    END IF;
END $$;
-- +goose StatementEnd
DROP TABLE plugin_entitlement_audit_events;
DROP TABLE plugin_entitlement_feature_activations;
DROP TABLE plugin_entitlement_revocations;
DROP TABLE plugin_entitlement_revocation_snapshots;
DROP TABLE plugin_entitlement_bindings;
DROP TABLE plugin_entitlement_grants;
