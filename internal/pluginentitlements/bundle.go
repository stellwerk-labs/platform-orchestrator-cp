package pluginentitlements

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/entitlements"
)

const MaxBundleBytes = 1 << 20

type Bundle struct {
	Grants      [][]byte
	Revocations [][]byte
	Digest      [sha256.Size]byte
}

type bundleDocument struct {
	SchemaVersion int      `json:"schema_version"`
	Grants        []string `json:"grants"`
	Revocations   []string `json:"revocations"`
}

// ParseBundleJSON accepts only transport bytes. The authenticated claims are
// exclusively those inside each independently verified COSE document.
func ParseBundleJSON(raw []byte) (Bundle, error) {
	var bundle Bundle
	if len(raw) == 0 || len(raw) > MaxBundleBytes || !utf8.Valid(raw) {
		return bundle, errors.New("invalid entitlement bundle size or UTF-8")
	}
	if err := rejectDuplicateKeys(raw); err != nil {
		return bundle, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || len(fields) != 3 {
		return bundle, errors.New("invalid entitlement bundle object")
	}
	for _, field := range []string{"schema_version", "grants", "revocations"} {
		if _, ok := fields[field]; !ok {
			return bundle, errors.New("missing or unknown entitlement bundle field")
		}
	}
	for _, field := range []string{"grants", "revocations"} {
		value := bytes.TrimSpace(fields[field])
		if len(value) == 0 || value[0] != '[' {
			return bundle, errors.New("entitlement bundle documents must be arrays")
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var doc bundleDocument
	if err := decoder.Decode(&doc); err != nil {
		return bundle, fmt.Errorf("invalid entitlement bundle: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return bundle, errors.New("trailing entitlement bundle JSON")
	}
	if doc.SchemaVersion != 1 || len(doc.Grants) > 64 || len(doc.Revocations) > 64 {
		return bundle, errors.New("unsupported entitlement bundle schema or item count")
	}
	for _, text := range doc.Grants {
		encoded, err := decodeCOSE(text)
		if err != nil {
			return Bundle{}, err
		}
		bundle.Grants = append(bundle.Grants, encoded)
	}
	for _, text := range doc.Revocations {
		encoded, err := decodeCOSE(text)
		if err != nil {
			return Bundle{}, err
		}
		bundle.Revocations = append(bundle.Revocations, encoded)
	}
	bundle.Digest = sha256.Sum256(raw)
	return bundle, nil
}

func decodeCOSE(text string) ([]byte, error) {
	raw, err := base64.RawURLEncoding.DecodeString(text)
	if err != nil || len(raw) == 0 || len(raw) > entitlements.MaxEnvelopeBytes || base64.RawURLEncoding.EncodeToString(raw) != text {
		return nil, errors.New("bundle COSE must be bounded unpadded base64url")
	}
	return raw, nil
}

func rejectDuplicateKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := scanJSON(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("trailing entitlement bundle JSON")
	}
	return nil
}

func scanJSON(decoder *json.Decoder, depth int) error {
	if depth > 6 {
		return errors.New("entitlement bundle JSON nesting exceeds limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok || seen[key] {
				return errors.New("duplicate entitlement bundle JSON key")
			}
			seen[key] = true
			if err := scanJSON(decoder, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := scanJSON(decoder, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("invalid entitlement bundle JSON delimiter")
	}
	_, err = decoder.Token()
	return err
}
