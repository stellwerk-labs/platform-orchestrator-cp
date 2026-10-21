// Command plugin-entitlement-dev creates one disposable development-only issuer
// and grant. It is deliberately separate from the Core server entrypoint.
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/google/uuid"
	"github.com/veraison/go-cose"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/entitlements"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/pluginentitlements"
)

const maxDevelopmentValidity = 7 * 24 * time.Hour
const developmentIssuerTool = "stellwerk-plugin-entitlement-dev-v1"
const developmentTrustClass = "development"

type issuerMetadata struct {
	SchemaVersion    int      `json:"schema_version"`
	Tool             string   `json:"tool"`
	TrustClass       string   `json:"trust_class"`
	Issuer           string   `json:"issuer"`
	KeyID            string   `json:"kid"`
	PublicKeySHA256  string   `json:"public_key_sha256"`
	PluginID         string   `json:"plugin_id"`
	InstallationUUID string   `json:"installation_uuid"`
	OrganizationUUID string   `json:"organization_uuid"`
	Features         []string `json:"features"`
}

type outputFile struct {
	name string
	data []byte
}

func main() {
	if err := run(os.Args[1:], time.Now().UTC(), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "development entitlement issuance failed:", err)
		os.Exit(1)
	}
}

func run(args []string, now time.Time, stdout io.Writer) error {
	flags := flag.NewFlagSet("plugin-entitlement-dev", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	outDir := flags.String("out-dir", "", "new absolute private output directory")
	issuerDir := flags.String("issuer-dir", "", "original development issuer directory for renewal")
	previousBundle := flags.String("previous-bundle", "", "latest development bundle file for renewal")
	pluginID := flags.String("plugin-id", "", "installed reverse-DNS plugin ID")
	installationText := flags.String("installation-id", "", "installed immutable UUID")
	organizationText := flags.String("organization-id", "", "Core organization UUID")
	featuresText := flags.String("features", "", "sorted comma-separated manifest features")
	validFor := flags.Duration("valid-for", 24*time.Hour, "development validity, at most seven days")
	graceFor := flags.Duration("grace-for", 24*time.Hour, "read/safety grace, at most seven days")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errors.New("invalid flags; see docs/development-entitlement-tool.md")
	}
	if *outDir == "" || *pluginID == "" || *installationText == "" || *organizationText == "" || *featuresText == "" {
		return errors.New("output directory and exact plugin, installation, organization, and features are required")
	}
	if (*issuerDir == "") != (*previousBundle == "") {
		return errors.New("renewal requires both original issuer directory and latest signed bundle")
	}
	if *validFor < time.Second || *validFor > maxDevelopmentValidity || *graceFor < 0 || *graceFor > maxDevelopmentValidity || *validFor%time.Second != 0 || *graceFor%time.Second != 0 {
		return errors.New("validity and grace must be whole seconds and each at most seven days")
	}
	if now.IsZero() || now.Unix() < 0 {
		return errors.New("a nonnegative issuance clock is required")
	}
	installation, err := uuid.Parse(*installationText)
	if err != nil || installation.String() != *installationText || installation == uuid.Nil {
		return errors.New("installation ID must be a canonical nonzero UUID")
	}
	organization, err := uuid.Parse(*organizationText)
	if err != nil || organization.String() != *organizationText || organization == uuid.Nil {
		return errors.New("organization ID must be a canonical nonzero UUID")
	}
	features := strings.Split(*featuresText, ",")
	binding := entitlements.Binding{PluginID: *pluginID, InstallationID: installation, OrganizationID: organization, DeclaredFeatures: features}
	var files []outputFile
	if *issuerDir == "" {
		files, err = issueFiles(binding, now.UTC().Truncate(time.Second), *validFor, *graceFor)
	} else {
		files, err = renewFiles(binding, now.UTC().Truncate(time.Second), *validFor, *graceFor, *issuerDir, *previousBundle)
	}
	if err != nil {
		return err
	}
	if err := writePrivateDirectory(*outDir, files); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "Development-only entitlement written to %s\n", *outDir)
	return err
}

