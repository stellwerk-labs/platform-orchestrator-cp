package genclient

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

var errInvalidStateStorageIdentity = errors.New("invalid state storage configuration identity")

// StateStorageConfigurationDigest binds typed Runner configuration input, not
// rendered backend HCL or the Environment-derived state key.
func StateStorageConfigurationDigest(raw json.RawMessage) (StateStorageType, string, error) {
	if len(raw) == 0 || len(raw) > 64<<10 {
		return "", "", errInvalidStateStorageIdentity
	}
	var discriminator struct {
		Type StateStorageType `json:"type"`
	}
	if json.Unmarshal(raw, &discriminator) != nil {
		return "", "", errInvalidStateStorageIdentity
	}
	switch discriminator.Type {
	case StateStorageTypeKubernetes:
		return digestStateStorageConfiguration(raw, discriminator.Type, &K8sStorageConfiguration{}, "namespace")
	case StateStorageTypeS3:
		return digestStateStorageConfiguration(raw, discriminator.Type, &S3StorageConfiguration{}, "bucket")
	case StateStorageTypeGcs:
		return digestStateStorageConfiguration(raw, discriminator.Type, &GCSStorageConfiguration{}, "bucket")
	case StateStorageTypeAzurerm:
		return digestStateStorageConfiguration(raw, discriminator.Type, &AzureRMStorageConfiguration{}, "container_name", "storage_account_name")
	default:
		return "", "", errInvalidStateStorageIdentity
	}
}

func digestStateStorageConfiguration(raw json.RawMessage, kind StateStorageType, config any, required ...string) (StateStorageType, string, error) {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return "", "", errInvalidStateStorageIdentity
	}
	for key := range object {
		// encoding/json also folds Unicode aliases; these schemas use ASCII keys.
		if strings.IndexFunc(key, func(r rune) bool { return (r < 'a' || r > 'z') && r != '_' }) != -1 {
			return "", "", errInvalidStateStorageIdentity
		}
	}
	for _, key := range required {
		var value string
		if json.Unmarshal(object[key], &value) != nil || value == "" {
			return "", "", errInvalidStateStorageIdentity
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(config) != nil {
		return "", "", errInvalidStateStorageIdentity
	}
	typed, err := json.Marshal(config)
	if err != nil {
		return "", "", errInvalidStateStorageIdentity
	}
	var canonicalObject map[string]json.RawMessage
	if json.Unmarshal(typed, &canonicalObject) != nil {
		return "", "", errInvalidStateStorageIdentity
	}
	canonical, err := json.Marshal(canonicalObject)
	if err != nil {
		return "", "", errInvalidStateStorageIdentity
	}
	digest := sha256.Sum256(canonical)
	return kind, hex.EncodeToString(digest[:]), nil
}
