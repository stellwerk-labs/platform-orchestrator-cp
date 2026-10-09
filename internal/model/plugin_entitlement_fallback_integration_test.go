//go:build integration

package model

import (
	"crypto/sha256"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestOrganizationGrantFallbackPreservesPolicyAndEvidence(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		notBefore, expires, grace     uint64
		wideExpires, wideGrace        uint64
		revokeOwn, revokeWide, tamper bool
		wantScope, wantState          string
		wantForward                   bool
	}{
		{"expired", 100, 120, 130, 200, 250, false, false, false, PluginGrantScopeInstallation, "valid", true},
		{"revoked", 100, 200, 250, 200, 250, true, false, false, PluginGrantScopeInstallation, "valid", true},
		{"installation grace", 100, 120, 130, 140, 200, false, false, false, PluginGrantScopeInstallation, "grace", true},
		{"own valid feature restriction", 100, 200, 250, 200, 250, false, false, false, PluginGrantScopeOrganization, "valid", false},
		{"own grace feature restriction", 100, 140, 200, 200, 250, false, false, false, PluginGrantScopeOrganization, "grace", false},
		{"own future", 170, 200, 250, 200, 250, false, false, false, PluginGrantScopeOrganization, "not_yet_valid", false},
		{"own untrusted", 100, 120, 130, 200, 250, false, false, true, PluginGrantScopeOrganization, "untrusted", false},
		{"both expired", 100, 120, 130, 120, 130, false, false, false, PluginGrantScopeOrganization, "expired", false},
		{"installation revoked", 100, 120, 130, 200, 250, false, true, false, PluginGrantScopeOrganization, "expired", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEntitlementFixture(t)
			ctx := t.Context()
			source := sha256.Sum256([]byte("operator-file"))
			own := f.grant(t, uuid.New(), 1, tc.notBefore, tc.expires, tc.grace, []string{"feature-b"})
			wide := f.installationGrant(t, uuid.New(), 1, tc.wideExpires, tc.wideGrace, []string{"feature-a"}, 0)
			_, err := ImportPluginEntitlementGrant(ctx, f.db, nil, own, "internal_system", source)
			require.NoError(t, err)
			_, err = ImportPluginEntitlementGrant(ctx, f.db, nil, wide, "internal_system", source)
			require.NoError(t, err)
			if tc.revokeOwn || tc.revokeWide {
				id := own.IssuanceID
				if tc.revokeWide {
					id = wide.IssuanceID
				}
				_, err = ImportPluginEntitlementRevocations(ctx, f.db, nil, f.revocations(t, 1, id), "internal_system", source)
				require.NoError(t, err)
			}
			if tc.tamper {
				_, err = f.db.(*databaser).ExecContext(ctx, `UPDATE plugin_entitlement_grants SET cose='tampered'::bytea WHERE issuance_id=$1`, own.IssuanceID)
				require.NoError(t, err)
			}
			decision := f.decide(t, 150, "feature-a", tc.wantForward)
			require.Equal(t, tc.wantScope, decision.Scope)
			require.Equal(t, tc.wantState, decision.State)
			require.Equal(t, tc.wantForward, decision.Forward)
			if tc.wantForward {
				require.True(t, decision.Read)
				require.False(t, f.decide(t, 150, "feature-b", false).Forward, "fallback never unions the feature sets")
				var first, last uuid.UUID
				require.NoError(t, f.db.(*databaser).QueryRowContext(ctx, `SELECT first_issuance_id,last_issuance_id FROM plugin_entitlement_feature_activations WHERE feature='feature-a'`).Scan(&first, &last))
				require.Equal(t, wide.IssuanceID, first)
				require.Equal(t, wide.IssuanceID, last)
				for _, access := range []PluginEntitlementAccess{
					{Enabled: false, Compatible: true, Retained: true},
					{Enabled: true, Compatible: false, Retained: true},
				} {
					tx, err := f.db.BeginTx(ctx, nil)
					require.NoError(t, err)
					blocked, err := DecidePluginEntitlementTx(ctx, f.db, tx, f.binding, "feature-a", access, time.Unix(150, 0), f.verify)
					require.NoError(t, err)
					require.False(t, blocked.Forward)
					require.NoError(t, tx.Rollback())
				}
			}
			var retained int
			require.NoError(t, f.db.(*databaser).QueryRowContext(ctx, `SELECT count(*) FROM plugin_entitlement_grants`).Scan(&retained))
			require.Equal(t, 2, retained, "fallback must not erase either grant")
			// Rewinding the wall clock must not revive an expired own grant.
			if tc.name == "expired" {
				require.Equal(t, PluginGrantScopeInstallation, f.decide(t, 110, "feature-a", false).Scope)
			}
		})
	}
}

