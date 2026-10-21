package entitlements

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/google/uuid"
	"github.com/veraison/go-cose"
)

var (
	testInstallation = uuid.MustParse("4ec3e35b-80fb-426f-aeb5-96bcd0b2f94c")
	testOrganization = uuid.MustParse("680e8afd-b0f8-4d13-b64a-22f4aca4118b")
	testIssuance     = uuid.MustParse("ec3dd342-05b2-4c0a-b392-9fa998e1c81d")
)

func fixture(t *testing.T) (ed25519.PrivateKey, TrustStore, Binding) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	trust := TrustStore{Class: Production, Issuers: []Issuer{{
		ID: "issuer.example", PluginIDs: []string{"dev.stellwerk.progressive-rollouts"},
		Keys: []Key{{ID: []byte("test-key"), PublicKey: public, Purposes: []Purpose{GrantPurpose, RevocationPurpose}}},
	}}}
	binding := Binding{PluginID: "dev.stellwerk.progressive-rollouts", InstallationID: testInstallation, OrganizationID: testOrganization, DeclaredFeatures: []string{"progressive-rollouts"}}
	return private, trust, binding
}

func grantClaims() map[uint64]any {
	return map[uint64]any{
		1: uint64(1), 2: "grant", 3: "issuer.example", 4: testIssuance[:], 5: uint64(1),
		6: "dev.stellwerk.progressive-rollouts", 7: testInstallation[:], 8: testOrganization[:],
		9: []string{"progressive-rollouts"}, 10: map[string]uint64{},
		11: uint64(100), 12: uint64(100), 13: uint64(200), 14: uint64(250), 15: "production",
	}
}