func issueFiles(binding entitlements.Binding, now time.Time, validFor, graceFor time.Duration) ([]outputFile, error) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	issuerRandom := make([]byte, 16)
	kidRandom := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, issuerRandom); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(rand.Reader, kidRandom); err != nil {
		return nil, err
	}
	issuer := "development." + hex.EncodeToString(issuerRandom)
	kid := []byte("development-" + hex.EncodeToString(kidRandom))
	envelope, err := signGrant(binding, now, validFor, graceFor, issuer, kid, private, 1)
	if err != nil {
		return nil, err
	}
	publicDER, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		return nil, err
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return nil, err
	}
	metadataJSON, err := json.MarshalIndent(developmentMetadata(binding, issuer, kid, public), "", "  ")
	if err != nil {
		return nil, err
	}
	issuedAt := now.Unix()
	trustJSON, err := json.MarshalIndent(map[string]any{
		"schema_version": 1, "trust_class": developmentTrustClass,
		"issuers": []any{map[string]any{
			"id": issuer, "plugin_ids": []string{binding.PluginID},
			"keys": []any{map[string]any{
				"kid":            base64.RawURLEncoding.EncodeToString(kid),
				"public_key_pem": string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})),
				"purposes":       []string{"grant", "revocation"}, "valid_from": issuedAt, "valid_until": int64(0),
			}},
		}},
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := validateGeneratedBundle(binding, trustJSON, envelope, now); err != nil {
		return nil, err
	}
	bundleJSON, err := bundleBytes(envelope)
	if err != nil {
		return nil, err
	}
	return []outputFile{
		{name: "issuer-private-development.pem", data: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})},
		{name: "issuer-development.json", data: append(metadataJSON, '\n')},
		{name: "trust-development.json", data: append(trustJSON, '\n')},
		{name: "bundle-development.json", data: append(bundleJSON, '\n')},
	}, nil
}

func developmentMetadata(binding entitlements.Binding, issuer string, kid []byte, public ed25519.PublicKey) issuerMetadata {
	digest := sha256.Sum256(public)
	return issuerMetadata{
		SchemaVersion: 1, Tool: developmentIssuerTool, TrustClass: developmentTrustClass,
		Issuer: issuer, KeyID: base64.RawURLEncoding.EncodeToString(kid), PublicKeySHA256: hex.EncodeToString(digest[:]),
		PluginID: binding.PluginID, InstallationUUID: binding.InstallationID.String(), OrganizationUUID: binding.OrganizationID.String(),
		Features: slices.Clone(binding.DeclaredFeatures),
	}
}

