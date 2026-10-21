package pluginentitlements

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/entitlements"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
)

const (
	testPlugin = "dev.example.workflow"
	// testNow is 2026-09-21T13:46:40Z; a constant keeps the CBOR claims exact.
	testNow = 1_790_000_000
)

type signer struct {
	kid     []byte
	private ed25519.PrivateKey
}

func newSigner(t *testing.T, kid string) (signer, ed25519.PublicKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return signer{kid: []byte(kid), private: private}, public
}

// sign builds a real COSE_Sign1 grant with the deterministic CBOR profile
// Core verifies. No verifier code is reused, so a verifier bug cannot hide.
func (s signer) sign(t *testing.T, binding entitlements.Binding, class entitlements.TrustClass) []byte {
	t.Helper()
	mode, err := cbor.CoreDetEncOptions().EncMode()
	require.NoError(t, err)
	issuance := uuid.New()
	base := uint64(testNow)
	payload, err := mode.Marshal(map[uint64]any{
		1: uint64(1), 2: "grant", 3: "issuer.example", 4: issuance[:], 5: uint64(1),
		6: binding.PluginID, 7: binding.InstallationID[:], 8: binding.OrganizationID[:],
		9: []string{"feature-a"}, 10: map[string]uint64{},
		11: base - 10, 12: base - 10, 13: base + 3600, 14: base + 7200, 15: string(class),
	})
	require.NoError(t, err)
	protected, err := mode.Marshal(map[int]any{1: -8, 4: s.kid})
	require.NoError(t, err)
	toSign, err := mode.Marshal([]any{"Signature1", protected, []byte{}, payload})
	require.NoError(t, err)
	raw, err := mode.Marshal(cbor.Tag{Number: 18, Content: []any{protected, map[int]any{}, payload, ed25519.Sign(s.private, toSign)}})
	require.NoError(t, err)
	return raw
}

// Each of these rejections used to stop Core at start-up. Now each skips only
// its own document, and the valid grant beside them is still accepted.
func TestVerifyBundleSkipsOnlyRejectedDocuments(t *testing.T) {
	now := time.Unix(testNow, 0).UTC()
	pinned, public := newSigner(t, "pinned-key")
	unpinned, _ := newSigner(t, "unpinned-key")
	trust := entitlements.TrustStore{Class: entitlements.Production, Issuers: []entitlements.Issuer{{
		ID: "issuer.example", PluginIDs: []string{testPlugin},
		Keys: []entitlements.Key{{ID: pinned.kid, PublicKey: public, Purposes: []entitlements.Purpose{entitlements.GrantPurpose, entitlements.RevocationPurpose}}},
	}}}
	installed := entitlements.Binding{PluginID: testPlugin, InstallationID: uuid.New(), OrganizationID: uuid.New(), DeclaredFeatures: []string{"feature-a"}}
	foreignInstallation, foreignOrganization := installed, installed
	foreignInstallation.InstallationID = uuid.New()
	foreignOrganization.OrganizationID = uuid.New()
	tampered := pinned.sign(t, installed, entitlements.Production)
	tampered[len(tampered)-80] ^= 0x01

	valid := pinned.sign(t, installed, entitlements.Production)
	bundle := Bundle{
		Grants: [][]byte{
			valid,
			pinned.sign(t, installed, entitlements.Development),
			pinned.sign(t, foreignInstallation, entitlements.Production),
			pinned.sign(t, foreignOrganization, entitlements.Production),
			unpinned.sign(t, installed, entitlements.Production),
			tampered,
		},
		Revocations: [][]byte{{0xd2, 0x80}},
	}
	grants, snapshots, diagnostics := verifyBundle(bundle, trust, entitlements.Production, []entitlements.Binding{installed}, now)

	require.Len(t, grants, 1)
	require.Equal(t, 0, grants[0].index)
	require.Equal(t, installed.InstallationID, grants[0].InstallationID)
	require.Empty(t, snapshots)
	expected := make([]Diagnostic, 0, 6)
	for i := 1; i <= 5; i++ {
		expected = append(expected, Diagnostic{DiagnosticGrantRejected, fmt.Sprintf("grant %d is invalid or has no exact installed binding", i)})
	}
	require.Equal(t, expected, diagnostics[:5])
	require.Len(t, diagnostics, 6)
	require.Equal(t, DiagnosticRevocationsRejected, diagnostics[5].Code)
}

func TestVerifyBundleWithoutInstalledBindingRejectsEveryGrant(t *testing.T) {
	now := time.Unix(testNow, 0).UTC()
	pinned, public := newSigner(t, "pinned-key")
	trust := entitlements.TrustStore{Class: entitlements.Production, Issuers: []entitlements.Issuer{{
		ID: "issuer.example", PluginIDs: []string{testPlugin},
		Keys: []entitlements.Key{{ID: pinned.kid, PublicKey: public, Purposes: []entitlements.Purpose{entitlements.GrantPurpose}}},
	}}}
	removed := entitlements.Binding{PluginID: testPlugin, InstallationID: uuid.New(), OrganizationID: uuid.New(), DeclaredFeatures: []string{"feature-a"}}
	grants, _, diagnostics := verifyBundle(Bundle{Grants: [][]byte{pinned.sign(t, removed, entitlements.Production)}}, trust, entitlements.Production, nil, now)
	require.Empty(t, grants)
	require.Equal(t, []Diagnostic{{DiagnosticGrantRejected, "grant 0 is invalid or has no exact installed binding"}}, diagnostics)
}

func TestLoadTrustRejectsMaterialWithoutStoppingCaller(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	malformed := filepath.Join(dir, "trust.json")
	require.NoError(t, os.WriteFile(malformed, []byte(`{"schema_version":1}`), 0o600))
	for name, path := range map[string]string{"missing": filepath.Join(dir, "absent.json"), "malformed": malformed} {
		t.Run(name, func(t *testing.T) {
			_, err := loadTrust(path, entitlements.Production)
			require.Error(t, err)
		})
	}
}

func TestImportBundleWithoutTrustOrAdapterIsADiagnostic(t *testing.T) {
	var unavailable *Adapter
	result, err := unavailable.ImportBundle(context.Background(), nil, []byte(`{}`), "operator")
	require.NoError(t, err)
	require.Equal(t, []Diagnostic{{DiagnosticBundleRejected, "the entitlement adapter is unavailable"}}, result.Diagnostics)
	untrusted := &Adapter{db: unavailableDatabase{}, mode: entitlements.Production, trust: entitlements.TrustStore{Class: entitlements.Production}}
	result, err = untrusted.ImportBundle(context.Background(), nil, []byte(`{}`), "operator")
	require.NoError(t, err)
	require.Equal(t, []Diagnostic{{DiagnosticBundleRejected, "the bundle was not imported because no issuer trust is configured"}}, result.Diagnostics)
	require.Zero(t, result.ImportedGrants+result.ImportedRevocations)
}

// unavailableDatabase is a Databaser that must never be reached: an import
// without trust stops before any database work.
type unavailableDatabase struct{ model.Databaser }