func encoded(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := encodeMode.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// This signs RFC 9052 Sig_structure directly with the standard library. It is
// independent of go-cose's Sign method, which the verifier uses for Verify.
func signIndependent(t *testing.T, payload []byte, private ed25519.PrivateKey, protected []byte) []byte {
	t.Helper()
	toSign := encoded(t, []any{"Signature1", protected, []byte{}, payload})
	signature := ed25519.Sign(private, toSign)
	return encoded(t, cbor.Tag{Number: 18, Content: []any{protected, map[int]any{}, payload, signature}})
}

func signWithAAD(t *testing.T, payload []byte, private ed25519.PrivateKey, protected, aad []byte) []byte {
	t.Helper()
	signature := ed25519.Sign(private, encoded(t, []any{"Signature1", protected, aad, payload}))
	return encoded(t, cbor.Tag{Number: 18, Content: []any{protected, map[int]any{}, payload, signature}})
}

func validEnvelope(t *testing.T, claims any, private ed25519.PrivateKey) []byte {
	t.Helper()
	protected := encoded(t, map[int]any{1: -8, 4: []byte("test-key")})
	return signIndependent(t, encoded(t, claims), private, protected)
}

func TestVerifyGrantIndependentSignatureAndTimeBoundaries(t *testing.T) {
	private, trust, binding := fixture(t)
	raw := validEnvelope(t, grantClaims(), private)
	grant, err := VerifyGrant(raw, trust, Production, binding, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if grant.IssuanceID != testIssuance || !bytes.Equal(grant.Envelope, raw) || grant.Digest == [32]byte{} || len(grant.EffectiveFeatures) != 1 {
		t.Fatalf("incorrect verified projection: %+v", grant)
	}
	for _, tc := range []struct {
		now  int64
		want TimeState
	}{
		{99, NotYetValid}, {100, Valid}, {199, Valid}, {200, Grace}, {249, Grace}, {250, Expired},
	} {
		if got := EvaluateTime(grant, tc.now); got != tc.want {
			t.Errorf("now %d: got %s, want %s", tc.now, got, tc.want)
		}
	}
	// Authentic historical grants are evidence even after expiry.
	if _, err := VerifyGrant(raw, trust, Production, binding, time.Unix(1000, 0)); err != nil {
		t.Fatal(err)
	}
	trust.Class = Development
	if _, err := VerifyGrant(raw, trust, Production, binding, time.Unix(100, 0)); err == nil {
		t.Fatal("production accepted development trust")
	}
	if _, err := VerifyGrant(raw, trust, Development, binding, time.Unix(100, 0)); err == nil {
		t.Fatal("development accepted a production-signed document")
	}
}

func TestGoCOSESignerInteroperability(t *testing.T) {
	private, trust, binding := fixture(t)
	signer, err := cose.NewSigner(cose.AlgorithmEdDSA, private)
	if err != nil {
		t.Fatal(err)
	}
	message := cose.NewSign1Message()
	message.Headers.Protected.SetAlgorithm(cose.AlgorithmEdDSA)
	message.Headers.Protected[cose.HeaderLabelKeyID] = []byte("test-key")
	message.Payload = encoded(t, grantClaims())
	if err := message.Sign(rand.Reader, nil, signer); err != nil {
		t.Fatal(err)
	}
	raw, err := message.MarshalCBOR()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyGrant(raw, trust, Production, binding, time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyGrantRejectsMalformedAndMismatched(t *testing.T) {
	private, trust, binding := fixture(t)
	good := validEnvelope(t, grantClaims(), private)
	badSig := bytes.Clone(good)
	badSig[len(badSig)-1] ^= 0x01
	untagged := bytes.Clone(good[1:])
	wrongTag := bytes.Clone(good)
	wrongTag[0] = 0xd3
	trailing := append(bytes.Clone(good), 0)
	badHeader := signIndependent(t, encoded(t, grantClaims()), private, encoded(t, map[int]any{1: -7, 4: []byte("test-key")}))
	unknownHeader := signIndependent(t, encoded(t, grantClaims()), private, encoded(t, map[int]any{1: -8, 4: []byte("test-key"), 9: true}))
	duplicateHeader := signIndependent(t, encoded(t, grantClaims()), private, []byte{0xa3, 0x01, 0x27, 0x01, 0x27, 0x04, 0x48, 't', 'e', 's', 't', '-', 'k', 'e', 'y'})
	missingKid := signIndependent(t, encoded(t, grantClaims()), private, encoded(t, map[int]any{1: -8}))
	wrongKid := signIndependent(t, encoded(t, grantClaims()), private, encoded(t, map[int]any{1: -8, 4: []byte("other-key")}))
	withAAD := signWithAAD(t, encoded(t, grantClaims()), private, encoded(t, map[int]any{1: -8, 4: []byte("test-key")}), []byte("unexpected"))
	unprotected := encoded(t, cbor.Tag{Number: 18, Content: []any{
		encoded(t, map[int]any{1: -8, 4: []byte("test-key")}), map[int]any{4: []byte("test-key")}, encoded(t, grantClaims()), make([]byte, ed25519.SignatureSize),
	}})
	detached := encoded(t, cbor.Tag{Number: 18, Content: []any{
		encoded(t, map[int]any{1: -8, 4: []byte("test-key")}), map[int]any{}, nil, make([]byte, ed25519.SignatureSize),
	}})
	badPayload := grantClaims()
	badPayload[7] = testOrganization[:]
	wrongInstallation := validEnvelope(t, badPayload, private)
	badPayload = grantClaims()
	badPayload[9] = []string{"z", "a"}
	unsortedFeatures := validEnvelope(t, badPayload, private)
	badPayload = grantClaims()
	badPayload[9] = []string{"progressive-rollouts", "progressive-rollouts"}
	duplicateFeatures := validEnvelope(t, badPayload, private)
	badPayload = grantClaims()
	badPayload[8] = []byte{1, 2}
	badUUID := validEnvelope(t, badPayload, private)
	badPayload = grantClaims()
	badPayload[10] = map[string]uint64{"max-waves": 1}
	unknownLimit := validEnvelope(t, badPayload, private)
	badPayload = grantClaims()
	badPayload[16] = uint64(1)
	unknownClaim := validEnvelope(t, badPayload, private)
	badPayload = grantClaims()
	delete(badPayload, 15)
	missingClass := validEnvelope(t, badPayload, private)
	badPayload = grantClaims()
	badPayload[15] = "staging"
	invalidClass := validEnvelope(t, badPayload, private)
	badPayload = grantClaims()
	badPayload[11] = uint64(131)
	futureIssued := validEnvelope(t, badPayload, private)
	for name, raw := range map[string][]byte{
		"signature": badSig, "untagged": untagged, "wrong tag": wrongTag, "trailing": trailing, "wrong algorithm": badHeader,
		"unknown header": unknownHeader, "duplicate header": duplicateHeader, "missing kid": missingKid,
		"unknown kid": wrongKid, "unexpected AAD": withAAD, "unprotected header": unprotected, "detached payload": detached,
		"wrong binding": wrongInstallation, "unsorted features": unsortedFeatures, "duplicate features": duplicateFeatures,
		"bad UUID":      badUUID,
		"unknown limit": unknownLimit, "unknown claim": unknownClaim, "missing trust class": missingClass,
		"invalid trust class": invalidClass, "issued_at skew": futureIssued,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := VerifyGrant(raw, trust, Production, binding, time.Unix(100, 0)); err == nil {
				t.Fatal("accepted invalid grant")
			}
		})
	}
	unknown := trust
	unknown.Issuers = nil
	if _, err := VerifyGrant(good, unknown, Production, binding, time.Unix(100, 0)); err == nil {
		t.Fatal("accepted unknown issuer")
	}
	wrongOrg := binding
	wrongOrg.OrganizationID = uuid.New()
	if _, err := VerifyGrant(good, trust, Production, wrongOrg, time.Unix(100, 0)); err == nil {
		t.Fatal("accepted wrong organization")
	}
}

func TestStrictCBORProfile(t *testing.T) {
	private, trust, binding := fixture(t)
	protected := encoded(t, map[int]any{1: -8, 4: []byte("test-key")})
	canonical := encoded(t, grantClaims())
	// The map count is 15. Append a second key 1 with its original value.
	duplicate := append(bytes.Clone(canonical), 0x01, 0x01)
	duplicate[0] = 0xb0
	// Claim 1 uses non-minimal integer width for its value.
	nonminimal := bytes.Replace(bytes.Clone(canonical), []byte{0x01, 0x01}, []byte{0x01, 0x18, 0x01}, 1)
	// A map with indefinite length, kept signed so only CBOR policy can reject it.
	indefinite := append([]byte{0xbf}, canonical[1:]...)
	indefinite = append(indefinite, 0xff)
	for name, payload := range map[string][]byte{
		"duplicate payload key": duplicate,
		"nonminimal integer":    nonminimal,
		"indefinite map":        indefinite,
		"invalid utf8":          bytes.Replace(bytes.Clone(canonical), []byte("issuer.example"), append([]byte{0xff}, []byte("ssuer.example")...), 1),
	} {
		t.Run(name, func(t *testing.T) {
			raw := signIndependent(t, payload, private, protected)
			if _, err := VerifyGrant(raw, trust, Production, binding, time.Unix(100, 0)); err == nil {
				t.Fatal("accepted malformed signed payload")
			}
		})
	}
	// Non-minimal tag 18 must be rejected before COSE decoding.
	badTag, _ := hex.DecodeString("d812")
	badTag = append(badTag, signIndependent(t, canonical, private, protected)[1:]...)
	if _, err := VerifyGrant(badTag, trust, Production, binding, time.Unix(100, 0)); err == nil {
		t.Fatal("accepted nonminimal tag")
	}
	oversized := bytes.Repeat([]byte{0}, MaxEnvelopeBytes+1)
	if _, err := VerifyGrant(oversized, trust, Production, binding, time.Unix(100, 0)); err == nil {
		t.Fatal("accepted oversize envelope")
	}
}

func TestVerifyRevocationsAndPurposeIsolation(t *testing.T) {
	private, trust, _ := fixture(t)
	claims := map[uint64]any{1: uint64(1), 2: "revocations", 3: "issuer.example", 4: uint64(2), 5: uint64(100), 6: [][]byte{testIssuance[:]}, 7: "production"}
	raw := validEnvelope(t, claims, private)
	snapshot, err := VerifyRevocations(raw, trust, Production, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Sequence != 2 || len(snapshot.IssuanceIDs) != 1 || snapshot.IssuanceIDs[0] != testIssuance || !bytes.Equal(snapshot.Envelope, raw) {
		t.Fatalf("wrong verified snapshot: %+v", snapshot)
	}
	trust.Issuers[0].Keys[0].Purposes = []Purpose{GrantPurpose}
	if _, err := VerifyRevocations(raw, trust, Production, time.Unix(100, 0)); err == nil {
		t.Fatal("grant-only key signed revocations")
	}
	trust.Issuers[0].Keys[0].Purposes = []Purpose{GrantPurpose, RevocationPurpose}
	claims[6] = [][]byte{testIssuance[:], testIssuance[:]}
	if _, err := VerifyRevocations(validEnvelope(t, claims, private), trust, Production, time.Unix(100, 0)); err == nil {
		t.Fatal("accepted duplicate revocation ID")
	}
}

func TestSignedTrustClassCannotBeReclassifiedByCopyingPublicKey(t *testing.T) {
	private, productionTrust, binding := fixture(t)
	developmentTrust := productionTrust
	developmentTrust.Class = Development
	grantClaims := grantClaims()
	grantClaims[15] = "development"
	grantRaw := validEnvelope(t, grantClaims, private)
	if _, err := VerifyGrant(grantRaw, developmentTrust, Development, binding, time.Unix(100, 0)); err != nil {
		t.Fatalf("development grant rejected: %v", err)
	}
	// The same issuer, kid and public key in a production-labelled trust store
	// cannot promote the exact development-signed bytes to production authority.
	if _, err := VerifyGrant(grantRaw, productionTrust, Production, binding, time.Unix(100, 0)); err == nil {
		t.Fatal("copied development key promoted its signed grant")
	}
	revocationClaims := map[uint64]any{1: uint64(1), 2: "revocations", 3: "issuer.example", 4: uint64(1), 5: uint64(100), 6: [][]byte{testIssuance[:]}, 7: "development"}
	revocationRaw := validEnvelope(t, revocationClaims, private)
	if _, err := VerifyRevocations(revocationRaw, developmentTrust, Development, time.Unix(100, 0)); err != nil {
		t.Fatalf("development revocations rejected: %v", err)
	}
	if _, err := VerifyRevocations(revocationRaw, productionTrust, Production, time.Unix(100, 0)); err == nil {
		t.Fatal("copied development key promoted its signed revocation snapshot")
	}
}

func TestUnknownFeatureIsAuditOnly(t *testing.T) {
	private, trust, binding := fixture(t)
	claims := grantClaims()
	claims[9] = []string{"future-feature", "progressive-rollouts"}
	grant, err := VerifyGrant(validEnvelope(t, claims, private), trust, Production, binding, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(grant.Features, ",") != "future-feature,progressive-rollouts" || strings.Join(grant.EffectiveFeatures, ",") != "progressive-rollouts" {
		t.Fatalf("feature intersection lost audit claims: %+v", grant)
	}
}

func TestCurrentTrustAndKeyValidity(t *testing.T) {
	private, trust, binding := fixture(t)
	raw := validEnvelope(t, grantClaims(), private)
	trust.Issuers[0].Keys[0].ValidFrom = 101
	if _, err := VerifyGrant(raw, trust, Production, binding, time.Unix(100, 0)); err == nil {
		t.Fatal("accepted document before key activation")
	}
	trust.Issuers[0].Keys[0].ValidFrom = 0
	trust.Issuers[0].Keys[0].ValidUntil = 100
	if _, err := VerifyGrant(raw, trust, Production, binding, time.Unix(100, 0)); err == nil {
		t.Fatal("accepted document issued at key retirement")
	}
	trust.Issuers[0].Keys[0].ValidUntil = 101
	if _, err := VerifyGrant(raw, trust, Production, binding, time.Unix(100, 0)); err != nil {
		t.Fatalf("retained retired key failed historical evidence: %v", err)
	}
	trust.Issuers[0].Keys[0].PublicKey = trust.Issuers[0].Keys[0].PublicKey[:31]
	if _, err := VerifyGrant(raw, trust, Production, binding, time.Unix(100, 0)); err == nil {
		t.Fatal("accepted malformed issuer key")
	}
	trust.Issuers = nil
	if _, err := VerifyGrant(raw, trust, Production, binding, time.Unix(100, 0)); err == nil {
		t.Fatal("removed issuer still grants authority")
	}
}

// Golden grant payloads of the licence tool, copied from
// progressive-rollouts-plugin tools/licence/internal/wire/wire_test.go
// (goldenOrganizationGrant, goldenInstallationGrant,
// goldenInstallationGrantWithLimit): Core must accept exactly the bytes the
// tool signs. The installation-wide grants have fourteen claims (map header
// 0xae) because claim 8 is absent; the last one carries claim 10
// {"organizations": 25}.
const (
	toolOrganizationGrant          = "af010102656772616e7403697374656c6c7765726b0450ec3dd34205b24c0ab3929fa998e1c81d05070678226465762e7374656c6c7765726b2e70726f67726573736976652d726f6c6c6f75747307504ec3e35b80fb426faeb596bcd0b2f94c0850680e8afdb0f84d13b64a22f4aca4118b09817470726f67726573736976652d726f6c6c6f7574730aa00b1a6ac4e2c00c1a6ac4e2c00d1a6ac634400e1a6ac785c00f6a70726f64756374696f6e"
	toolInstallationGrant          = "ae010102656772616e7403697374656c6c7765726b0450ec3dd34205b24c0ab3929fa998e1c81d05070678226465762e7374656c6c7765726b2e70726f67726573736976652d726f6c6c6f75747307504ec3e35b80fb426faeb596bcd0b2f94c09817470726f67726573736976652d726f6c6c6f7574730aa00b1a6ac4e2c00c1a6ac4e2c00d1a6ac634400e1a6ac785c00f6a70726f64756374696f6e"
	toolInstallationGrantWithLimit = "ae010102656772616e7403697374656c6c7765726b0450ec3dd34205b24c0ab3929fa998e1c81d05070678226465762e7374656c6c7765726b2e70726f67726573736976652d726f6c6c6f75747307504ec3e35b80fb426faeb596bcd0b2f94c09817470726f67726573736976652d726f6c6c6f7574730aa16d6f7267616e697a6174696f6e7318190b1a6ac4e2c00c1a6ac4e2c00d1a6ac634400e1a6ac785c00f6a70726f64756374696f6e"
)

func TestLicenceToolGoldenGrantsVerifyForTheirScope(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const plugin = "dev.stellwerk.progressive-rollouts"
	trust := TrustStore{Class: Production, Issuers: []Issuer{{ID: "stellwerk", PluginIDs: []string{plugin},
		Keys: []Key{{ID: []byte("tool-key"), PublicKey: public, Purposes: []Purpose{GrantPurpose}}}}}}
	issued := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	other := uuid.MustParse("0f6b2a62-9a3e-4f0e-8d1c-3b7a5e9c2d41")
	for _, c := range []struct {
		name         string
		payload      string
		organization uuid.UUID
		limit        int64
	}{
		{"organization grant", toolOrganizationGrant, testOrganization, 0},
		{"installation-wide grant", toolInstallationGrant, uuid.Nil, 0},
		{"installation-wide grant with limit", toolInstallationGrantWithLimit, uuid.Nil, 25},
	} {
		t.Run(c.name, func(t *testing.T) {
			payload, err := hex.DecodeString(c.payload)
			if err != nil {
				t.Fatal(err)
			}
			raw := signIndependent(t, payload, private, encoded(t, map[int]any{1: -8, 4: []byte("tool-key")}))
			fits := map[string]uuid.UUID{"its organization": testOrganization, "another organization": other, "the installation scope": uuid.Nil}
			for scope, organization := range fits {
				binding := Binding{PluginID: plugin, InstallationID: testInstallation, OrganizationID: organization, DeclaredFeatures: []string{"progressive-rollouts"}}
				grant, err := VerifyGrant(raw, trust, Production, binding, issued)
				if c.organization != uuid.Nil && organization != c.organization {
					if err == nil {
						t.Fatalf("an organization grant fit %s", scope)
					}
					continue
				}
				if err != nil {
					t.Fatalf("%s: %v", scope, err)
				}
				want := Grant{Issuer: "stellwerk", IssuanceID: testIssuance, Sequence: 7, PluginID: plugin, InstallationID: testInstallation, OrganizationID: c.organization,
					Features: []string{"progressive-rollouts"}, OrganizationLimit: c.limit, IssuedAt: issued.Unix(), NotBefore: issued.Unix(),
					ExpiresAt: issued.Unix() + 86400, GraceUntil: issued.Unix() + 2*86400, TrustClass: Production}
				if grant.Issuer != want.Issuer || grant.IssuanceID != want.IssuanceID || grant.Sequence != want.Sequence || grant.PluginID != want.PluginID ||
					grant.InstallationID != want.InstallationID || grant.OrganizationID != want.OrganizationID || strings.Join(grant.Features, ",") != "progressive-rollouts" ||
					grant.OrganizationLimit != want.OrganizationLimit || grant.IssuedAt != want.IssuedAt || grant.NotBefore != want.NotBefore ||
					grant.ExpiresAt != want.ExpiresAt || grant.GraceUntil != want.GraceUntil || grant.TrustClass != want.TrustClass {
					t.Fatalf("%s: decoded %+v, want %+v", scope, grant, want)
				}
				if grant.InstallationWide() != (c.organization == uuid.Nil) {
					t.Fatalf("%s: installation-wide is %v", scope, grant.InstallationWide())
				}
			}
		})
	}
}

func TestInstallationScopeAndOrganizationLimitAreStrict(t *testing.T) {
	private, trust, binding := fixture(t)
	installationWide := func(limits map[string]uint64) map[uint64]any {
		claims := grantClaims()
		delete(claims, 8)
		claims[10] = limits
		return claims
	}
	scope := binding
	scope.OrganizationID = uuid.Nil
	for _, c := range []struct {
		name   string
		limits map[string]uint64
		want   int64
	}{{"no limit", map[string]uint64{}, 0}, {"one organization", map[string]uint64{"organizations": 1}, 1}, {"largest limit", map[string]uint64{"organizations": math.MaxInt64}, math.MaxInt64}} {
		grant, err := VerifyGrant(validEnvelope(t, installationWide(c.limits), private), trust, Production, scope, time.Unix(100, 0))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if grant.OrganizationID != uuid.Nil || grant.OrganizationLimit != c.want {
			t.Fatalf("%s: wrong scope or limit %+v", c.name, grant)
		}
	}
	nilOrganization := grantClaims()
	nilOrganization[8] = uuid.Nil[:]
	shortOrganization := grantClaims()
	shortOrganization[8] = testOrganization[:15]
	textOrganization := grantClaims()
	textOrganization[8] = testOrganization.String()
	limitedOrganization := grantClaims()
	limitedOrganization[10] = map[string]uint64{"organizations": 3}
	missingFeatures := installationWide(map[string]uint64{})
	delete(missingFeatures, 9)
	missingFeatures[8] = testOrganization[:]
	// Sequence 1 with a needless one-byte argument: still decodable, never
	// the canonical encoding Core re-creates.
	nonCanonical := bytes.Replace(encoded(t, installationWide(map[string]uint64{})), []byte{0x05, 0x01}, []byte{0x05, 0x18, 0x01}, 1)
	for name, claims := range map[string]map[uint64]any{
		"nil organization":                  nilOrganization,
		"organization of 15 bytes":          shortOrganization,
		"organization as text":              textOrganization,
		"limit on an organization grant":    limitedOrganization,
		"zero organizations":                installationWide(map[string]uint64{"organizations": 0}),
		"more organizations than int64":     installationWide(map[string]uint64{"organizations": math.MaxInt64 + 1}),
		"unknown limit name":                installationWide(map[string]uint64{"seats": 5}),
		"organizations and an unknown name": installationWide(map[string]uint64{"organizations": 3, "seats": 5}),
	} {
		if _, err := VerifyGrant(validEnvelope(t, claims, private), trust, Production, binding, time.Unix(100, 0)); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	// Claim 8 is the only claim that may be missing.
	if _, err := VerifyGrant(validEnvelope(t, missingFeatures, private), trust, Production, binding, time.Unix(100, 0)); err == nil || !strings.Contains(err.Error(), "missing or unknown entitlement claim") {
		t.Fatalf("fourteen claims without claim 9: %v", err)
	}
	raw := signIndependent(t, nonCanonical, private, encoded(t, map[int]any{1: -8, 4: []byte("test-key")}))
	if _, err := VerifyGrant(raw, trust, Production, scope, time.Unix(100, 0)); err == nil || !strings.Contains(err.Error(), "non-deterministic") {
		t.Fatalf("a non-canonical installation-wide payload: %v", err)
	}
	// An organization grant never fits the installation scope.
	if _, err := VerifyGrant(validEnvelope(t, grantClaims(), private), trust, Production, scope, time.Unix(100, 0)); err == nil {
		t.Fatal("an organization grant fit the installation scope")
	}
}

func FuzzVerifyGrantBounded(f *testing.F) {
	f.Add([]byte{0xd2, 0x80})
	f.Add(bytes.Repeat([]byte{0xff}, 128))
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > MaxEnvelopeBytes+1 {
			return
		}
		_, _ = VerifyGrant(raw, TrustStore{Class: Production}, Production, Binding{
			PluginID: "dev.stellwerk.progressive-rollouts", InstallationID: testInstallation, OrganizationID: testOrganization,
		}, time.Unix(100, 0))
	})
}
