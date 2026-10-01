//go:build integration

package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/herrors"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/entitlements"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/pluginentitlements"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"
)

type pluginEntitlementAPIFixture struct {
	db        model.Databaser
	item      plugins.Installation
	server    *Server
	adapter   *pluginentitlements.Adapter
	private   ed25519.PrivateKey
	trust     entitlements.TrustStore
	binding   entitlements.Binding
	clock     atomic.Int64
	base      int64
	trustPath string
	grant     entitlements.Grant
}

func newPluginEntitlementAPIFixture(t *testing.T) *pluginEntitlementAPIFixture {
	t.Helper()
	db, _ := pluginDeploymentBoundaryDatabase(t)
	org, err := db.GetOrg(t.Context(), nil, "acme")
	require.NoError(t, err)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKIXPublicKey(public)
	require.NoError(t, err)
	installationID := uuid.New()
	manifestDigest := sha256.Sum256([]byte("verified package manifest"))
	item := plugins.Installation{
		ID: "dev.example.workflow", InstallationID: installationID.String(), ManifestSHA256: hex.EncodeToString(manifestDigest[:]),
		DisplayName: "Workflow", Version: "1.0.0", Backend: "https://workflow.example.test",
		Organizations: map[string]plugins.AvailabilityState{"acme": plugins.StateAvailable},
		Entitlement:   &plugins.EntitlementDeclaration{Mode: "required", Features: []string{"feature-a"}, Limits: []string{}},
		ServiceUserID: userid.NewServiceUserTokenId().String(), AdmissionCheckerID: userid.NewServiceUserTokenId().String(),
		ServiceGrants: []plugins.ServiceGrant{{Action: "effect.commit", RequiredFeature: "feature-a", Permission: "plugin.dev.example.workflow.control", Scope: "organization"}},
		Operations:    []plugins.Operation{{ID: "effect", Method: "POST", Path: "/orgs/{orgId}/effects", Mutating: true, RequiredFeature: "feature-a", ServiceActions: []string{"effect.commit"}}},
	}
	encodedItem, err := json.Marshal([]plugins.Installation{item})
	require.NoError(t, err)
	registry, err := plugins.LoadInstallations(bytes.NewReader(encodedItem), false)
	require.NoError(t, err)
	item = registry.Installations()[0]
	binding := entitlements.Binding{PluginID: item.ID, InstallationID: installationID, OrganizationID: org.Uuid, DeclaredFeatures: []string{"feature-a"}}
	trust := entitlements.TrustStore{Class: entitlements.Development, Issuers: []entitlements.Issuer{{ID: "development.example", PluginIDs: []string{item.ID}, Keys: []entitlements.Key{{ID: []byte("development-key"), PublicKey: public, Purposes: []entitlements.Purpose{entitlements.GrantPurpose, entitlements.RevocationPurpose}}}}}}
	trustJSON, err := json.Marshal(map[string]any{"schema_version": 1, "trust_class": "development", "issuers": []any{map[string]any{"id": "development.example", "plugin_ids": []string{item.ID}, "keys": []any{map[string]any{"kid": base64.RawURLEncoding.EncodeToString([]byte("development-key")), "public_key_pem": string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: keyDER})), "purposes": []string{"grant", "revocation"}, "valid_from": 0, "valid_until": 0}}}}})
	require.NoError(t, err)
	tempDir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	trustPath := filepath.Join(tempDir, "development-trust.json")
	require.NoError(t, os.WriteFile(trustPath, trustJSON, 0o600))
	f := &pluginEntitlementAPIFixture{db: db, item: item, private: private, trust: trust, binding: binding, base: time.Now().UTC().Truncate(time.Second).Unix(), trustPath: trustPath}
	f.clock.Store(f.base)
	clock := func() time.Time { return time.Unix(f.clock.Load(), 0).UTC() }
	missing, err := pluginentitlements.New(t.Context(), db, []plugins.Installation{item}, entitlements.Development, trustPath, "", clock)
	require.NoError(t, err)
	f.server = &Server{Database: db, PluginEntitlements: missing}
	_, err = f.server.pluginEffectDatabase("acme", item, "effect.commit", false).BeginTx(t.Context(), nil)
	requirePlatformEntitlementCode(t, err, "plugin_not_entitled")
	f.grant = f.signGrant(t, uuid.New(), 1, f.base+10, f.base+20)
	bundleJSON, err := json.Marshal(map[string]any{"schema_version": 1, "grants": []string{base64.RawURLEncoding.EncodeToString(f.grant.Envelope)}, "revocations": []string{}})
	require.NoError(t, err)
	bundlePath := filepath.Join(tempDir, "development-bundle.json")
	require.NoError(t, os.WriteFile(bundlePath, bundleJSON, 0o600))
	f.adapter, err = pluginentitlements.New(t.Context(), db, []plugins.Installation{item}, entitlements.Development, trustPath, bundlePath, clock)
	require.NoError(t, err)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `CREATE TABLE entitlement_effect_probe(marker TEXT PRIMARY KEY)`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	return f
}