func TestFallbackAndRevocationSerializeInEitherOrder(t *testing.T) {
	for _, importFirst := range []bool{false, true} {
		name := "effect before revocation"
		if importFirst {
			name = "revocation before effect"
		}
		t.Run(name, func(t *testing.T) {
			f := newEntitlementFixture(t)
			ctx := t.Context()
			source := sha256.Sum256([]byte("operator-file"))
			own := f.grant(t, uuid.New(), 1, 100, 120, 130, []string{"feature-a"})
			wide := f.installationGrant(t, uuid.New(), 1, 200, 250, []string{"feature-a"}, 0)
			_, err := ImportPluginEntitlementGrant(ctx, f.db, nil, own, "internal_system", source)
			require.NoError(t, err)
			_, err = ImportPluginEntitlementGrant(ctx, f.db, nil, wide, "internal_system", source)
			require.NoError(t, err)
			require.True(t, f.decide(t, 150, "feature-a", true).Forward)
			first, err := f.db.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer func() { _ = first.Rollback() }()
			second, err := f.db.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer func() { _ = second.Rollback() }()
			var secondPID int
			require.NoError(t, second.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&secondPID))
			snapshot := f.revocations(t, 1, wide.IssuanceID)
			decide := func(tx Tx) (PluginEntitlementDecision, error) {
				return DecidePluginEntitlementTx(ctx, f.db, tx, f.binding, "feature-a", PluginEntitlementAccess{Enabled: true, Retained: true, Compatible: true}, time.Unix(150, 0), f.verify)
			}
			finished := make(chan error, 1)
			if importFirst {
				_, err = ImportPluginEntitlementRevocations(ctx, f.db, first, snapshot, "internal_system", source)
				require.NoError(t, err)
				go func() {
					decision, err := decide(second)
					if err == nil && decision.Forward {
						err = NewErrConflict("effect ignored committed revocation")
					}
					finished <- err
				}()
			} else {
				decision, err := decide(first)
				require.NoError(t, err)
				require.True(t, decision.Forward)
				go func() {
					_, err := ImportPluginEntitlementRevocations(ctx, f.db, second, snapshot, "internal_system", source)
					finished <- err
				}()
			}
			waitForEntitlementSnapshotLock(t, f.db.(*databaser).DB, secondPID)
			require.NoError(t, first.Commit())
			require.NoError(t, <-finished)
			require.NoError(t, second.Commit())
			denied := f.decide(t, 150, "feature-a", false)
			require.False(t, denied.Forward)
			require.True(t, denied.Safety, "retained safety survives loss of both forward grants")
		})
	}
}

func waitForEntitlementSnapshotLock(t *testing.T, db *sql.DB, pid int) {
	t.Helper()
	require.Eventually(t, func() bool {
		var waiting bool
		err := db.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND locktype='advisory' AND classid=640 AND objid=1 AND NOT granted)`, pid).Scan(&waiting)
		return err == nil && waiting
	}, 5*time.Second, 10*time.Millisecond, "the contender must actually reach the transaction guard")
}

func TestEntitlementImportFenceSurvivesRejectedDocumentSavepoint(t *testing.T) {
	f := newEntitlementFixture(t)
	ctx := t.Context()
	source := sha256.Sum256([]byte("operator-file"))
	current := f.grant(t, uuid.New(), 2, 100, 200, 250, []string{"feature-a"})
	_, err := ImportPluginEntitlementGrant(ctx, f.db, nil, current, "internal_system", source)
	require.NoError(t, err)
	tx, err := f.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	require.NoError(t, LockPluginEntitlementImportTx(ctx, tx))
	_, err = tx.ExecContext(ctx, `SAVEPOINT entitlement_document`)
	require.NoError(t, err)
	_, err = ImportPluginEntitlementGrant(ctx, f.db, tx, f.grant(t, uuid.New(), 1, 100, 200, 250, []string{"feature-a"}), "internal_system", source)
	require.ErrorContains(t, err, "stale entitlement grant sequence")
	_, err = tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT entitlement_document`)
	require.NoError(t, err)
	contender, err := f.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = contender.Rollback() }()
	var acquired bool
	require.NoError(t, contender.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock_shared(640, 1)`).Scan(&acquired))
	require.False(t, acquired, "a rejected document cannot release the bundle guard")
	require.NoError(t, tx.Commit())
	require.NoError(t, contender.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock_shared(640, 1)`).Scan(&acquired))
	require.True(t, acquired)
}
