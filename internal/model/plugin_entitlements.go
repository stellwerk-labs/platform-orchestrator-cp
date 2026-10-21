package model

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stellwerk-labs/golib/hstandardoutbox"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/entitlements"
)

const entitlementEventSubject = "io.platform-orchestrator.plugin-entitlement.changed"

// Scopes of the grant that covers an organization. A current organization
// grant wins; an expired or revoked one can fall back to the installation.
const (
	PluginGrantScopeOrganization = "organization"
	PluginGrantScopeInstallation = "installation"
)

// organizationScope is the stored organization of a binding: NULL for the
// installation scope.
func organizationScope(organization uuid.UUID) uuid.NullUUID {
	return uuid.NullUUID{UUID: organization, Valid: organization != uuid.Nil}
}

// PluginEntitlementDecision is a bounded projection; signed bytes and issuance
// identifiers are intentionally absent from plugin-facing decisions.
type PluginEntitlementDecision struct {
	Feature string `json:"feature"`
	State   string `json:"state"`
	// Scope names the grant that covers the organization, empty without one.
	Scope      string                  `json:"scope,omitempty"`
	TrustClass entitlements.TrustClass `json:"trust_class,omitempty"`
	Forward    bool                    `json:"forward"`
	Read       bool                    `json:"read"`
	Safety     bool                    `json:"safety"`
	ExpiresAt  *time.Time              `json:"expires_at,omitempty"`
	GraceUntil *time.Time              `json:"grace_until,omitempty"`
	Revision   int64                   `json:"revision"`
}

// PluginEntitlementAccess contains the independent installation checks. The
// caller must also enforce IAM and operation/service grants.
type PluginEntitlementAccess struct {
	Enabled    bool
	Retained   bool
	Compatible bool
	Activate   bool
}

// PluginGrantVerifier re-verifies persisted COSE against the current local
// trust store and signed package declaration. It must do no I/O or network work.
type PluginGrantVerifier func([]byte, time.Time) (entitlements.Grant, error)

// LockPluginEntitlementImportTx fences a complete bundle, including rejected
// document savepoints, against decisions that may inspect both grant scopes.
// Imports must take it before all issuer/binding locks and before savepoints.
// Decision transactions must never upgrade their shared guard to an import.
func LockPluginEntitlementImportTx(ctx context.Context, tx Tx) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(640, 1)`)
	return err
}

func entitlementDecisionLock(ctx context.Context, tx Tx) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock_shared(640, 1)`)
	return err
}

func entitlementIssuerLock(ctx context.Context, tx Tx, issuer string, exclusive bool) error {
	query := `SELECT pg_advisory_xact_lock_shared(hashtextextended($1, 638))`
	if exclusive {
		query = `SELECT pg_advisory_xact_lock(hashtextextended($1, 638))`
	}
	_, err := tx.ExecContext(ctx, query, "plugin-entitlement-issuer:"+issuer)
	return err
}

func entitlementBindingLock(ctx context.Context, tx Tx, binding entitlements.Binding) error {
	key, _ := json.Marshal([]string{binding.InstallationID.String(), binding.PluginID, binding.OrganizationID.String()})
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 639))`, "plugin-entitlement-binding:"+string(key))
	return err
}

func entitlementAudit(ctx context.Context, db Databaser, tx Tx, issuer string, binding *entitlements.Binding, kind string, sequence int64, trustClass entitlements.TrustClass, importer string, sourceDigest [32]byte) error {
	var installation, plugin any
	var organization uuid.NullUUID
	if binding != nil {
		installation, organization, plugin = binding.InstallationID, organizationScope(binding.OrganizationID), binding.PluginID
	}
	eventID := uuid.New()
	if _, err := tx.ExecContext(ctx, `INSERT INTO plugin_entitlement_audit_events(event_id,issuer,installation_uuid,plugin_id,organization_uuid,kind,sequence,trust_class,importer,source_digest)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, eventID, issuer, installation, plugin, organization, kind, sequence, trustClass, importer, sourceDigest[:]); err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{"specversion": "1.0", "id": eventID, "type": entitlementEventSubject, "time": time.Now().UTC(), "data": map[string]any{"kind": kind, "trust_class": trustClass, "installation_uuid": installation, "plugin_id": plugin, "organization_uuid": organization}})
	if err != nil {
		return err
	}
	_, err = db.InsertPendingEventMessages(ctx, tx, []*hstandardoutbox.PendingEventMessage{{Subject: entitlementEventSubject, Payload: payload}})
	return err
}

