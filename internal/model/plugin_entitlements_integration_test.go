//go:build integration

package model

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/entitlements"
	"github.com/stretchr/testify/require"
)

type entitlementFixture struct {
	db      Databaser
	private ed25519.PrivateKey
	trust   entitlements.TrustStore
	binding entitlements.Binding
}

func newEntitlementFixture(t *testing.T) entitlementFixture {
	t.Helper()
	db := externalOperationDatabase(t)
	org, err := db.CreateOrg(t.Context(), nil, &Org{Id: "entitlement-" + uuid.NewString(), CreatedAt: time.Now(), CreatedBy: uuid.New(), Source: OrgSourceInternal, Plan: OrgPlanCustom})
	require.NoError(t, err)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	binding := entitlements.Binding{PluginID: "dev.example.native-plugin", InstallationID: uuid.New(), OrganizationID: org.Uuid, DeclaredFeatures: []string{"feature-a", "feature-b"}}
	trust := entitlements.TrustStore{Class: entitlements.Development, Issuers: []entitlements.Issuer{{ID: "development.example", PluginIDs: []string{binding.PluginID}, Keys: []entitlements.Key{{ID: []byte("development-key"), PublicKey: public, Purposes: []entitlements.Purpose{entitlements.GrantPurpose, entitlements.RevocationPurpose}}}}}}
	return entitlementFixture{db: db, private: private, trust: trust, binding: binding}
}

func (f entitlementFixture) sign(t *testing.T, claims map[uint64]any) []byte {
	t.Helper()
	mode, err := cbor.CoreDetEncOptions().EncMode()
	require.NoError(t, err)
	protected, err := mode.Marshal(map[int]any{1: -8, 4: []byte("development-key")})
	require.NoError(t, err)
	payload, err := mode.Marshal(claims)
	require.NoError(t, err)
	sigStructure, err := mode.Marshal([]any{"Signature1", protected, []byte{}, payload})
	require.NoError(t, err)
	returnRaw, err := mode.Marshal(cbor.Tag{Number: 18, Content: []any{protected, map[int]any{}, payload, ed25519.Sign(f.private, sigStructure)}})
	require.NoError(t, err)
	return returnRaw
}

func (f entitlementFixture) grant(t *testing.T, issuance uuid.UUID, sequence, notBefore, expires, grace uint64, features []string) entitlements.Grant {
	t.Helper()
	claims := map[uint64]any{1: uint64(1), 2: "grant", 3: "development.example", 4: issuance[:], 5: sequence, 6: f.binding.PluginID, 7: f.binding.InstallationID[:], 8: f.binding.OrganizationID[:], 9: features, 10: map[string]uint64{}, 11: uint64(100), 12: notBefore, 13: expires, 14: grace, 15: "development"}
	grant, err := entitlements.VerifyGrant(f.sign(t, claims), f.trust, entitlements.Development, f.binding, time.Unix(100, 0))
	require.NoError(t, err)
	return grant
}

func (f entitlementFixture) revocations(t *testing.T, sequence uint64, ids ...uuid.UUID) entitlements.Revocations {
	t.Helper()
	encoded := make([][]byte, len(ids))
	for i, id := range ids {
		encoded[i] = id[:]
	}
	claims := map[uint64]any{1: uint64(1), 2: "revocations", 3: "development.example", 4: sequence, 5: uint64(100), 6: encoded, 7: "development"}
	snapshot, err := entitlements.VerifyRevocations(f.sign(t, claims), f.trust, entitlements.Development, time.Unix(100, 0))
	require.NoError(t, err)
	return snapshot
}

func (f entitlementFixture) verify(raw []byte, now time.Time) (entitlements.Grant, error) {
	return entitlements.VerifyGrant(raw, f.trust, entitlements.Development, f.binding, now)
}

func (f entitlementFixture) decide(t *testing.T, now int64, feature string, activate bool) PluginEntitlementDecision {
	t.Helper()
	tx, err := f.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	decision, err := DecidePluginEntitlementTx(t.Context(), f.db, tx, f.binding, feature, PluginEntitlementAccess{Enabled: true, Retained: true, Compatible: true, Activate: activate}, time.Unix(now, 0), f.verify)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	return decision
}

