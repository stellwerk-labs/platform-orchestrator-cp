package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/entitlements"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/pluginentitlements"
)

func developmentArgs(dir string, installation, organization uuid.UUID) []string {
	return []string{
		"-out-dir", dir,
		"-plugin-id", "dev.example.workflow",
		"-installation-id", installation.String(),
		"-organization-id", organization.String(),
		"-features", "feature-a,feature-b",
		"-valid-for", "2h", "-grace-for", "1h",
	}
}

func TestDevelopmentIssuerGeneratesOnlyIsolatedVerifiedEvidence(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	dir := filepath.Join(parent, "fresh-development-issuer")
	installation, organization := uuid.New(), uuid.New()
	now := time.Now().UTC().Truncate(time.Second)
	var output bytes.Buffer
	require.NoError(t, run(developmentArgs(dir, installation, organization), now, &output))
	require.Equal(t, "Development-only entitlement written to "+dir+"\n", output.String())

	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	for _, name := range []string{"issuer-private-development.pem", "issuer-development.json", "trust-development.json", "bundle-development.json"} {
		info, err := os.Lstat(filepath.Join(dir, name))
		require.NoError(t, err)
		require.True(t, info.Mode().IsRegular())
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
	// #nosec G304 -- the fixture directory was exclusively created by this test.
	privateBytes, err := os.ReadFile(filepath.Join(dir, "issuer-private-development.pem"))
	require.NoError(t, err)
	block, rest := pem.Decode(privateBytes)
	require.Equal(t, "PRIVATE KEY", block.Type)
	require.Empty(t, rest)
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	require.NoError(t, err)
	_, ok := key.(ed25519.PrivateKey)
	require.True(t, ok)

	// #nosec G304 -- the fixture directory was exclusively created by this test.
	trustBytes, err := os.ReadFile(filepath.Join(dir, "trust-development.json"))
	require.NoError(t, err)
	trust, err := entitlements.ParseTrustJSON(trustBytes, entitlements.Development)
	require.NoError(t, err)
	require.Equal(t, entitlements.Development, trust.Class)
	require.Len(t, trust.Issuers, 1)
	require.True(t, strings.HasPrefix(trust.Issuers[0].ID, "development."))
	_, err = entitlements.ParseTrustJSON(trustBytes, entitlements.Production)
	require.Error(t, err)

	// #nosec G304 -- the fixture directory was exclusively created by this test.
	bundleBytes, err := os.ReadFile(filepath.Join(dir, "bundle-development.json"))
	require.NoError(t, err)
	bundle, err := pluginentitlements.ParseBundleJSON(bundleBytes)
	require.NoError(t, err)
	require.Len(t, bundle.Grants, 1)
	require.Empty(t, bundle.Revocations)
	binding := entitlements.Binding{PluginID: "dev.example.workflow", InstallationID: installation, OrganizationID: organization, DeclaredFeatures: []string{"feature-a", "feature-b"}}
	grant, err := entitlements.VerifyGrant(bundle.Grants[0], trust, entitlements.Development, binding, now)
	require.NoError(t, err)
	require.Equal(t, int64(1), grant.Sequence)
	require.Equal(t, now.Unix(), grant.IssuedAt)
	require.Equal(t, now.Add(2*time.Hour).Unix(), grant.ExpiresAt)
	require.Equal(t, now.Add(3*time.Hour).Unix(), grant.GraceUntil)
	require.Equal(t, []string{"feature-a", "feature-b"}, grant.EffectiveFeatures)
	_, err = entitlements.VerifyGrant(bundle.Grants[0], trust, entitlements.Production, binding, now)
	require.Error(t, err)
	wrongBinding := binding
	wrongBinding.OrganizationID = uuid.New()
	_, err = entitlements.VerifyGrant(bundle.Grants[0], trust, entitlements.Development, wrongBinding, now)
	require.Error(t, err)
	require.NotContains(t, output.String(), string(privateBytes))
}

func TestDevelopmentIssuerRejectsOverwriteSymlinkAndInvalidGrant(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	installation, organization := uuid.New(), uuid.New()
	now := time.Now().UTC().Truncate(time.Second)
	dir := filepath.Join(parent, "new")
	args := developmentArgs(dir, installation, organization)
	require.NoError(t, run(args, now, &bytes.Buffer{}))
	privatePath := filepath.Join(dir, "issuer-private-development.pem")
	// #nosec G304 -- privatePath is the fixed fixture filename.
	before, err := os.ReadFile(privatePath)
	require.NoError(t, err)
	require.Error(t, run(args, now, &bytes.Buffer{}))
	// #nosec G304 -- privatePath is the fixed fixture filename.
	after, err := os.ReadFile(privatePath)
	require.NoError(t, err)
	require.Equal(t, before, after)

	symlinkParent := filepath.Join(parent, "linked")
	require.NoError(t, os.Symlink(parent, symlinkParent))
	require.Error(t, run(developmentArgs(filepath.Join(symlinkParent, "forbidden"), installation, organization), now, &bytes.Buffer{}))
	_, err = os.Lstat(filepath.Join(parent, "forbidden"))
	require.True(t, os.IsNotExist(err))

	invalid := developmentArgs(filepath.Join(parent, "invalid"), installation, organization)
	invalid[9] = "feature-b,feature-a"
	require.Error(t, run(invalid, now, &bytes.Buffer{}))
	_, err = os.Lstat(filepath.Join(parent, "invalid"))
	require.True(t, os.IsNotExist(err))

	tooLong := developmentArgs(filepath.Join(parent, "too-long"), installation, organization)
	tooLong[11] = "168h1s"
	require.Error(t, run(tooLong, now, &bytes.Buffer{}))
	_, err = os.Lstat(filepath.Join(parent, "too-long"))
	require.True(t, os.IsNotExist(err))
}

func TestDevelopmentIssuerCleansPartialOutputAfterExclusiveCreateFailure(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	dir := filepath.Join(parent, "partial")
	err = writePrivateDirectory(dir, []outputFile{{name: "collision", data: []byte("first")}, {name: "collision", data: []byte("second")}})
	require.Error(t, err)
	_, err = os.Lstat(dir)
	require.True(t, os.IsNotExist(err))
}

func TestDevelopmentIssuerRenewsSameIssuerWithHigherSequence(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	installation, organization := uuid.New(), uuid.New()
	now := time.Now().UTC().Truncate(time.Second)
	issuerDir := filepath.Join(parent, "original-issuer")
	require.NoError(t, run(developmentArgs(issuerDir, installation, organization), now, &bytes.Buffer{}))
	trustBytes, err := readPrivateRegularFile(filepath.Join(issuerDir, "trust-development.json"), entitlements.MaxTrustJSONBytes)
	require.NoError(t, err)
	trust, err := entitlements.ParseTrustJSON(trustBytes, entitlements.Development)
	require.NoError(t, err)
	binding := entitlements.Binding{PluginID: "dev.example.workflow", InstallationID: installation, OrganizationID: organization, DeclaredFeatures: []string{"feature-a", "feature-b"}}
	verifyBundle := func(path string, at time.Time) entitlements.Grant {
		t.Helper()
		raw, readErr := readPrivateRegularFile(path, pluginentitlements.MaxBundleBytes)
		require.NoError(t, readErr)
		bundle, parseErr := pluginentitlements.ParseBundleJSON(raw)
		require.NoError(t, parseErr)
		require.Len(t, bundle.Grants, 1)
		grant, verifyErr := entitlements.VerifyGrant(bundle.Grants[0], trust, entitlements.Development, binding, at)
		require.NoError(t, verifyErr)
		return grant
	}
	firstBundle := filepath.Join(issuerDir, "bundle-development.json")
	first := verifyBundle(firstBundle, now)
	renew := func(dir, previous string, at time.Time) (entitlements.Grant, string) {
		t.Helper()
		args := append(developmentArgs(dir, installation, organization), "-issuer-dir", issuerDir, "-previous-bundle", previous)
		require.NoError(t, run(args, at, &bytes.Buffer{}))
		info, statErr := os.Stat(dir)
		require.NoError(t, statErr)
		require.Equal(t, os.FileMode(0o700), info.Mode().Perm())
		entries, readErr := os.ReadDir(dir)
		require.NoError(t, readErr)
		require.Len(t, entries, 1, "renewal must not duplicate the issuer private key")
		require.Equal(t, "bundle-development.json", entries[0].Name())
		path := filepath.Join(dir, "bundle-development.json")
		info, statErr = os.Stat(path)
		require.NoError(t, statErr)
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
		return verifyBundle(path, at), path
	}
	second, secondBundle := renew(filepath.Join(parent, "second"), firstBundle, now.Add(3*time.Hour))
	require.Equal(t, first.Issuer, second.Issuer)
	require.Equal(t, first.KeyID, second.KeyID)
	require.NotEqual(t, first.IssuanceID, second.IssuanceID)
	require.Equal(t, int64(2), second.Sequence)
	require.Greater(t, second.ExpiresAt, first.ExpiresAt)
	third, _ := renew(filepath.Join(parent, "third"), secondBundle, now.Add(4*time.Hour))
	require.Equal(t, int64(3), third.Sequence)
	require.Equal(t, first.Issuer, third.Issuer)
	require.Greater(t, third.ExpiresAt, second.ExpiresAt)
	_, err = entitlements.VerifyGrant(second.Envelope, entitlements.TrustStore{Class: entitlements.Production, Issuers: trust.Issuers}, entitlements.Production, binding, now.Add(3*time.Hour))
	require.Error(t, err, "copying development public trust cannot reclassify the renewed bytes")
}

func TestDevelopmentIssuerRenewalRejectsWrongInputWithoutOutput(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	installation, organization := uuid.New(), uuid.New()
	now := time.Now().UTC().Truncate(time.Second)
	issuerDir := filepath.Join(parent, "original-issuer")
	require.NoError(t, run(developmentArgs(issuerDir, installation, organization), now, &bytes.Buffer{}))
	previous := filepath.Join(issuerDir, "bundle-development.json")
	cases := []struct {
		name   string
		modify func([]string) []string
		at     time.Time
	}{
		{name: "missing previous bundle", modify: func(args []string) []string { return append(args, "-issuer-dir", issuerDir) }, at: now.Add(3 * time.Hour)},
		{name: "wrong organization", modify: func(args []string) []string {
			args[7] = uuid.NewString()
			return append(args, "-issuer-dir", issuerDir, "-previous-bundle", previous)
		}, at: now.Add(3 * time.Hour)},
		{name: "feature expansion", modify: func(args []string) []string {
			args[9] = "feature-a,feature-b,feature-c"
			return append(args, "-issuer-dir", issuerDir, "-previous-bundle", previous)
		}, at: now.Add(3 * time.Hour)},
		{name: "clock rollback", modify: func(args []string) []string {
			return append(args, "-issuer-dir", issuerDir, "-previous-bundle", previous)
		}, at: now.Add(-time.Hour)},
		{name: "no expiry extension", modify: func(args []string) []string {
			return append(args, "-issuer-dir", issuerDir, "-previous-bundle", previous)
		}, at: now},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			dir := filepath.Join(parent, strings.ReplaceAll(item.name, " ", "-"))
			args := item.modify(developmentArgs(dir, installation, organization))
			require.Error(t, run(args, item.at, &bytes.Buffer{}))
			_, statErr := os.Lstat(dir)
			require.True(t, os.IsNotExist(statErr))
		})
	}
	symlink := filepath.Join(parent, "bundle-link")
	require.NoError(t, os.Symlink(previous, symlink))
	dir := filepath.Join(parent, "symlink-renewal")
	args := append(developmentArgs(dir, installation, organization), "-issuer-dir", issuerDir, "-previous-bundle", symlink)
	require.Error(t, run(args, now.Add(3*time.Hour), &bytes.Buffer{}))
	_, err = os.Lstat(dir)
	require.True(t, os.IsNotExist(err))
	metadataPath := filepath.Join(issuerDir, "issuer-development.json")
	// #nosec G304 -- metadataPath names the fixed file in this private test fixture.
	metadata, err := os.ReadFile(metadataPath)
	require.NoError(t, err)
	// #nosec G703 -- metadataPath is the fixed file in the isolated fixture directory.
	require.NoError(t, os.WriteFile(metadataPath, bytes.Replace(metadata, []byte("dev.example.workflow"), []byte("dev.example.relabelled"), 1), 0o600))
	dir = filepath.Join(parent, "altered-metadata")
	args = append(developmentArgs(dir, installation, organization), "-issuer-dir", issuerDir, "-previous-bundle", previous)
	require.Error(t, run(args, now.Add(3*time.Hour), &bytes.Buffer{}))
	_, err = os.Lstat(dir)
	require.True(t, os.IsNotExist(err))
}