// inTransaction runs work in the caller's transaction, or in its own one that
// commits only when work succeeds.
func inTransaction[T any](ctx context.Context, db Databaser, optionalTx Tx, work func(Tx) (T, error)) (T, error) {
	if optionalTx != nil {
		return work(optionalTx)
	}
	var zero T
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := work(tx)
	if err != nil {
		return zero, err
	}
	return result, tx.Commit()
}

func validEntitlementImport(envelope []byte, digest [32]byte, issuer, importer string, sourceDigest [32]byte, class entitlements.TrustClass) error {
	if issuer == "" || importer == "" || class != entitlements.Production && class != entitlements.Development || len(envelope) == 0 || len(envelope) > entitlements.MaxEnvelopeBytes || sha256.Sum256(envelope) != digest || sourceDigest == [32]byte{} {
		return NewErrBadRequest("verified entitlement evidence, importer and source digest are required")
	}
	return nil
}

// ImportPluginEntitlementGrant accepts only the result of the strict local
// verifier. The caller must exact-match its binding to the installed package.
// The binding is the plugin, installation and organization of an
// organization grant, or the plugin and installation of an installation-wide
// grant. Exact replay is idempotent; changed or lower sequences conflict. It
// runs in optionalTx, or in its own transaction when that is nil.
func ImportPluginEntitlementGrant(ctx context.Context, db Databaser, optionalTx Tx, grant entitlements.Grant, importer string, sourceDigest [32]byte) (bool, error) {
	if err := validEntitlementImport(grant.Envelope, grant.Digest, grant.Issuer, importer, sourceDigest, grant.TrustClass); err != nil {
		return false, err
	}
	if grant.InstallationID == uuid.Nil || grant.IssuanceID == uuid.Nil || grant.PluginID == "" || grant.Sequence <= 0 || len(grant.Features) == 0 || len(grant.KeyID) == 0 || len(grant.KeyID) > 64 || grant.OrganizationLimit < 0 || (grant.OrganizationLimit != 0 && !grant.InstallationWide()) || grant.IssuedAt > grant.NotBefore || grant.NotBefore >= grant.ExpiresAt || grant.ExpiresAt > grant.GraceUntil {
		return false, NewErrBadRequest("invalid verified grant projection")
	}
	binding := entitlements.Binding{InstallationID: grant.InstallationID, PluginID: grant.PluginID, OrganizationID: grant.OrganizationID}
	scope := organizationScope(grant.OrganizationID)
	return inTransaction(ctx, db, optionalTx, func(tx Tx) (bool, error) {
		var err error
		if err := LockPluginEntitlementImportTx(ctx, tx); err != nil {
			return false, err
		}
		if err := entitlementIssuerLock(ctx, tx, grant.Issuer, false); err != nil {
			return false, err
		}
		if err := entitlementBindingLock(ctx, tx, binding); err != nil {
			return false, err
		}
		var oldIssuer string
		var oldIssuance uuid.UUID
		var oldSequence int64
		err = tx.QueryRowContext(ctx, `SELECT issuer,issuance_id,grant_sequence FROM plugin_entitlement_bindings WHERE installation_uuid=$1 AND plugin_id=$2 AND organization_uuid IS NOT DISTINCT FROM $3 FOR UPDATE`, binding.InstallationID, binding.PluginID, scope).Scan(&oldIssuer, &oldIssuance, &oldSequence)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
		newBinding := errors.Is(err, sql.ErrNoRows)
		if err == nil {
			if oldIssuer != grant.Issuer {
				return false, NewErrConflict("entitlement issuer changed for an existing binding")
			}
			if grant.Sequence < oldSequence {
				return false, NewErrConflict("stale entitlement grant sequence")
			}
			if grant.Sequence == oldSequence {
				var oldDigest []byte
				if err := tx.QueryRowContext(ctx, `SELECT digest FROM plugin_entitlement_grants WHERE issuer=$1 AND issuance_id=$2`, oldIssuer, oldIssuance).Scan(&oldDigest); err != nil {
					return false, err
				}
				if oldIssuance != grant.IssuanceID || !bytes.Equal(oldDigest, grant.Digest[:]) {
					return false, NewErrConflict("changed entitlement at the same grant sequence")
				}
				return false, nil
			}
			if oldIssuance == grant.IssuanceID {
				return false, NewErrConflict("issuance ID cannot be reused at a higher sequence")
			}
		}
		limits := map[string]int64{}
		if grant.OrganizationLimit != 0 {
			limits[entitlements.LimitOrganizations] = grant.OrganizationLimit
		}
		encodedLimits, err := json.Marshal(limits)
		if err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO plugin_entitlement_grants(issuer,issuance_id,grant_sequence,installation_uuid,plugin_id,organization_uuid,cose,digest,key_id,features,limits,issued_at,not_before,expires_at,grace_until,trust_class,importer,source_digest)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`, grant.Issuer, grant.IssuanceID, grant.Sequence, grant.InstallationID, grant.PluginID, scope, grant.Envelope, grant.Digest[:], grant.KeyID, pq.Array(grant.Features), string(encodedLimits), time.Unix(grant.IssuedAt, 0).UTC(), time.Unix(grant.NotBefore, 0).UTC(), time.Unix(grant.ExpiresAt, 0).UTC(), time.Unix(grant.GraceUntil, 0).UTC(), grant.TrustClass, importer, sourceDigest[:]); err != nil {
			return false, err
		}
		if newBinding {
			_, err = tx.ExecContext(ctx, `INSERT INTO plugin_entitlement_bindings(installation_uuid,plugin_id,organization_uuid,issuer,issuance_id,grant_sequence,revision) VALUES($1,$2,$3,$4,$5,$6,1)`, binding.InstallationID, binding.PluginID, scope, grant.Issuer, grant.IssuanceID, grant.Sequence)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE plugin_entitlement_bindings SET issuance_id=$4,grant_sequence=$5,observed_grace_at=NULL,observed_expired_at=NULL,revision=revision+1 WHERE installation_uuid=$1 AND plugin_id=$2 AND organization_uuid IS NOT DISTINCT FROM $3`, binding.InstallationID, binding.PluginID, scope, grant.IssuanceID, grant.Sequence)
		}
		if err != nil {
			return false, err
		}
		if err := entitlementAudit(ctx, db, tx, grant.Issuer, &binding, "grant_imported", grant.Sequence, grant.TrustClass, importer, sourceDigest); err != nil {
			return false, err
		}
		return true, nil
	})
}