func TestPluginEntitlementImportTimeActivationAndTrust(t *testing.T) {
	f := newEntitlementFixture(t)
	source := sha256.Sum256([]byte("operator-file"))
	id1 := uuid.New()
	first := f.grant(t, id1, 1, 100, 200, 250, []string{"feature-a"})
	imported, err := ImportPluginEntitlementGrant(t.Context(), f.db, nil, first, "internal_system", source)
	require.NoError(t, err)
	require.True(t, imported)
	imported, err = ImportPluginEntitlementGrant(t.Context(), f.db, nil, first, "internal_system", source)
	require.NoError(t, err)
	require.False(t, imported)
	require.True(t, f.decide(t, 150, "feature-a", true).Forward)
	require.False(t, f.decide(t, 150, "feature-b", false).Forward)
	grace := f.decide(t, 200, "feature-a", false)
	require.Equal(t, "grace", grace.State)
	require.True(t, grace.Forward, "grace keeps forward work and only warns")
	require.True(t, grace.Read)
	require.Equal(t, int64(250), grace.GraceUntil.Unix())
	expired := f.decide(t, 250, "feature-a", false)
	require.Equal(t, "expired", expired.State)
	require.True(t, expired.Safety)
	require.Equal(t, "expired", f.decide(t, 150, "feature-a", false).State, "clock rollback cannot restore forward authority")
	changed := f.grant(t, uuid.New(), 1, 100, 400, 450, []string{"feature-a"})
	_, err = ImportPluginEntitlementGrant(t.Context(), f.db, nil, changed, "internal_system", source)
	require.ErrorContains(t, err, "same grant sequence")
	second := f.grant(t, uuid.New(), 2, 100, 400, 450, []string{"feature-b"})
	imported, err = ImportPluginEntitlementGrant(t.Context(), f.db, nil, second, "internal_system", source)
	require.NoError(t, err)
	require.True(t, imported)
	require.False(t, f.decide(t, 300, "feature-a", false).Forward, "old feature cannot be restored by activation")
	require.True(t, f.decide(t, 300, "feature-a", false).Read)
	require.True(t, f.decide(t, 300, "feature-b", true).Forward)
	var activationAudits int
	require.NoError(t, f.db.(*databaser).DB.QueryRowContext(t.Context(), `SELECT count(*) FROM plugin_entitlement_audit_events WHERE kind='feature_activated'`).Scan(&activationAudits))
	require.Equal(t, 2, activationAudits)
	require.True(t, f.decide(t, 300, "feature-b", true).Forward)
	require.NoError(t, f.db.(*databaser).DB.QueryRowContext(t.Context(), `SELECT count(*) FROM plugin_entitlement_audit_events WHERE kind='feature_activated'`).Scan(&activationAudits))
	require.Equal(t, 2, activationAudits, "ordinary forward checks do not rewrite activation evidence")
	_, err = ImportPluginEntitlementGrant(t.Context(), f.db, nil, first, "internal_system", source)
	require.ErrorContains(t, err, "stale")
	otherBinding := f.binding
	otherBinding.OrganizationID = uuid.New()
	tx, err := f.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	decision, err := DecidePluginEntitlementTx(t.Context(), f.db, tx, otherBinding, "feature-a", PluginEntitlementAccess{Enabled: true, Retained: true, Compatible: true}, time.Unix(300, 0), f.verify)
	require.NoError(t, err)
	require.Equal(t, "not_entitled", decision.State)
	require.NoError(t, tx.Rollback())
	for _, other := range []entitlements.Binding{
		{PluginID: "dev.example.other", InstallationID: f.binding.InstallationID, OrganizationID: f.binding.OrganizationID},
		{PluginID: f.binding.PluginID, InstallationID: uuid.New(), OrganizationID: f.binding.OrganizationID},
	} {
		tx, err := f.db.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		decision, err := DecidePluginEntitlementTx(t.Context(), f.db, tx, other, "feature-a", PluginEntitlementAccess{Enabled: true, Retained: true, Compatible: true}, time.Unix(300, 0), f.verify)
		require.NoError(t, err)
		require.Equal(t, "not_entitled", decision.State)
		require.NoError(t, tx.Rollback())
	}
	f.trust.Issuers = nil
	untrusted := f.decide(t, 300, "feature-b", false)
	require.Equal(t, "untrusted", untrusted.State)
	require.False(t, untrusted.Forward)
	require.True(t, untrusted.Read)
	var grants, activations, audits int
	require.NoError(t, f.db.(*databaser).DB.QueryRowContext(t.Context(), `SELECT count(*) FROM plugin_entitlement_grants`).Scan(&grants))
	require.NoError(t, f.db.(*databaser).DB.QueryRowContext(t.Context(), `SELECT count(*) FROM plugin_entitlement_feature_activations`).Scan(&activations))
	require.NoError(t, f.db.(*databaser).DB.QueryRowContext(t.Context(), `SELECT count(*) FROM plugin_entitlement_audit_events`).Scan(&audits))
	require.Equal(t, 2, grants)
	require.Equal(t, 2, activations)
	require.GreaterOrEqual(t, audits, 5)
}

