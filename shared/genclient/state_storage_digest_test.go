package genclient

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func validStateStorageDigest(t *testing.T, raw string) (StateStorageType, string) {
	t.Helper()
	kind, digest, err := StateStorageConfigurationDigest(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != 32 {
		t.Fatalf("expected SHA256 identity, got %q: %v", digest, err)
	}
	return kind, digest
}

func TestStateStorageConfigurationDigestBindsTypedInput(t *testing.T) {
	for _, test := range []struct {
		kind    StateStorageType
		base    string
		equal   string
		changed string
	}{
		{StateStorageTypeKubernetes, `{"type":"kubernetes","namespace":"states"}`, ` {"namespace":"states", "type":"kubernetes"} `, `{"type":"kubernetes","namespace":"other"}`},
		{StateStorageTypeS3, `{"type":"s3","bucket":"states"}`, `{"bucket":"states","path_prefix":null,"type":"s3"}`, `{"type":"s3","bucket":"states","path_prefix":""}`},
		{StateStorageTypeGcs, `{"type":"gcs","bucket":"states"}`, `{"path_prefix":null,"bucket":"states","type":"gcs"}`, `{"type":"gcs","bucket":"other"}`},
		{StateStorageTypeAzurerm, `{"type":"azurerm","container_name":"states","storage_account_name":"account"}`, `{"storage_account_name":"account","resource_group_name":null,"container_name":"states","path_prefix":null,"type":"azurerm"}`, `{"type":"azurerm","container_name":"states","storage_account_name":"account","resource_group_name":"group"}`},
	} {
		t.Run(string(test.kind), func(t *testing.T) {
			kind, digest := validStateStorageDigest(t, test.base)
			equalKind, equalDigest := validStateStorageDigest(t, test.equal)
			_, changed := validStateStorageDigest(t, test.changed)
			if kind != test.kind || equalKind != kind || equalDigest != digest || changed == digest {
				t.Fatal("typed identity must normalize equivalent input and distinguish changed input")
			}
		})
	}
	_, s3 := validStateStorageDigest(t, `{"type":"s3","bucket":"states"}`)
	_, gcs := validStateStorageDigest(t, `{"type":"gcs","bucket":"states"}`)
	if s3 == gcs {
		t.Fatal("otherwise identical backend shapes must retain their discriminator")
	}
}

func TestStateStorageConfigurationDigestRejectsInvalidInput(t *testing.T) {
	for index, raw := range []string{
		``, `null`, `[]`, `"s3"`, `{}`, `{"type":"future"}`,
		`{"type":"s3"}`, `{"type":"s3","bucket":null}`, `{"type":"s3","bucket":""}`, `{"type":"s3","bucket":7}`,
		`{"type":"gcs"}`, `{"type":"gcs","bucket":null}`, `{"type":"gcs","bucket":""}`,
		`{"type":"kubernetes"}`, `{"type":"kubernetes","namespace":null}`, `{"type":"kubernetes","namespace":""}`,
		`{"type":"azurerm","container_name":"states"}`, `{"type":"azurerm","storage_account_name":"account"}`,
		`{"type":"azurerm","storage_account_name":null,"container_name":"states"}`,
		`{"type":"azurerm","storage_account_name":"account","container_name":null}`,
		`{"type":"azurerm","storage_account_name":"","container_name":"states"}`,
		`{"type":"azurerm","storage_account_name":"account","container_name":""}`,
		`{"type":"s3","bucket":"states","namespace":"other"}`, `{"type":"s3","bucket":"states","path_prefix":7}`,
		`{"type":"s3","bucket":"states"} {}`, `{"type":"s3","bucket":"states","TYPE":"gcs"}`,
		`{"type":"s3","bucket":"states","BUCKET":"other"}`,
		`{"type":"azurerm","storage_account_name":"account","container_name":"states","reſource_group_name":"other"}`,
		`{"type":"azurerm","ſtorage_account_name":"account","container_name":"states"}`,
		`{"type":"s3","bucket":"` + strings.Repeat("private-canary", 6000) + `"}`,
	} {
		kind, digest, err := StateStorageConfigurationDigest(json.RawMessage(raw))
		if !errors.Is(err, errInvalidStateStorageIdentity) || kind != "" || digest != "" {
			t.Fatalf("invalid case %d must refuse without an identity: %v", index, err)
		}
		if err.Error() != "invalid state storage configuration identity" {
			t.Fatalf("invalid case %d must return a fixed value-free error", index)
		}
	}
}