// ImportPluginEntitlementRevocations persists full signed snapshots and only
// adds revocation rows. The issuer exclusive lock fences all forward decisions.
func ImportPluginEntitlementRevocations(ctx context.Context, db Databaser, optionalTx Tx, snapshot entitlements.Revocations, importer string, sourceDigest [32]byte) (bool, error) {
	if err := validEntitlementImport(snapshot.Envelope, snapshot.Digest, snapshot.Issuer, importer, sourceDigest, snapshot.TrustClass); err != nil {
		return false, err
	}
	if snapshot.Sequence <= 0 || len(snapshot.KeyID) == 0 || len(snapshot.KeyID) > 64 {
		return false, NewErrBadRequest("invalid verified revocation projection")
	}
	return inTransaction(ctx, db, optionalTx, func(tx Tx) (bool, error) {
		var err error
		if err := LockPluginEntitlementImportTx(ctx, tx); err != nil {
			return false, err
		}
		if err := entitlementIssuerLock(ctx, tx, snapshot.Issuer, true); err != nil {
			return false, err
		}
		var priorSequence int64
		var priorDigest []byte
		err = tx.QueryRowContext(ctx, `SELECT revocation_sequence,digest FROM plugin_entitlement_revocation_snapshots WHERE issuer=$1 ORDER BY revocation_sequence DESC LIMIT 1`, snapshot.Issuer).Scan(&priorSequence, &priorDigest)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
		if err == nil {
			if snapshot.Sequence < priorSequence || snapshot.Sequence == priorSequence && !bytes.Equal(snapshot.Digest[:], priorDigest) {
				return false, NewErrConflict("stale or changed revocation sequence")
			}
			if snapshot.Sequence == priorSequence {
				return false, nil
			}
		}
		previous, err := tx.QueryContext(ctx, `SELECT issuance_id FROM plugin_entitlement_revocations WHERE issuer=$1`, snapshot.Issuer)
		if err != nil {
			return false, err
		}
		defer func() { _ = previous.Close() }()
		priorIDs := []uuid.UUID{}
		for previous.Next() {
			var id uuid.UUID
			if err := previous.Scan(&id); err != nil {
				_ = previous.Close()
				return false, err
			}
			priorIDs = append(priorIDs, id)
		}
		err = previous.Err()
		_ = previous.Close()
		if err != nil {
			return false, err
		}
		incoming := make(map[uuid.UUID]bool, len(snapshot.IssuanceIDs))
		for _, id := range snapshot.IssuanceIDs {
			if id == uuid.Nil || incoming[id] {
				return false, NewErrBadRequest("revocation IDs must be unique canonical UUIDs")
			}
			incoming[id] = true
		}
		for _, id := range priorIDs {
			if !incoming[id] {
				return false, NewErrConflict("revocation snapshot must include all previously revoked issuances")
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO plugin_entitlement_revocation_snapshots(issuer,revocation_sequence,cose,digest,key_id,issued_at,trust_class,importer,source_digest) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, snapshot.Issuer, snapshot.Sequence, snapshot.Envelope, snapshot.Digest[:], snapshot.KeyID, time.Unix(snapshot.IssuedAt, 0).UTC(), snapshot.TrustClass, importer, sourceDigest[:]); err != nil {
			return false, err
		}
		newIDs := make([]string, 0, len(snapshot.IssuanceIDs))
		for _, id := range snapshot.IssuanceIDs {
			var inserted uuid.UUID
			err := tx.QueryRowContext(ctx, `INSERT INTO plugin_entitlement_revocations(issuer,issuance_id,first_snapshot_sequence,revoked_at) VALUES($1,$2,$3,$4) ON CONFLICT (issuer,issuance_id) DO NOTHING RETURNING issuance_id`, snapshot.Issuer, id, snapshot.Sequence, time.Unix(snapshot.IssuedAt, 0).UTC()).Scan(&inserted)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return false, err
			}
			if err == nil {
				newIDs = append(newIDs, inserted.String())
			}
		}
		if len(newIDs) > 0 {
			rows, err := tx.QueryContext(ctx, `SELECT installation_uuid,plugin_id,organization_uuid FROM plugin_entitlement_bindings WHERE issuer=$1 AND issuance_id=ANY($2::uuid[]) ORDER BY installation_uuid::text COLLATE "C",plugin_id COLLATE "C",organization_uuid::text COLLATE "C"`, snapshot.Issuer, pq.Array(newIDs))
			if err != nil {
				return false, err
			}
			defer func() { _ = rows.Close() }()
			bindings := []entitlements.Binding{}
			for rows.Next() {
				var binding entitlements.Binding
				var organization uuid.NullUUID
				if err := rows.Scan(&binding.InstallationID, &binding.PluginID, &organization); err != nil {
					_ = rows.Close()
					return false, err
				}
				binding.OrganizationID = organization.UUID
				bindings = append(bindings, binding)
			}
			err = rows.Err()
			_ = rows.Close()
			if err != nil {
				return false, err
			}
			for _, binding := range bindings {
				if err := entitlementBindingLock(ctx, tx, binding); err != nil {
					return false, err
				}
				if _, err := tx.ExecContext(ctx, `UPDATE plugin_entitlement_bindings SET revision=revision+1 WHERE installation_uuid=$1 AND plugin_id=$2 AND organization_uuid IS NOT DISTINCT FROM $3`, binding.InstallationID, binding.PluginID, organizationScope(binding.OrganizationID)); err != nil {
					return false, err
				}
			}
		}
		if err := entitlementAudit(ctx, db, tx, snapshot.Issuer, nil, "revocations_imported", snapshot.Sequence, snapshot.TrustClass, importer, sourceDigest); err != nil {
			return false, err
		}
		return true, nil
	})
}