func renewFiles(binding entitlements.Binding, now time.Time, validFor, graceFor time.Duration, issuerDir, previousBundle string) ([]outputFile, error) {
	if !filepath.IsAbs(issuerDir) || filepath.Clean(issuerDir) != issuerDir {
		return nil, errors.New("original issuer directory must be a clean absolute path")
	}
	issuerInfo, err := os.Lstat(issuerDir)
	if err != nil || !issuerInfo.IsDir() || issuerInfo.Mode().Perm() != 0o700 {
		return nil, errors.New("original issuer directory must be private mode 0700")
	}
	trustJSON, err := readPrivateRegularFile(filepath.Join(issuerDir, "trust-development.json"), entitlements.MaxTrustJSONBytes)
	if err != nil {
		return nil, fmt.Errorf("read original development trust: %w", err)
	}
	trust, err := entitlements.ParseTrustJSON(trustJSON, entitlements.Development)
	if err != nil || len(trust.Issuers) != 1 || len(trust.Issuers[0].Keys) != 1 {
		return nil, errors.New("renewal requires one strict development issuer and key")
	}
	issuer := trust.Issuers[0]
	key := issuer.Keys[0]
	if !strings.HasPrefix(issuer.ID, "development.") || len(issuer.ID) != len("development.")+32 || !strings.HasPrefix(string(key.ID), "development-") || len(key.ID) != len("development-")+32 || !slices.Equal(issuer.PluginIDs, []string{binding.PluginID}) || !slices.Contains(key.Purposes, entitlements.GrantPurpose) {
		return nil, errors.New("issuer is not a tool-created development issuer for this plugin")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(issuer.ID, "development.")); err != nil {
		return nil, errors.New("invalid development issuer identity")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(string(key.ID), "development-")); err != nil {
		return nil, errors.New("invalid development key identity")
	}
	metadataJSON, err := readPrivateRegularFile(filepath.Join(issuerDir, "issuer-development.json"), 4096)
	if err != nil {
		return nil, fmt.Errorf("read original issuer metadata: %w", err)
	}
	var metadata issuerMetadata
	decoder := json.NewDecoder(bytes.NewReader(metadataJSON))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&metadata); err != nil {
		return nil, errors.New("invalid original issuer metadata")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF || !reflect.DeepEqual(metadata, developmentMetadata(binding, issuer.ID, key.ID, key.PublicKey)) {
		return nil, errors.New("original issuer metadata does not match exact development binding and key")
	}
	privatePEM, err := readPrivateRegularFile(filepath.Join(issuerDir, "issuer-private-development.pem"), 4096)
	if err != nil {
		return nil, fmt.Errorf("read original development key: %w", err)
	}
	privateBlock, rest := pem.Decode(privatePEM)
	if privateBlock == nil || privateBlock.Type != "PRIVATE KEY" || len(privateBlock.Headers) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("original issuer key must be one PKCS#8 private key")
	}
	parsedKey, err := x509.ParsePKCS8PrivateKey(privateBlock.Bytes)
	if err != nil {
		return nil, errors.New("invalid original development private key")
	}
	private, ok := parsedKey.(ed25519.PrivateKey)
	if !ok || !bytes.Equal(private.Public().(ed25519.PublicKey), key.PublicKey) {
		return nil, errors.New("original private key does not match pinned development trust")
	}
	previousJSON, err := readPrivateRegularFile(previousBundle, pluginentitlements.MaxBundleBytes)
	if err != nil {
		return nil, fmt.Errorf("read prior development bundle: %w", err)
	}
	previous, err := pluginentitlements.ParseBundleJSON(previousJSON)
	if err != nil || len(previous.Grants) != 1 || len(previous.Revocations) != 0 {
		return nil, errors.New("renewal requires one prior grant and no revocation documents")
	}
	prior, err := entitlements.VerifyGrant(previous.Grants[0], trust, entitlements.Development, binding, now)
	if err != nil || prior.Issuer != issuer.ID || !bytes.Equal(prior.KeyID, key.ID) || !slices.Equal(prior.Features, binding.DeclaredFeatures) {
		return nil, errors.New("prior grant is not an exact development issuer, key and binding match")
	}
	if prior.Sequence == math.MaxInt64 || now.Unix() < prior.IssuedAt || now.Add(validFor).Unix() <= prior.ExpiresAt {
		return nil, errors.New("renewal requires a higher sequence, monotonic issuance time and later expiry")
	}
	envelope, err := signGrant(binding, now, validFor, graceFor, issuer.ID, key.ID, private, prior.Sequence+1)
	if err != nil {
		return nil, err
	}
	if err := validateGeneratedBundle(binding, trustJSON, envelope, now); err != nil {
		return nil, err
	}
	bundleJSON, err := bundleBytes(envelope)
	if err != nil {
		return nil, err
	}
	return []outputFile{{name: "bundle-development.json", data: append(bundleJSON, '\n')}}, nil
}

