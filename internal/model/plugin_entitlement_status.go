package model

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/entitlements"
)

// storedGrant is the grant Core stored as the current one of a binding.
type storedGrant struct {
	covering              entitlements.Binding
	issuer                string
	issuanceID            uuid.UUID
	sequence              int64
	envelope, digest      []byte
	trustClass            string
	expiresAt, graceUntil time.Time
}

// trusts reports whether verified, what the current trust and package make of
// the stored envelope, is exactly the grant Core stored for the binding.
func (s storedGrant) trusts(verified entitlements.Grant, err error) bool {
	return err == nil && verified.Issuer == s.issuer && verified.IssuanceID == s.issuanceID && verified.Sequence == s.sequence &&
		verified.InstallationID == s.covering.InstallationID && verified.OrganizationID == s.covering.OrganizationID && verified.PluginID == s.covering.PluginID &&
		string(verified.TrustClass) == s.trustClass && bytes.Equal(verified.Digest[:], s.digest) && bytes.Equal(verified.Envelope, s.envelope) &&
		verified.ExpiresAt == s.expiresAt.Unix() && verified.GraceUntil == s.graceUntil.Unix()
}

// grantState is the licence state of a stored grant at effectiveNow.
func grantState(verified entitlements.Grant, trusted, revoked bool, effectiveNow time.Time) string {
	switch {
	case !trusted:
		return "untrusted"
	case revoked:
		return "revoked"
	default:
		return string(entitlements.EvaluateTime(verified, effectiveNow.Unix()))
	}
}

// PluginEntitlementGrantStatus is the operator's view of the grant a binding
// holds: which grant it is, what it allows and its state now (valid, grace,
// expired, not_yet_valid, revoked or untrusted).
type PluginEntitlementGrantStatus struct {
	Scope      string    `json:"scope"`
	Issuer     string    `json:"issuer"`
	IssuanceID uuid.UUID `json:"issuance_id"`
	Sequence   int64     `json:"sequence"`
	Features   []string  `json:"features"`
	// OrganizationLimit caps the enabled organizations of the installation;
	// zero for none. Only an installation-wide grant carries one.
	OrganizationLimit int64     `json:"organization_limit,omitempty"`
	TrustClass        string    `json:"trust_class"`
	NotBefore         time.Time `json:"not_before"`
	ExpiresAt         time.Time `json:"expires_at"`
	GraceUntil        time.Time `json:"grace_until"`
	State             string    `json:"state"`
}

// PluginGrantVerifierFor re-verifies a stored envelope for a binding, as
// decisions do, without I/O.
type PluginGrantVerifierFor func(raw []byte, binding entitlements.Binding, now time.Time) (entitlements.Grant, error)

// PluginEntitlementGrantStatuses returns the current grant of every binding
// of the installation, keyed by organization ID, the installation-wide grant
// under "". It writes nothing; like a decision it never evaluates time
// before what decisions already observed.
func PluginEntitlementGrantStatuses(ctx context.Context, tx Tx, installation uuid.UUID, plugin string, now time.Time, verify PluginGrantVerifierFor) (map[string]PluginEntitlementGrantStatus, error) {
	rows, err := tx.QueryContext(ctx, `SELECT COALESCE(o.id,''), b.organization_uuid, b.issuer, b.issuance_id, b.grant_sequence, b.evaluated_through,
			g.cose, g.digest, g.features, g.limits, g.trust_class, g.not_before, g.expires_at, g.grace_until,
			EXISTS (SELECT 1 FROM plugin_entitlement_revocations r WHERE r.issuer=b.issuer AND r.issuance_id=b.issuance_id)
		FROM plugin_entitlement_bindings b JOIN plugin_entitlement_grants g ON g.issuer=b.issuer AND g.issuance_id=b.issuance_id
		LEFT JOIN orgs o ON o.uuid=b.organization_uuid
		WHERE b.installation_uuid=$1 AND b.plugin_id=$2`, installation, plugin)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	statuses := map[string]PluginEntitlementGrantStatus{}
	for rows.Next() {
		var org string
		var scope uuid.NullUUID
		var evaluated sql.NullTime
		var limits []byte
		var revoked bool
		stored := storedGrant{covering: entitlements.Binding{InstallationID: installation, PluginID: plugin}}
		status := PluginEntitlementGrantStatus{}
		if err := rows.Scan(&org, &scope, &stored.issuer, &stored.issuanceID, &stored.sequence, &evaluated, &stored.envelope, &stored.digest,
			pq.Array(&status.Features), &limits, &stored.trustClass, &status.NotBefore, &stored.expiresAt, &stored.graceUntil, &revoked); err != nil {
			return nil, err
		}
		var limit map[string]int64
		if err := json.Unmarshal(limits, &limit); err != nil {
			return nil, err
		}
		stored.covering.OrganizationID = scope.UUID
		status.Scope = PluginGrantScopeInstallation
		if scope.Valid {
			status.Scope = PluginGrantScopeOrganization
			if org == "" {
				return nil, errors.New("an entitlement binding names an organization that does not exist")
			}
		}
		status.Issuer, status.IssuanceID, status.Sequence, status.OrganizationLimit = stored.issuer, stored.issuanceID, stored.sequence, limit[entitlements.LimitOrganizations]
		status.TrustClass, status.ExpiresAt, status.GraceUntil = stored.trustClass, stored.expiresAt, stored.graceUntil
		effectiveNow := now.UTC().Truncate(time.Second)
		if evaluated.Valid && evaluated.Time.After(effectiveNow) {
			effectiveNow = evaluated.Time
		}
		verified, trustErr := verify(stored.envelope, stored.covering, effectiveNow)
		status.State = grantState(verified, stored.trusts(verified, trustErr), revoked, effectiveNow)
		statuses[org] = status
	}
	return statuses, rows.Err()
}

// PluginOrganizationLimit returns the organization limit of the
// installation's current installation-wide grant, whatever its state, or zero
// when it has none.
func PluginOrganizationLimit(ctx context.Context, tx Tx, installation uuid.UUID, plugin string) (int64, error) {
	var limit int64
	err := tx.QueryRowContext(ctx, `SELECT COALESCE((g.limits->>'organizations')::bigint, 0)
		FROM plugin_entitlement_bindings b JOIN plugin_entitlement_grants g ON g.issuer=b.issuer AND g.issuance_id=b.issuance_id
		WHERE b.installation_uuid=$1 AND b.plugin_id=$2 AND b.organization_uuid IS NULL`, installation, plugin).Scan(&limit)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return limit, err
}