// DecidePluginEntitlementTx is the authoritative admission point for the
// organization of binding. A current organization grant takes precedence;
// an expired or revoked one falls back to a current installation-wide grant.
// Feature sets are never combined. Activations stay per organization.
// The caller owns a READ COMMITTED tx and commits its effect in that same
// transaction. A shared guard fences rare imports until the effect commits.
// All decisions take it before issuer/binding locks. The
// verifier reads only in-memory local trust and signed package metadata while
// locks are held. A failed trust check still permits historical read/safety
// activation.
func DecidePluginEntitlementTx(ctx context.Context, db Databaser, tx Tx, binding entitlements.Binding, feature string, access PluginEntitlementAccess, now time.Time, verify PluginGrantVerifier) (PluginEntitlementDecision, error) {
	decision := PluginEntitlementDecision{Feature: feature, State: "not_entitled"}
	if tx == nil || verify == nil || binding.InstallationID == uuid.Nil || binding.OrganizationID == uuid.Nil || binding.PluginID == "" || feature == "" || now.IsZero() {
		return decision, NewErrBadRequest("exact entitlement decision inputs and caller transaction are required")
	}
	if err := entitlementDecisionLock(ctx, tx); err != nil {
		return decision, err
	}
	inspect := access
	inspect.Activate = false
	scope := organizationScope(binding.OrganizationID)
	decision, err := decidePluginGrantTx(ctx, db, tx, binding, scope, feature, inspect, now, verify)
	if err != nil {
		return decision, err
	}
	if decision.Scope == "" || decision.State == string(entitlements.Expired) || decision.State == pluginGrantRevoked {
		wide, err := decidePluginGrantTx(ctx, db, tx, binding, uuid.NullUUID{}, feature, inspect, now, verify)
		if err != nil {
			return decision, err
		}
		if decision.Scope == "" || PluginEntitlementFallback(decision.State, wide.State) {
			decision, scope = wide, uuid.NullUUID{}
		}
	}
	if access.Activate {
		if !decision.Forward {
			return decision, NewErrConflict("feature cannot be activated without a current forward entitlement")
		}
		return decidePluginGrantTx(ctx, db, tx, binding, scope, feature, access, now, verify)
	}
	return decision, nil
}

