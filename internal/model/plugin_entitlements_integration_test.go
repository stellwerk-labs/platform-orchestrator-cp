//go:build integration

package model

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
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