func (f *pluginEntitlementAPIFixture) sign(t *testing.T, claims map[uint64]any) []byte {
	t.Helper()
	mode, err := cbor.CoreDetEncOptions().EncMode()
	require.NoError(t, err)
	protected, err := mode.Marshal(map[int]any{1: -8, 4: []byte("development-key")})
	require.NoError(t, err)
	payload, err := mode.Marshal(claims)
	require.NoError(t, err)
	toSign, err := mode.Marshal([]any{"Signature1", protected, []byte{}, payload})
	require.NoError(t, err)
	raw, err := mode.Marshal(cbor.Tag{Number: 18, Content: []any{protected, map[int]any{}, payload, ed25519.Sign(f.private, toSign)}})
	require.NoError(t, err)
	return raw
}

func (f *pluginEntitlementAPIFixture) signGrant(t *testing.T, issuance uuid.UUID, sequence, expires, grace int64) entitlements.Grant {
	t.Helper()
	if sequence < 1 || f.base < 10 || expires < 0 || grace < 0 {
		t.Fatal("signed grant fixture requires a positive sequence and nonnegative timestamps")
		return entitlements.Grant{}
	}
	raw := f.sign(t, map[uint64]any{1: uint64(1), 2: "grant", 3: "development.example", 4: issuance[:], 5: uint64(sequence), 6: f.item.ID, 7: f.binding.InstallationID[:], 8: f.binding.OrganizationID[:], 9: []string{"feature-a"}, 10: map[string]uint64{}, 11: uint64(f.base - 10), 12: uint64(f.base - 5), 13: uint64(expires), 14: uint64(grace), 15: "development"})
	grant, err := entitlements.VerifyGrant(raw, f.trust, entitlements.Development, f.binding, time.Unix(f.base, 0))
	require.NoError(t, err)
	return grant
}

func (f *pluginEntitlementAPIFixture) signRevocation(t *testing.T, issuance uuid.UUID) entitlements.Revocations {
	t.Helper()
	if f.base < 0 {
		t.Fatal("signed revocation fixture requires a nonnegative timestamp")
		return entitlements.Revocations{}
	}
	raw := f.sign(t, map[uint64]any{1: uint64(1), 2: "revocations", 3: "development.example", 4: uint64(1), 5: uint64(f.base), 6: [][]byte{issuance[:]}, 7: "development"})
	snapshot, err := entitlements.VerifyRevocations(raw, f.trust, entitlements.Development, time.Unix(f.base, 0))
	require.NoError(t, err)
	return snapshot
}