func decidePluginGrantTx(ctx context.Context, db Databaser, tx Tx, binding entitlements.Binding, scope uuid.NullUUID, feature string, access PluginEntitlementAccess, now time.Time, verify PluginGrantVerifier) (PluginEntitlementDecision, error) {
	decision := PluginEntitlementDecision{Feature: feature, State: "not_entitled"}
	var issuer string
	err := tx.QueryRowContext(ctx, `SELECT issuer FROM plugin_entitlement_bindings WHERE installation_uuid=$1 AND plugin_id=$2
		AND organization_uuid IS NOT DISTINCT FROM $3`, binding.InstallationID, binding.PluginID, scope).Scan(&issuer)
	if errors.Is(err, sql.ErrNoRows) {
		return decision, nil
	}
	if err != nil {
		return decision, err
	}
	covering := entitlements.Binding{InstallationID: binding.InstallationID, PluginID: binding.PluginID, OrganizationID: scope.UUID}
	decision.Scope = PluginGrantScopeInstallation
	if scope.Valid {
		decision.Scope = PluginGrantScopeOrganization
	}
	if err := entitlementIssuerLock(ctx, tx, issuer, false); err != nil {
		return decision, err
	}
	if err := entitlementBindingLock(ctx, tx, covering); err != nil {
		return decision, err
	}
	issuerHint := issuer
	var issuanceID uuid.UUID
	var sequence, revision int64
	var evaluated, observedGrace, observedExpired sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT issuer,issuance_id,grant_sequence,evaluated_through,observed_grace_at,observed_expired_at,revision FROM plugin_entitlement_bindings WHERE installation_uuid=$1 AND plugin_id=$2 AND organization_uuid IS NOT DISTINCT FROM $3 FOR UPDATE`, binding.InstallationID, binding.PluginID, scope).Scan(&issuer, &issuanceID, &sequence, &evaluated, &observedGrace, &observedExpired, &revision)
	if err != nil {
		return decision, err
	}
	// The issuer read before locking is only a hint. Cross-issuer replacement is
	// currently forbidden on import, but this check also protects future schema.
	if issuer != issuerHint {
		return decision, NewErrConflict("entitlement issuer changed while acquiring its lock")
	}
	var raw, digest []byte
	var storedClass string
	var expiresAt, graceUntil time.Time
	var importer string
	var sourceDigest []byte
	if err := tx.QueryRowContext(ctx, `SELECT cose,digest,trust_class,expires_at,grace_until,importer,source_digest FROM plugin_entitlement_grants WHERE issuer=$1 AND issuance_id=$2`, issuer, issuanceID).Scan(&raw, &digest, &storedClass, &expiresAt, &graceUntil, &importer, &sourceDigest); err != nil {
		return decision, err
	}
	decision.TrustClass = entitlements.TrustClass(storedClass)
	decision.ExpiresAt, decision.GraceUntil, decision.Revision = &expiresAt, &graceUntil, revision
	effectiveNow := now.UTC().Truncate(time.Second)
	if evaluated.Valid && evaluated.Time.After(effectiveNow) {
		effectiveNow = evaluated.Time
	}
	stored := storedGrant{covering: covering, issuer: issuer, issuanceID: issuanceID, sequence: sequence, envelope: raw, digest: digest, trustClass: storedClass,
		expiresAt: expiresAt, graceUntil: graceUntil}
	verified, trustErr := verify(raw, effectiveNow)
	trusted := stored.trusts(verified, trustErr)
	var revoked bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM plugin_entitlement_revocations WHERE issuer=$1 AND issuance_id=$2)`, issuer, issuanceID).Scan(&revoked); err != nil {
		return decision, err
	}
	decision.State = grantState(verified, trusted, revoked, effectiveNow)
	var lastIssuer string
	var lastIssuance uuid.UUID
	err = tx.QueryRowContext(ctx, `SELECT last_issuer,last_issuance_id FROM plugin_entitlement_feature_activations WHERE installation_uuid=$1 AND plugin_id=$2 AND organization_uuid=$3 AND feature=$4`, binding.InstallationID, binding.PluginID, binding.OrganizationID, feature).Scan(&lastIssuer, &lastIssuance)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return decision, err
	}
	activated := err == nil
	decision.Read = access.Retained && access.Compatible && activated
	decision.Safety = decision.Read
	// Grace is a commercial warning, not a stop: forward work continues until
	// grace_until and only then falls back to read and safety access.
	forwardTime := decision.State == string(entitlements.Valid) || decision.State == string(entitlements.Grace)
	decision.Forward = access.Enabled && access.Compatible && trusted && !revoked && forwardTime && slices.Contains(verified.EffectiveFeatures, feature)
	if access.Activate && !decision.Forward {
		return decision, NewErrConflict("feature cannot be activated without a current forward entitlement")
	}
	activationChanged := access.Activate && (!activated || lastIssuer != issuer || lastIssuance != issuanceID)
	if activationChanged {
		_, err := tx.ExecContext(ctx, `INSERT INTO plugin_entitlement_feature_activations(installation_uuid,plugin_id,organization_uuid,feature,first_issuer,first_issuance_id,last_issuer,last_issuance_id,first_activated_at,last_activated_at) VALUES($1,$2,$3,$4,$5,$6,$5,$6,$7,$7)
			ON CONFLICT (installation_uuid,plugin_id,organization_uuid,feature) DO UPDATE SET last_issuer=$5,last_issuance_id=$6,last_activated_at=$7`, binding.InstallationID, binding.PluginID, binding.OrganizationID, feature, issuer, issuanceID, effectiveNow)
		if err != nil {
			return decision, err
		}
		decision.Read, decision.Safety = access.Retained && access.Compatible, access.Retained && access.Compatible
	}
	graceObserved := decision.State == string(entitlements.Grace) && !observedGrace.Valid
	expiryObserved := decision.State == string(entitlements.Expired) && !observedExpired.Valid
	if !evaluated.Valid || effectiveNow.After(evaluated.Time) || graceObserved || expiryObserved || activationChanged {
		if err := tx.QueryRowContext(ctx, `UPDATE plugin_entitlement_bindings SET evaluated_through=$4,observed_grace_at=CASE WHEN $5 AND observed_grace_at IS NULL THEN $4 ELSE observed_grace_at END,observed_expired_at=CASE WHEN $6 AND observed_expired_at IS NULL THEN $4 ELSE observed_expired_at END,revision=revision+1 WHERE installation_uuid=$1 AND plugin_id=$2 AND organization_uuid IS NOT DISTINCT FROM $3 RETURNING revision`, binding.InstallationID, binding.PluginID, scope, effectiveNow, graceObserved, expiryObserved).Scan(&decision.Revision); err != nil {
			return decision, err
		}
	}
	var auditDigest [32]byte
	copy(auditDigest[:], sourceDigest)
	// A feature activation belongs to the organization; grace and expiry to
	// the covering grant's binding.
	for _, event := range []struct {
		active  bool
		kind    string
		binding *entitlements.Binding
	}{{activationChanged, "feature_activated", &binding}, {graceObserved, "grace_observed", &covering}, {expiryObserved, "expiry_observed", &covering}} {
		if event.active {
			if err := entitlementAudit(ctx, db, tx, issuer, event.binding, event.kind, sequence, entitlements.TrustClass(storedClass), importer, auditDigest); err != nil {
				return decision, err
			}
		}
	}
	return decision, nil
}

