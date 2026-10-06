//go:build integration

package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/entitlements"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/pgtest"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/pluginentitlements"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestDevelopmentRenewalImportsAfterExpiryWithMonotonicSequences(t *testing.T) {
	dsn := os.Getenv("PLUGIN_TEST_DATABASE_URL")
	require.NotEmpty(t, dsn)
	admin, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	schema := "plugin_dev_renewal_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = admin.Exec(`CREATE SCHEMA ` + schema)
	require.NoError(t, err)
	db, err := model.NewDatabaser(t.Context(), zap.NewNop(), pgtest.WithSearchPath(dsn, schema))
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
		_, dropErr := admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		require.NoError(t, dropErr)
		require.NoError(t, admin.Close())
	})
	now := time.Now().UTC().Truncate(time.Second)
	organization, err := db.CreateOrg(t.Context(), nil, &model.Org{Id: "development-renewal", CreatedAt: now, CreatedBy: uuid.New(), Source: model.OrgSourceInternal, Plan: model.OrgPlanCustom})
	require.NoError(t, err)
	installation := uuid.New()
	binding := entitlements.Binding{PluginID: "dev.example.workflow", InstallationID: installation, OrganizationID: organization.Uuid, DeclaredFeatures: []string{"feature-a", "feature-b"}}
	parent, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	issuerDir := filepath.Join(parent, "issuer")
	require.NoError(t, run(developmentArgs(issuerDir, installation, organization.Uuid), now, &bytes.Buffer{}))
	trustJSON, err := readPrivateRegularFile(filepath.Join(issuerDir, "trust-development.json"), entitlements.MaxTrustJSONBytes)
	require.NoError(t, err)
	trust, err := entitlements.ParseTrustJSON(trustJSON, entitlements.Development)
	require.NoError(t, err)
	load := func(path string, at time.Time) (entitlements.Grant, [32]byte) {
		t.Helper()
		raw, readErr := readPrivateRegularFile(path, pluginentitlements.MaxBundleBytes)
		require.NoError(t, readErr)
		bundle, parseErr := pluginentitlements.ParseBundleJSON(raw)
		require.NoError(t, parseErr)
		require.Len(t, bundle.Grants, 1)
		grant, verifyErr := entitlements.VerifyGrant(bundle.Grants[0], trust, entitlements.Development, binding, at)
		require.NoError(t, verifyErr)
		return grant, bundle.Digest
	}
	firstBundle := filepath.Join(issuerDir, "bundle-development.json")
	first, firstDigest := load(firstBundle, now)
	imported, err := model.ImportPluginEntitlementGrant(t.Context(), db, first, "internal_system", firstDigest)
	require.NoError(t, err)
	require.True(t, imported)
	imported, err = model.ImportPluginEntitlementGrant(t.Context(), db, first, "internal_system", firstDigest)
	require.NoError(t, err)
	require.False(t, imported, "exact signed replay must be idempotent")
	decide := func(at time.Time, activate bool) model.PluginEntitlementDecision {
		t.Helper()
		tx, txErr := db.BeginTx(t.Context(), nil)
		require.NoError(t, txErr)
		defer func() { _ = tx.Rollback() }()
		decision, decisionErr := model.DecidePluginEntitlementTx(t.Context(), db, tx, binding, "feature-a", model.PluginEntitlementAccess{Enabled: true, Retained: true, Compatible: true, Activate: activate}, at, func(raw []byte, effective time.Time) (entitlements.Grant, error) {
			return entitlements.VerifyGrant(raw, trust, entitlements.Development, binding, effective)
		})
		require.NoError(t, decisionErr)
		require.NoError(t, tx.Commit())
		return decision
	}
	require.True(t, decide(now, true).Forward)
	expiredAt := now.Add(3 * time.Hour)
	expired := decide(expiredAt, false)
	require.Equal(t, "expired", expired.State)
	require.False(t, expired.Forward)
	require.True(t, expired.Read, "previous activation preserves historical read")
	renewDir := filepath.Join(parent, "renewal")
	args := append(developmentArgs(renewDir, installation, organization.Uuid), "-issuer-dir", issuerDir, "-previous-bundle", firstBundle)
	require.NoError(t, run(args, expiredAt, &bytes.Buffer{}))
	second, secondDigest := load(filepath.Join(renewDir, "bundle-development.json"), expiredAt)
	require.Equal(t, first.Issuer, second.Issuer)
	require.Equal(t, first.KeyID, second.KeyID)
	require.Equal(t, int64(2), second.Sequence)
	require.NotEqual(t, first.IssuanceID, second.IssuanceID)
	imported, err = model.ImportPluginEntitlementGrant(t.Context(), db, second, "internal_system", secondDigest)
	require.NoError(t, err)
	require.True(t, imported)
	imported, err = model.ImportPluginEntitlementGrant(t.Context(), db, second, "internal_system", secondDigest)
	require.NoError(t, err)
	require.False(t, imported)
	_, err = model.ImportPluginEntitlementGrant(t.Context(), db, first, "internal_system", firstDigest)
	require.Error(t, err, "old sequence cannot replace the renewed head")
	branchedDir := filepath.Join(parent, "stale-branch")
	branchArgs := append(developmentArgs(branchedDir, installation, organization.Uuid), "-issuer-dir", issuerDir, "-previous-bundle", firstBundle)
	require.NoError(t, run(branchArgs, expiredAt.Add(time.Second), &bytes.Buffer{}))
	branched, branchDigest := load(filepath.Join(branchedDir, "bundle-development.json"), expiredAt.Add(time.Second))
	require.Equal(t, second.Sequence, branched.Sequence)
	require.NotEqual(t, second.IssuanceID, branched.IssuanceID)
	_, err = model.ImportPluginEntitlementGrant(t.Context(), db, branched, "internal_system", branchDigest)
	require.Error(t, err, "same-sequence branch cannot replace imported renewal")
	renewed := decide(expiredAt, false)
	require.Equal(t, "valid", renewed.State)
	require.True(t, renewed.Forward)
	require.True(t, renewed.Read)
	require.NotEqual(t, sha256.Sum256(first.Envelope), sha256.Sum256(second.Envelope))
}
