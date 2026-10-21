package entitlements

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

const MaxTrustJSONBytes = 64 << 10

type trustDocument struct {
	SchemaVersion int           `json:"schema_version"`
	TrustClass    TrustClass    `json:"trust_class"`
	Issuers       []trustIssuer `json:"issuers"`
}

type trustIssuer struct {
	ID        string     `json:"id"`
	PluginIDs []string   `json:"plugin_ids"`
	Keys      []trustKey `json:"keys"`
}

type trustKey struct {
	KID          string    `json:"kid"`
	PublicKeyPEM string    `json:"public_key_pem"`
	Purposes     []Purpose `json:"purposes"`
	ValidFrom    *int64    `json:"valid_from"`
	ValidUntil   *int64    `json:"valid_until"`
}

// ParseTrustJSON validates the exact operator trust schema and its pinned
// PKIX Ed25519 public keys. The caller owns bounded regular-file, no-symlink
// reading; this function accepts only bytes and performs no I/O.
func ParseTrustJSON(raw []byte, expectedClass TrustClass) (TrustStore, error) {
	var out TrustStore
	if expectedClass != Production && expectedClass != Development {
		return out, errors.New("invalid expected trust class")
	}
	if len(raw) == 0 || len(raw) > MaxTrustJSONBytes || !utf8.Valid(raw) {
		return out, errors.New("invalid trust document size or UTF-8")
	}
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return out, err
	}
	if err := checkTrustSchema(raw); err != nil {
		return out, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var doc trustDocument
	if err := decoder.Decode(&doc); err != nil {
		return out, fmt.Errorf("trust JSON: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return out, errors.New("trailing trust JSON")
	}
	if doc.SchemaVersion != 1 || doc.TrustClass != expectedClass || len(doc.Issuers) == 0 || len(doc.Issuers) > 16 {
		return out, errors.New("invalid trust schema, class or issuer count")
	}
	out.Class = doc.TrustClass
	seenIssuers := make(map[string]bool, len(doc.Issuers))
	for _, issuer := range doc.Issuers {
		if !asciiIdentifier(issuer.ID) || seenIssuers[issuer.ID] || len(issuer.PluginIDs) == 0 || len(issuer.PluginIDs) > 64 || len(issuer.Keys) == 0 || len(issuer.Keys) > 32 {
			return TrustStore{}, errors.New("invalid or duplicate trust issuer")
		}
		seenIssuers[issuer.ID] = true
		parsed := Issuer{ID: issuer.ID}
		seenPlugins := make(map[string]bool, len(issuer.PluginIDs))
		for _, plugin := range issuer.PluginIDs {
			if len(plugin) > 255 || !pluginID.MatchString(plugin) || seenPlugins[plugin] {
				return TrustStore{}, errors.New("invalid or duplicate trusted plugin")
			}
			seenPlugins[plugin] = true
			parsed.PluginIDs = append(parsed.PluginIDs, plugin)
		}
		seenKeys := make(map[string]bool, len(issuer.Keys))
		for _, key := range issuer.Keys {
			kid, err := base64.RawURLEncoding.DecodeString(key.KID)
			if err != nil || len(kid) == 0 || len(kid) > 64 || base64.RawURLEncoding.EncodeToString(kid) != key.KID || seenKeys[string(kid)] {
				return TrustStore{}, errors.New("invalid or duplicate trusted key ID")
			}
			seenKeys[string(kid)] = true
			public, err := parseEd25519PublicKey(key.PublicKeyPEM)
			if err != nil {
				return TrustStore{}, err
			}
			if key.ValidFrom == nil || key.ValidUntil == nil || *key.ValidFrom < 0 || (*key.ValidUntil != 0 && *key.ValidUntil <= *key.ValidFrom) {
				return TrustStore{}, errors.New("invalid trusted key validity")
			}
			if len(key.Purposes) == 0 || len(key.Purposes) > 2 {
				return TrustStore{}, errors.New("invalid trusted key purposes")
			}
			purposeSeen := make(map[Purpose]bool, len(key.Purposes))
			for _, purpose := range key.Purposes {
				if (purpose != GrantPurpose && purpose != RevocationPurpose) || purposeSeen[purpose] {
					return TrustStore{}, errors.New("invalid or duplicate trusted key purpose")
				}
				purposeSeen[purpose] = true
			}
			parsed.Keys = append(parsed.Keys, Key{ID: kid, PublicKey: public, Purposes: key.Purposes, ValidFrom: *key.ValidFrom, ValidUntil: *key.ValidUntil})
		}
		out.Issuers = append(out.Issuers, parsed)
	}
	return out, nil
}

func parseEd25519PublicKey(text string) (ed25519.PublicKey, error) {
	if len(text) == 0 || len(text) > 2048 {
		return nil, errors.New("invalid issuer public key PEM size")
	}
	block, rest := pem.Decode([]byte(text))
	if block == nil || block.Type != "PUBLIC KEY" || len(block.Headers) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("issuer key must be one PKIX PUBLIC KEY PEM block")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("invalid issuer public key: %w", err)
	}
	public, ok := parsed.(ed25519.PublicKey)
	if !ok || len(public) != ed25519.PublicKeySize {
		return nil, errors.New("issuer public key must be Ed25519")
	}
	return bytes.Clone(public), nil
}

func rejectDuplicateJSONKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := scanJSONValue(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("trailing trust JSON")
	}
	return nil
}

func checkTrustSchema(raw []byte) error {
	root, err := exactObject(raw, "schema_version", "trust_class", "issuers")
	if err != nil {
		return err
	}
	var issuers []json.RawMessage
	if err := json.Unmarshal(root["issuers"], &issuers); err != nil {
		return fmt.Errorf("invalid trust issuers: %w", err)
	}
	for _, rawIssuer := range issuers {
		issuer, err := exactObject(rawIssuer, "id", "plugin_ids", "keys")
		if err != nil {
			return err
		}
		var keys []json.RawMessage
		if err := json.Unmarshal(issuer["keys"], &keys); err != nil {
			return fmt.Errorf("invalid trust keys: %w", err)
		}
		for _, rawKey := range keys {
			if _, err := exactObject(rawKey, "kid", "public_key_pem", "purposes", "valid_from", "valid_until"); err != nil {
				return err
			}
		}
	}
	return nil
}

func exactObject(raw []byte, names ...string) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &object) != nil || len(object) != len(names) {
		return nil, errors.New("trust JSON object has missing or unknown fields")
	}
	for _, name := range names {
		if _, ok := object[name]; !ok {
			return nil, errors.New("trust JSON object has missing or unknown fields")
		}
	}
	return object, nil
}

func scanJSONValue(decoder *json.Decoder, depth int) error {
	if depth > 8 {
		return errors.New("trust JSON nesting exceeds limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("invalid trust JSON: %w", err)
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
				return errors.New("duplicate or invalid trust JSON key")
			}
			seen[key] = true
			if err := scanJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("invalid trust JSON delimiter")
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	return nil
}