func TestPluginEntitlementRevocationAndForwardSerialize(t *testing.T) {
	f := newEntitlementFixture(t)
	source := sha256.Sum256([]byte("operator-file"))
	grant := f.grant(t, uuid.New(), 1, 100, 400, 450, []string{"feature-a"})
	_, err := ImportPluginEntitlementGrant(t.Context(), f.db, nil, grant, "internal_system", source)
	require.NoError(t, err)
	initial := f.decide(t, 150, "feature-a", true)
	require.True(t, initial.Forward)
	forwardTx, err := f.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	forward, err := DecidePluginEntitlementTx(t.Context(), f.db, forwardTx, f.binding, "feature-a", PluginEntitlementAccess{Enabled: true, Retained: true, Compatible: true}, time.Unix(160, 0), f.verify)
	require.NoError(t, err)
	require.True(t, forward.Forward)
	snapshot := f.revocations(t, 1, grant.IssuanceID)
	finished := make(chan error, 1)
	go func() {
		_, importErr := ImportPluginEntitlementRevocations(context.Background(), f.db, nil, snapshot, "internal_system", source)
		finished <- importErr
	}()
	select {
	case err := <-finished:
		t.Fatalf("revocation passed an open forward transaction: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	require.NoError(t, forwardTx.Commit())
	require.NoError(t, <-finished)
	revoked := f.decide(t, 160, "feature-a", false)
	require.Equal(t, "revoked", revoked.State)
	require.False(t, revoked.Forward)
	require.True(t, revoked.Safety)
	require.Greater(t, revoked.Revision, forward.Revision)
	imported, err := ImportPluginEntitlementRevocations(t.Context(), f.db, nil, snapshot, "internal_system", source)
	require.NoError(t, err)
	require.False(t, imported)
	changed := f.revocations(t, 1)
	_, err = ImportPluginEntitlementRevocations(t.Context(), f.db, nil, changed, "internal_system", source)
	require.ErrorContains(t, err, "changed")
	missing := f.revocations(t, 2)
	_, err = ImportPluginEntitlementRevocations(t.Context(), f.db, nil, missing, "internal_system", source)
	require.ErrorContains(t, err, "include all")
	_, err = ImportPluginEntitlementRevocations(t.Context(), f.db, nil, f.revocations(t, 2, grant.IssuanceID), "internal_system", source)
	require.NoError(t, err)
	_, err = ImportPluginEntitlementRevocations(t.Context(), f.db, nil, snapshot, "internal_system", source)
	require.ErrorContains(t, err, "stale")
	renewal := f.grant(t, uuid.New(), 2, 100, 400, 450, []string{"feature-a"})
	_, err = ImportPluginEntitlementGrant(t.Context(), f.db, nil, renewal, "internal_system", source)
	require.NoError(t, err)
	require.True(t, f.decide(t, 160, "feature-a", false).Forward, "new higher-sequence issuance may restore forward authority")
}

func TestPluginEntitlementConcurrentHigherGrants(t *testing.T) {
	f := newEntitlementFixture(t)
	source := sha256.Sum256([]byte("operator-file"))
	first := f.grant(t, uuid.New(), 1, 100, 500, 500, []string{"feature-a"})
	_, err := ImportPluginEntitlementGrant(t.Context(), f.db, nil, first, "internal_system", source)
	require.NoError(t, err)
	grants := []entitlements.Grant{f.grant(t, uuid.New(), 2, 100, 500, 500, []string{"feature-a"}), f.grant(t, uuid.New(), 3, 100, 500, 500, []string{"feature-a"})}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, grant := range grants {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := ImportPluginEntitlementGrant(context.Background(), f.db, nil, grant, "internal_system", source)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			require.ErrorContains(t, err, "stale")
		}
	}
	var sequence int64
	require.NoError(t, f.db.(*databaser).DB.QueryRowContext(t.Context(), `SELECT grant_sequence FROM plugin_entitlement_bindings`).Scan(&sequence))
	require.Equal(t, int64(3), sequence)
}

func TestPluginEntitlementMigrationDownRefusesEvidence(t *testing.T) {
	f := newEntitlementFixture(t)
	provider := extensionMigrationProvider(t, f.db)
	_, err := provider.DownTo(t.Context(), 37)
	require.NoError(t, err)
	_, err = provider.UpTo(t.Context(), 38)
	require.NoError(t, err)
	grant := f.grant(t, uuid.New(), 1, 100, 200, 200, []string{"feature-a"})
	_, err = ImportPluginEntitlementGrant(t.Context(), f.db, nil, grant, "internal_system", sha256.Sum256([]byte("operator-file")))
	require.NoError(t, err)
	_, err = provider.DownTo(t.Context(), 37)
	require.ErrorContains(t, err, "cannot discard plugin entitlement evidence")
	var count int
	require.NoError(t, f.db.(*databaser).DB.QueryRowContext(t.Context(), `SELECT count(*) FROM plugin_entitlement_grants`).Scan(&count))
	require.Equal(t, 1, count)
}

func TestPluginEntitlementSweepPersistsBoundaries(t *testing.T) {
	f := newEntitlementFixture(t)
	grant := f.grant(t, uuid.New(), 1, 100, 200, 250, []string{"feature-a"})
	_, err := ImportPluginEntitlementGrant(t.Context(), f.db, nil, grant, "internal_system", sha256.Sum256([]byte("operator-file")))
	require.NoError(t, err)
	count, err := SweepPluginEntitlementBoundaries(t.Context(), f.db, time.Unix(200, 0), 100)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	count, err = SweepPluginEntitlementBoundaries(t.Context(), f.db, time.Unix(200, 0), 100)
	require.NoError(t, err)
	require.Zero(t, count)
	count, err = SweepPluginEntitlementBoundaries(t.Context(), f.db, time.Unix(250, 0), 100)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	require.Equal(t, "expired", f.decide(t, 150, "feature-a", false).State)
	var grace, expiry sql.NullTime
	require.NoError(t, f.db.(*databaser).DB.QueryRowContext(t.Context(), `SELECT observed_grace_at,observed_expired_at FROM plugin_entitlement_bindings`).Scan(&grace, &expiry))
	require.True(t, grace.Valid)
	require.True(t, expiry.Valid)
}

// installationGrant signs a grant without an organization: it covers every
// organization of the fixture's installation.
func (f entitlementFixture) installationGrant(t *testing.T, issuance uuid.UUID, sequence, expires, grace uint64, features []string, limit uint64) entitlements.Grant {
	t.Helper()
	limits := map[string]uint64{}
	if limit != 0 {
		limits["organizations"] = limit
	}
	claims := map[uint64]any{1: uint64(1), 2: "grant", 3: "development.example", 4: issuance[:], 5: sequence, 6: f.binding.PluginID, 7: f.binding.InstallationID[:], 9: features, 10: limits, 11: uint64(100), 12: uint64(100), 13: expires, 14: grace, 15: "development"}
	scope := f.binding
	scope.OrganizationID = uuid.Nil
	grant, err := entitlements.VerifyGrant(f.sign(t, claims), f.trust, entitlements.Development, scope, time.Unix(100, 0))
	require.NoError(t, err)
	require.True(t, grant.InstallationWide())
	return grant
}

// decideFor decides feature for the organization with UUID org of the
// fixture's installation.
func (f entitlementFixture) decideFor(t *testing.T, org uuid.UUID, now int64, feature string, activate bool) PluginEntitlementDecision {
	t.Helper()
	binding := f.binding
	binding.OrganizationID = org
	tx, err := f.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	decision, err := DecidePluginEntitlementTx(t.Context(), f.db, tx, binding, feature, PluginEntitlementAccess{Enabled: true, Retained: true, Compatible: true, Activate: activate}, time.Unix(now, 0),
		func(raw []byte, now time.Time) (entitlements.Grant, error) {
			return entitlements.VerifyGrant(raw, f.trust, entitlements.Development, binding, now)
		})
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	return decision
}

func TestAnInstallationWideGrantCoversEveryOrganizationWithoutAGrantOfItsOwn(t *testing.T) {
	f := newEntitlementFixture(t)
	ctx := t.Context()
	db := f.db.(*databaser).DB
	source := sha256.Sum256([]byte("operator-file"))
	acme := f.binding.OrganizationID
	later, err := f.db.CreateOrg(ctx, nil, &Org{Id: "later-" + uuid.NewString(), CreatedAt: time.Now(), CreatedBy: uuid.New(), Source: OrgSourceInternal, Plan: OrgPlanCustom})
	require.NoError(t, err)
	wide := f.installationGrant(t, uuid.New(), 1, 200, 250, []string{"feature-a"}, 2)
	imported, err := ImportPluginEntitlementGrant(ctx, f.db, nil, wide, "internal_system", source)
	require.NoError(t, err)
	require.True(t, imported)
	var stored string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COALESCE(organization_uuid::text,'none') || ' ' || limits::text FROM plugin_entitlement_grants`).Scan(&stored))
	require.Equal(t, `none {"organizations": 2}`, stored, "an installation-wide grant names no organization")

	// It covers every organization, also one created after the import.
	for _, org := range []uuid.UUID{acme, later.Uuid} {
		decision := f.decideFor(t, org, 150, "feature-a", true)
		require.True(t, decision.Forward, org)
		require.Equal(t, PluginGrantScopeInstallation, decision.Scope)
	}
	var activations string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT string_agg(organization_uuid::text, ' ' ORDER BY organization_uuid::text) FROM plugin_entitlement_feature_activations`).Scan(&activations))
	expected := []string{acme.String(), later.Uuid.String()}
	slices.Sort(expected)
	require.Equal(t, strings.Join(expected, " "), activations, "features activate per organization")
	var audited string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT string_agg(COALESCE(organization_uuid::text, 'none'), ' ' ORDER BY organization_uuid::text) FROM plugin_entitlement_audit_events WHERE kind='feature_activated'`).Scan(&audited))
	require.Equal(t, strings.Join(expected, " "), audited, "each activation is audited for its organization")
	require.Equal(t, int64(2), f.organizationLimit(t))

	// An organization grant wins for its organization, in features and
	// validity: acme's own grant licenses only feature-b and has expired.
	own := f.grant(t, uuid.New(), 1, 100, 120, 130, []string{"feature-b"})
	_, err = ImportPluginEntitlementGrant(ctx, f.db, nil, own, "internal_system", source)
	require.NoError(t, err)
	decision := f.decideFor(t, acme, 150, "feature-a", false)
	require.Equal(t, PluginGrantScopeOrganization, decision.Scope)
	require.Equal(t, "expired", decision.State)
	require.False(t, decision.Forward, "acme's own grant decides, not the installation-wide one")
	require.True(t, decision.Read, "feature-a stays readable where it was activated")
	require.True(t, f.decideFor(t, later.Uuid, 150, "feature-a", false).Forward, "the other organization keeps the installation-wide grant")

	// A higher sequence replaces the installation-wide grant, a lower one is
	// refused, and the same sequence with other content conflicts.
	renewal := f.installationGrant(t, uuid.New(), 2, 400, 450, []string{"feature-a"}, 0)
	_, err = ImportPluginEntitlementGrant(ctx, f.db, nil, renewal, "internal_system", source)
	require.NoError(t, err)
	renewed := f.decideFor(t, later.Uuid, 300, "feature-a", false)
	require.Equal(t, "valid", renewed.State)
	require.Equal(t, int64(400), renewed.ExpiresAt.Unix())
	require.Zero(t, f.organizationLimit(t), "the renewal carries no limit")
	_, err = ImportPluginEntitlementGrant(ctx, f.db, nil, wide, "internal_system", source)
	require.ErrorContains(t, err, "stale")
	_, err = ImportPluginEntitlementGrant(ctx, f.db, nil, f.installationGrant(t, uuid.New(), 2, 500, 550, []string{"feature-a"}, 0), "internal_system", source)
	require.ErrorContains(t, err, "same grant sequence")

	// Revoking the installation-wide grant revokes it for every
	// organization it covers, and only those. The revocation advances the
	// binding's revision like every change of its decision.
	_, err = ImportPluginEntitlementRevocations(ctx, f.db, nil, f.revocations(t, 1, renewal.IssuanceID), "internal_system", source)
	require.NoError(t, err)
	revoked := f.decideFor(t, later.Uuid, 300, "feature-a", false)
	require.Equal(t, "revoked", revoked.State)
	require.Equal(t, renewed.Revision+1, revoked.Revision)
	require.Equal(t, PluginGrantScopeOrganization, f.decideFor(t, acme, 300, "feature-b", false).Scope)

	// Grace and expiry of the installation-wide grant are observed on its
	// own binding, and audited without an organization.
	expiring := f.installationGrant(t, uuid.New(), 3, 600, 650, []string{"feature-a"}, 0)
	_, err = ImportPluginEntitlementGrant(ctx, f.db, nil, expiring, "internal_system", source)
	require.NoError(t, err)
	count, err := SweepPluginEntitlementBoundaries(ctx, f.db, time.Unix(650, 0), 100)
	require.NoError(t, err)
	require.Equal(t, 1, count, "acme's own expiry was observed by its decision already")
	var unscoped int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM plugin_entitlement_audit_events WHERE kind='expiry_observed' AND organization_uuid IS NULL`).Scan(&unscoped))
	require.Equal(t, 1, unscoped)

	// Storage holds one current installation-wide binding and one grant per
	// issuer, binding and sequence, NULL organization included.
	_, err = db.ExecContext(ctx, `INSERT INTO plugin_entitlement_bindings (installation_uuid,plugin_id,organization_uuid,issuer,issuance_id,grant_sequence,revision)
		VALUES ($1,$2,NULL,'development.example',$3,3,1)`, f.binding.InstallationID, f.binding.PluginID, expiring.IssuanceID)
	require.ErrorContains(t, err, "plugin_entitlement_bindings_key")
	_, err = db.ExecContext(ctx, `INSERT INTO plugin_entitlement_grants (issuer,issuance_id,grant_sequence,installation_uuid,plugin_id,organization_uuid,cose,digest,key_id,features,limits,issued_at,not_before,expires_at,grace_until,trust_class,importer,source_digest)
		SELECT issuer,$1,grant_sequence,installation_uuid,plugin_id,organization_uuid,cose,digest,key_id,features,limits,issued_at,not_before,expires_at,grace_until,trust_class,importer,source_digest
		FROM plugin_entitlement_grants WHERE issuance_id=$2`, uuid.New(), expiring.IssuanceID)
	require.ErrorContains(t, err, "plugin_entitlement_grants_sequence_key")
	_, err = db.ExecContext(ctx, `UPDATE plugin_entitlement_grants SET limits='{"organizations": 3}' WHERE issuance_id=$1`, own.IssuanceID)
	require.ErrorContains(t, err, "plugin_entitlement_grants_limits_scope", "only an installation-wide grant limits organizations")
}

