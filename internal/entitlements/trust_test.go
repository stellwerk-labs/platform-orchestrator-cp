package entitlements

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"
	"time"
)

func trustJSON(t *testing.T, publicKey any) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{
		"schema_version": 1, "trust_class": "production",
		"issuers": []any{map[string]any{
			"id": "issuer.example", "plugin_ids": []string{"dev.stellwerk.progressive-rollouts"},
			"keys": []any{map[string]any{
				"kid":            base64.RawURLEncoding.EncodeToString([]byte("test-key")),
				"public_key_pem": string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})),
				"purposes":       []string{"grant", "revocation"}, "valid_from": 0, "valid_until": 0,
			}},
		}},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestParseTrustJSONAndClassIsolation(t *testing.T) {
	private, _, _ := fixture(t)
	raw := trustJSON(t, private.Public())
	store, err := ParseTrustJSON(raw, Production)
	if err != nil {
		t.Fatal(err)
	}
	if store.Class != Production || len(store.Issuers) != 1 || len(store.Issuers[0].Keys) != 1 {
		t.Fatalf("wrong trust projection: %+v", store)
	}
	if _, err := ParseTrustJSON(raw, Development); err == nil {
		t.Fatal("development mode accepted production trust file")
	}
	grant, err := VerifyGrant(validEnvelope(t, grantClaims(), private), store, Production, Binding{
		PluginID: "dev.stellwerk.progressive-rollouts", InstallationID: testInstallation,
		OrganizationID: testOrganization, DeclaredFeatures: []string{"progressive-rollouts"},
	}, time.Unix(100, 0))
	if err != nil || grant.TrustClass != Production {
		t.Fatalf("parsed trust failed grant verification: %v", err)
	}
}

func TestParseTrustJSONRejectsAmbiguousAndWrongKeys(t *testing.T) {
	private, _, _ := fixture(t)
	raw := trustJSON(t, private.Public())
	for name, candidate := range map[string][]byte{
		"duplicate top-level": []byte(`{"schema_version":1,"schema_version":1,"trust_class":"production","issuers":[]}`),
		"duplicate nested":    []byte(`{"schema_version":1,"trust_class":"production","issuers":[{"id":"a","id":"b"}]}`),
		"wrong field case":    bytes.Replace(bytes.Clone(raw), []byte(`"trust_class"`), []byte(`"Trust_Class"`), 1),
		"unknown field":       append(raw[:len(raw)-1:len(raw)-1], []byte(`,"extra":1}`)...),
		"trailing":            append(append([]byte{}, raw...), []byte(` {}`)...),
		"invalid utf8":        append(append([]byte{}, raw...), 0xff),
		"oversize":            []byte(strings.Repeat(" ", MaxTrustJSONBytes+1)),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseTrustJSON(candidate, Production); err == nil {
				t.Fatal("accepted invalid trust file")
			}
		})
	}
	ecdsaKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseTrustJSON(trustJSON(t, &ecdsaKey.PublicKey), Production); err == nil {
		t.Fatal("accepted wrong key curve")
	}
}