func signGrant(binding entitlements.Binding, now time.Time, validFor, graceFor time.Duration, issuer string, kid []byte, private ed25519.PrivateKey, sequence int64) ([]byte, error) {
	issuance, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}
	issuedAt := now.Unix()
	if issuedAt < 0 || issuedAt > math.MaxInt64-int64(2*maxDevelopmentValidity/time.Second) {
		return nil, errors.New("issuance clock is out of range")
	}
	// #nosec G115 -- the Unix second and both bounded durations were checked above.
	issuedSeconds := uint64(issuedAt)
	// #nosec G115 -- the sum is nonnegative and bounded by MaxInt64 above.
	expiresSeconds := uint64(issuedAt + int64(validFor/time.Second))
	// #nosec G115 -- the sum is nonnegative and bounded by MaxInt64 above.
	graceSeconds := uint64(issuedAt + int64((validFor+graceFor)/time.Second))
	mode, err := cbor.CoreDetEncOptions().EncMode()
	if err != nil {
		return nil, err
	}
	payload, err := mode.Marshal(map[uint64]any{
		1: uint64(1), 2: "grant", 3: issuer, 4: issuance[:], 5: sequence,
		6: binding.PluginID, 7: binding.InstallationID[:], 8: binding.OrganizationID[:],
		9: binding.DeclaredFeatures, 10: map[string]uint64{},
		11: issuedSeconds, 12: issuedSeconds,
		13: expiresSeconds, 14: graceSeconds, 15: developmentTrustClass,
	})
	if err != nil {
		return nil, err
	}
	signer, err := cose.NewSigner(cose.AlgorithmEdDSA, private)
	if err != nil {
		return nil, err
	}
	message := cose.NewSign1Message()
	message.Headers.Protected.SetAlgorithm(cose.AlgorithmEdDSA)
	message.Headers.Protected[cose.HeaderLabelKeyID] = kid
	message.Payload = payload
	if err := message.Sign(rand.Reader, nil, signer); err != nil {
		return nil, err
	}
	envelope, err := message.MarshalCBOR()
	if err != nil {
		return nil, err
	}
	return envelope, nil
}

func bundleBytes(envelope []byte) ([]byte, error) {
	return json.MarshalIndent(map[string]any{
		"schema_version": 1,
		"grants":         []string{base64.RawURLEncoding.EncodeToString(envelope)},
		"revocations":    []string{},
	}, "", "  ")
}

func validateGeneratedBundle(binding entitlements.Binding, trustJSON, envelope []byte, now time.Time) error {
	bundleJSON, err := bundleBytes(envelope)
	if err != nil {
		return err
	}
	trust, err := entitlements.ParseTrustJSON(trustJSON, entitlements.Development)
	if err != nil {
		return fmt.Errorf("generated development trust rejected: %w", err)
	}
	bundle, err := pluginentitlements.ParseBundleJSON(bundleJSON)
	if err != nil || len(bundle.Grants) != 1 {
		return errors.New("generated development bundle rejected")
	}
	if _, err := entitlements.VerifyGrant(bundle.Grants[0], trust, entitlements.Development, binding, now); err != nil {
		return fmt.Errorf("generated signed grant rejected: %w", err)
	}
	return nil
}

func writePrivateDirectory(dir string, files []outputFile) (err error) {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || dir == string(filepath.Separator) {
		return errors.New("output must be a new absolute directory")
	}
	parent := filepath.Dir(dir)
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil || resolved != parent {
		return errors.New("output parent must exist without symlinks")
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return fmt.Errorf("create new private output directory: %w", err)
	}
	created := []string{}
	defer func() {
		if err == nil {
			return
		}
		for _, name := range created {
			if removeErr := os.Remove(filepath.Join(dir, name)); removeErr != nil {
				err = errors.Join(err, removeErr)
			}
		}
		if removeErr := os.Remove(dir); removeErr != nil {
			err = errors.Join(err, removeErr)
		}
	}()
	// #nosec G302 -- this is an exclusively created private directory, not a file.
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	for _, item := range files {
		if item.name != filepath.Base(item.name) || item.name == "." || item.name == ".." {
			return errors.New("invalid output filename")
		}
		path := filepath.Join(dir, item.name)
		// #nosec G304 -- dir is newly created and private; name is a validated basename.
		file, openErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if openErr != nil {
			return fmt.Errorf("create exclusive output file: %w", openErr)
		}
		created = append(created, item.name)
		if writeErr := file.Chmod(0o600); writeErr != nil {
			_ = file.Close()
			return writeErr
		}
		written, writeErr := file.Write(item.data)
		if writeErr != nil || written != len(item.data) {
			_ = file.Close()
			if writeErr != nil {
				return writeErr
			}
			return io.ErrShortWrite
		}
		if syncErr := file.Sync(); syncErr != nil {
			_ = file.Close()
			return syncErr
		}
		if closeErr := file.Close(); closeErr != nil {
			return closeErr
		}
	}
	return nil
}