// organizationLimit reads the organization limit of the fixture's
// installation.
func (f entitlementFixture) organizationLimit(t *testing.T) int64 {
	t.Helper()
	tx, err := f.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	limit, err := PluginOrganizationLimit(t.Context(), tx, f.binding.InstallationID, f.binding.PluginID)
	require.NoError(t, err)
	return limit
}

func TestInstallationWideGrantsBlockTheDowngrade(t *testing.T) {
	f := newEntitlementFixture(t)
	provider := extensionMigrationProvider(t, f.db)
	wide := f.installationGrant(t, uuid.New(), 1, 200, 250, []string{"feature-a"}, 0)
	_, err := ImportPluginEntitlementGrant(t.Context(), f.db, nil, wide, "internal_system", sha256.Sum256([]byte("operator-file")))
	require.NoError(t, err)
	_, err = provider.DownTo(t.Context(), 47)
	require.ErrorContains(t, err, "cannot downgrade while installation-wide entitlement grants exist")
	_, err = f.db.(*databaser).ExecContext(t.Context(), `DELETE FROM plugin_entitlement_bindings`)
	require.NoError(t, err)
	_, err = f.db.(*databaser).ExecContext(t.Context(), `DELETE FROM plugin_entitlement_grants`)
	require.NoError(t, err)
	_, err = provider.DownTo(t.Context(), 47)
	require.NoError(t, err)
	_, err = provider.UpTo(t.Context(), 48)
	require.NoError(t, err)
}