func (f *pluginEntitlementAPIFixture) effectCount(t *testing.T, marker string) int {
	t.Helper()
	tx, err := f.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	var count int
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT count(*) FROM entitlement_effect_probe WHERE marker=$1`, marker).Scan(&count))
	return count
}

func TestPluginEffectDatabaseAuthorizesAndCommitsWithBusinessEffect(t *testing.T) {
	f := newPluginEntitlementAPIFixture(t)
	// The installed feature declaration and IAM service grant are still separate
	// from the signed licence; neither alone can create an effect.
	f.server.PluginEntitlements = f.adapter
	tx, err := f.server.pluginEffectDatabase("acme", f.item, "effect.commit", false).BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `INSERT INTO entitlement_effect_probe(marker) VALUES('committed')`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.Equal(t, 1, f.effectCount(t, "committed"))
	var activations int
	rawTx, err := f.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	require.NoError(t, rawTx.QueryRowContext(t.Context(), `SELECT count(*) FROM plugin_entitlement_feature_activations`).Scan(&activations))
	require.NoError(t, rawTx.Rollback())
	require.Equal(t, 1, activations)
	// A business failure cannot leave a separately committed entitlement
	// activation or effect from that same transaction.
	tx, err = f.server.pluginEffectDatabase("acme", f.item, "effect.commit", false).BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `INSERT INTO entitlement_effect_probe(marker) VALUES('rolled-back')`)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
	require.Zero(t, f.effectCount(t, "rolled-back"))
}

func TestPluginEffectDatabaseDenialPersistsMonotonicTimeAndCurrentTrust(t *testing.T) {
	f := newPluginEntitlementAPIFixture(t)
	f.server.PluginEntitlements = f.adapter
	tx, err := f.server.pluginEffectDatabase("acme", f.item, "effect.commit", false).BeginTx(t.Context(), nil)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	f.clock.Store(f.base + 10)
	_, err = f.server.pluginEffectDatabase("acme", f.item, "effect.commit", false).BeginTx(t.Context(), nil)
	requirePlatformEntitlementCode(t, err, "plugin_not_entitled")
	rawTx, err := f.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	var evaluated time.Time
	var grace time.Time
	require.NoError(t, rawTx.QueryRowContext(t.Context(), `SELECT evaluated_through,observed_grace_at FROM plugin_entitlement_bindings`).Scan(&evaluated, &grace))
	require.NoError(t, rawTx.Rollback())
	require.Equal(t, f.base+10, evaluated.Unix())
	require.Equal(t, f.base+10, grace.Unix())
	f.clock.Store(f.base)
	_, err = f.server.pluginEffectDatabase("acme", f.item, "effect.commit", false).BeginTx(t.Context(), nil)
	requirePlatformEntitlementCode(t, err, "plugin_not_entitled")
	// A new adapter with no pinned trust rechecks exact persisted COSE and
	// denies forward work without erasing prior activation for read/safety.
	untrusted, err := pluginentitlements.New(t.Context(), f.db, []plugins.Installation{f.item}, entitlements.Development, "", "", func() time.Time { return time.Unix(f.base, 0) })
	require.NoError(t, err)
	f.server.PluginEntitlements = untrusted
	_, err = f.server.pluginEffectDatabase("acme", f.item, "effect.commit", false).BeginTx(t.Context(), nil)
	requirePlatformEntitlementCode(t, err, "plugin_not_entitled")
	status, err := untrusted.ResolveEntitlement(t.Context(), "acme", f.item)
	require.NoError(t, err)
	require.True(t, status.Features["feature-a"].Read)
	require.True(t, status.Features["feature-a"].Safety)
	require.False(t, status.Features["feature-a"].Forward)
	require.Equal(t, "untrusted", status.Features["feature-a"].State)
}

func TestPluginEffectDatabaseSerializesRevocationWithBusinessCommit(t *testing.T) {
	f := newPluginEntitlementAPIFixture(t)
	f.server.PluginEntitlements = f.adapter
	tx, err := f.server.pluginEffectDatabase("acme", f.item, "effect.commit", false).BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `INSERT INTO entitlement_effect_probe(marker) VALUES('before-revocation')`)
	require.NoError(t, err)
	snapshot := f.signRevocation(t, f.grant.IssuanceID)
	done := make(chan error, 1)
	go func() {
		_, importErr := model.ImportPluginEntitlementRevocations(context.Background(), f.db, snapshot, "internal_system", sha256.Sum256([]byte("operator-file")))
		done <- importErr
	}()
	select {
	case err := <-done:
		t.Fatalf("revocation passed the open Core effect transaction: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	require.NoError(t, tx.Commit())
	require.NoError(t, <-done)
	require.Equal(t, 1, f.effectCount(t, "before-revocation"))
	_, err = f.server.pluginEffectDatabase("acme", f.item, "effect.commit", false).BeginTx(t.Context(), nil)
	requirePlatformEntitlementCode(t, err, "plugin_not_entitled")
}

func requirePlatformEntitlementCode(t *testing.T, err error, code string) {
	t.Helper()
	require.Error(t, err)
	var boundary *herrors.PlatformOrchestratorError
	require.ErrorAs(t, err, &boundary, "expected platform entitlement diagnostic, got %v", err)
	require.Equal(t, code, boundary.Code)
}