// SweepPluginEntitlementBoundaries durably observes elapsed boundaries even
// when no request arrives. It never authorizes work and never calls a plugin.
// The caller schedules bounded repeats until fewer than limit rows are found.
func SweepPluginEntitlementBoundaries(ctx context.Context, db Databaser, now time.Time, limit int) (int, error) {
	if now.IsZero() || limit < 1 || limit > 1000 {
		return 0, NewErrBadRequest("bounded entitlement sweep and Core clock are required")
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT b.installation_uuid,b.plugin_id,b.organization_uuid,b.issuer
		FROM plugin_entitlement_bindings b JOIN plugin_entitlement_grants g ON g.issuer=b.issuer AND g.issuance_id=b.issuance_id
		WHERE (b.observed_grace_at IS NULL AND g.expires_at <= GREATEST($1,b.evaluated_through) AND GREATEST($1,b.evaluated_through) < g.grace_until)
		   OR (b.observed_expired_at IS NULL AND g.grace_until <= GREATEST($1,b.evaluated_through))
		ORDER BY b.issuer,b.installation_uuid,b.plugin_id,b.organization_uuid LIMIT $2`, now.UTC(), limit)
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	type candidate struct {
		binding entitlements.Binding
		issuer  string
	}
	candidates := []candidate{}
	for rows.Next() {
		var item candidate
		var organization uuid.NullUUID
		if err := rows.Scan(&item.binding.InstallationID, &item.binding.PluginID, &organization, &item.issuer); err != nil {
			_ = rows.Close()
			_ = tx.Rollback()
			return 0, err
		}
		item.binding.OrganizationID = organization.UUID
		candidates = append(candidates, item)
	}
	err = rows.Err()
	_ = rows.Close()
	_ = tx.Rollback()
	if err != nil {
		return 0, err
	}
	observed := 0
	for _, item := range candidates {
		effectTx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return observed, err
		}
		if err := entitlementDecisionLock(ctx, effectTx); err != nil {
			_ = effectTx.Rollback()
			return observed, err
		}
		if err := entitlementIssuerLock(ctx, effectTx, item.issuer, false); err != nil {
			_ = effectTx.Rollback()
			return observed, err
		}
		if err := entitlementBindingLock(ctx, effectTx, item.binding); err != nil {
			_ = effectTx.Rollback()
			return observed, err
		}
		var issuer, importer, trustClass string
		var sequence int64
		var evaluated, graceObserved, expiryObserved sql.NullTime
		var expiresAt, graceUntil time.Time
		var source []byte
		err = effectTx.QueryRowContext(ctx, `SELECT b.issuer,b.grant_sequence,b.evaluated_through,b.observed_grace_at,b.observed_expired_at,g.expires_at,g.grace_until,g.trust_class,g.importer,g.source_digest
			FROM plugin_entitlement_bindings b JOIN plugin_entitlement_grants g ON g.issuer=b.issuer AND g.issuance_id=b.issuance_id
			WHERE b.installation_uuid=$1 AND b.plugin_id=$2 AND b.organization_uuid IS NOT DISTINCT FROM $3 FOR UPDATE OF b`, item.binding.InstallationID, item.binding.PluginID, organizationScope(item.binding.OrganizationID)).Scan(&issuer, &sequence, &evaluated, &graceObserved, &expiryObserved, &expiresAt, &graceUntil, &trustClass, &importer, &source)
		if err != nil || issuer != item.issuer {
			_ = effectTx.Rollback()
			if err == nil {
				err = NewErrConflict("entitlement issuer changed during sweep")
			}
			return observed, err
		}
		effectiveNow := now.UTC().Truncate(time.Second)
		if evaluated.Valid && evaluated.Time.After(effectiveNow) {
			effectiveNow = evaluated.Time
		}
		markExpired := !expiryObserved.Valid && !effectiveNow.Before(graceUntil)
		markGrace := !markExpired && !graceObserved.Valid && !effectiveNow.Before(expiresAt) && effectiveNow.Before(graceUntil)
		if !markGrace && !markExpired {
			_ = effectTx.Rollback()
			continue
		}
		if _, err := effectTx.ExecContext(ctx, `UPDATE plugin_entitlement_bindings SET evaluated_through=$4,observed_grace_at=CASE WHEN $5 THEN $4 ELSE observed_grace_at END,observed_expired_at=CASE WHEN $6 THEN $4 ELSE observed_expired_at END,revision=revision+1 WHERE installation_uuid=$1 AND plugin_id=$2 AND organization_uuid IS NOT DISTINCT FROM $3`, item.binding.InstallationID, item.binding.PluginID, organizationScope(item.binding.OrganizationID), effectiveNow, markGrace, markExpired); err != nil {
			_ = effectTx.Rollback()
			return observed, err
		}
		var sourceDigest [32]byte
		copy(sourceDigest[:], source)
		kind := "grace_observed"
		if markExpired {
			kind = "expiry_observed"
		}
		if err := entitlementAudit(ctx, db, effectTx, issuer, &item.binding, kind, sequence, entitlements.TrustClass(trustClass), importer, sourceDigest); err != nil {
			_ = effectTx.Rollback()
			return observed, err
		}
		if err := effectTx.Commit(); err != nil {
			return observed, err
		}
		observed++
	}
	return observed, nil
}