func TestPluginEntitlementImportRollbackLeavesNoEvidence(t *testing.T) {
	f := newEntitlementFixture(t)
	f.binding.OrganizationID = uuid.New() // Valid signed binding, absent from this database.
	grant := f.grant(t, uuid.New(), 1, 100, 200, 200, []string{"feature-a"})
	_, err := ImportPluginEntitlementGrant(t.Context(), f.db, nil, grant, "internal_system", sha256.Sum256([]byte("operator-file")))
	require.Error(t, err)
	for _, table := range []string{"plugin_entitlement_grants", "plugin_entitlement_bindings", "plugin_entitlement_audit_events"} {
		var count int
		require.NoError(t, f.db.(*databaser).DB.QueryRowContext(t.Context(), `SELECT count(*) FROM `+table).Scan(&count))
		require.Equal(t, 0, count)
	}
	var outboxCount int
	require.NoError(t, f.db.(*databaser).DB.QueryRowContext(t.Context(), `SELECT count(*) FROM pending_event_messages`).Scan(&outboxCount))
	require.Equal(t, 0, outboxCount)
}

func TestPluginEntitlementOutboxFailureRollsBackImport(t *testing.T) {
	f := newEntitlementFixture(t)
	_, err := f.db.(*databaser).ExecContext(t.Context(), `DROP TABLE pending_event_messages`)
	require.NoError(t, err)
	grant := f.grant(t, uuid.New(), 1, 100, 200, 200, []string{"feature-a"})
	_, err = ImportPluginEntitlementGrant(t.Context(), f.db, nil, grant, "internal_system", sha256.Sum256([]byte("operator-file")))
	require.Error(t, err)
	for _, table := range []string{"plugin_entitlement_grants", "plugin_entitlement_bindings", "plugin_entitlement_audit_events"} {
		var count int
		require.NoError(t, f.db.(*databaser).DB.QueryRowContext(t.Context(), `SELECT count(*) FROM `+table).Scan(&count))
		require.Zero(t, count)
	}
}
