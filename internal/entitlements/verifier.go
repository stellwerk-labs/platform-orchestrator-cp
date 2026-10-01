// Package entitlements verifies the closed offline entitlement v1 wire profile.
// It performs no I/O. Callers own trust-file loading, persistence, monotonic
// sequence checks, revocation state, and authorization decisions.
package entitlements

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/google/uuid"
	"github.com/veraison/go-cose"
)

const MaxEnvelopeBytes = 64 << 10

type TrustClass string

const (
	Production  TrustClass = "production"
	Development TrustClass = "development"
)

type Purpose string

const (
	GrantPurpose      Purpose = "grant"
	RevocationPurpose Purpose = "revocation"
)

type Key struct {
	ID         []byte
	PublicKey  ed25519.PublicKey
	Purposes   []Purpose
	ValidFrom  int64
	ValidUntil int64 // Zero means no scheduled retirement.
}

type Issuer struct {
	ID        string
	PluginIDs []string
	Keys      []Key
}

// TrustStore is an operator-supplied set of pinned public keys. Mode must be
// passed separately to prevent a development store being used in production.
type TrustStore struct {
	Class   TrustClass
	Issuers []Issuer
}

type Binding struct {
	PluginID         string
	InstallationID   uuid.UUID
	OrganizationID   uuid.UUID
	DeclaredFeatures []string
}

type Grant struct {
	Envelope          []byte
	Issuer            string
	KeyID             []byte
	IssuanceID        uuid.UUID
	Sequence          int64
	PluginID          string
	InstallationID    uuid.UUID
	OrganizationID    uuid.UUID
	Features          []string
	EffectiveFeatures []string
	Limits            map[string]int64
	IssuedAt          int64
	NotBefore         int64
	ExpiresAt         int64
	GraceUntil        int64
	TrustClass        TrustClass
	Digest            [sha256.Size]byte
}

type Revocations struct {
	Envelope    []byte
	Issuer      string
	KeyID       []byte
	Sequence    int64
	IssuedAt    int64
	IssuanceIDs []uuid.UUID
	TrustClass  TrustClass
	Digest      [sha256.Size]byte
}

type TimeState string

const (
	NotYetValid TimeState = "not_yet_valid"
	Valid       TimeState = "valid"
	Grace       TimeState = "grace"
	Expired     TimeState = "expired"
)

var (
	decodeMode cbor.DecMode
	encodeMode cbor.EncMode
	slug       = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	pluginID   = regexp.MustCompile(`^[a-z][a-z0-9-]*(\.[a-z][a-z0-9-]*)+$`)
)

func init() {
	var err error
	decodeMode, err = (cbor.DecOptions{
		DupMapKey:        cbor.DupMapKeyEnforcedAPF,
		MaxNestedLevels:  8,
		MaxArrayElements: 4096,
		MaxMapPairs:      64,
		IndefLength:      cbor.IndefLengthForbidden,
		TagsMd:           cbor.TagsForbidden,
	}).DecMode()
	if err != nil {
		panic(err)
	}
	encodeMode, err = cbor.CoreDetEncOptions().EncMode()
	if err != nil {
		panic(err)
	}
}

// VerifyGrant authenticates an exact installation and organisation grant.
// Grant features absent from the signed package declaration remain in Features
// for audit but are omitted from EffectiveFeatures. V1 accepts no limits.
func VerifyGrant(raw []byte, trust TrustStore, mode TrustClass, binding Binding, now time.Time) (Grant, error) {
	var out Grant
	if err := validateBinding(binding); err != nil {
		return out, err
	}
	message, kid, payload, err := decodeEnvelope(raw)
	if err != nil {
		return out, err
	}
	claims, err := decodeGrant(payload)
	if err != nil {
		return out, err
	}
	key, err := selectKey(trust, mode, claims.Issuer, kid, GrantPurpose, claims.PluginID, claims.IssuedAt)
	if err != nil {
		return out, err
	}
	if err := verify(message, key); err != nil {
		return out, err
	}
	if claims.TrustClass != mode {
		return out, errors.New("signed entitlement trust class does not match verifier mode")
	}
	if claims.PluginID != binding.PluginID || claims.InstallationID != binding.InstallationID || claims.OrganizationID != binding.OrganizationID {
		return out, errors.New("entitlement binding mismatch")
	}
	if err := checkIssuedAt(claims.IssuedAt, now); err != nil {
		return out, err
	}
	claims.KeyID = bytes.Clone(kid)
	claims.Envelope = bytes.Clone(raw)
	claims.Digest = sha256.Sum256(raw)
	for _, feature := range claims.Features {
		if slices.Contains(binding.DeclaredFeatures, feature) {
			claims.EffectiveFeatures = append(claims.EffectiveFeatures, feature)
		}
	}
	return claims, nil
}

// VerifyRevocations authenticates a complete signed snapshot. The caller must
// enforce monotonic sequence and superset rules against its persisted state.
func VerifyRevocations(raw []byte, trust TrustStore, mode TrustClass, now time.Time) (Revocations, error) {
	var out Revocations
	message, kid, payload, err := decodeEnvelope(raw)
	if err != nil {
		return out, err
	}
	claims, err := decodeRevocations(payload)
	if err != nil {
		return out, err
	}
	key, err := selectKey(trust, mode, claims.Issuer, kid, RevocationPurpose, "", claims.IssuedAt)
	if err != nil {
		return out, err
	}
	if err := verify(message, key); err != nil {
		return out, err
	}
	if claims.TrustClass != mode {
		return out, errors.New("signed revocation trust class does not match verifier mode")
	}
	if err := checkIssuedAt(claims.IssuedAt, now); err != nil {
		return out, err
	}
	claims.KeyID = bytes.Clone(kid)
	claims.Envelope = bytes.Clone(raw)
	claims.Digest = sha256.Sum256(raw)
	return claims, nil
}

// EvaluateTime uses the caller's effective monotonic Unix second. The caller
// persists evaluated-through and terminal observations; this pure function
// cannot make a wall clock monotonic across transactions or restarts.
func EvaluateTime(grant Grant, effectiveNow int64) TimeState {
	switch {
	case effectiveNow < grant.NotBefore:
		return NotYetValid
	case effectiveNow < grant.ExpiresAt:
		return Valid
	case effectiveNow < grant.GraceUntil:
		return Grace
	default:
		return Expired
	}
}

func decodeEnvelope(raw []byte) (*cose.Sign1Message, []byte, []byte, error) {
	if len(raw) == 0 || len(raw) > MaxEnvelopeBytes || raw[0] != 0xd2 {
		return nil, nil, nil, errors.New("entitlement requires bounded, minimally tagged COSE_Sign1")
	}
	var parts []cbor.RawMessage
	if err := decodeMode.Unmarshal(raw[1:], &parts); err != nil || len(parts) != 4 {
		return nil, nil, nil, errors.New("invalid COSE_Sign1 array")
	}
	var protected, payload, signature []byte
	if err := decodeMajor(parts[0], 2, &protected); err != nil {
		return nil, nil, nil, fmt.Errorf("protected header: %w", err)
	}
	if !bytes.Equal(parts[1], []byte{0xa0}) {
		return nil, nil, nil, errors.New("unprotected header must be empty")
	}
	if err := decodeMajor(parts[2], 2, &payload); err != nil {
		return nil, nil, nil, fmt.Errorf("embedded payload: %w", err)
	}
	if err := decodeMajor(parts[3], 2, &signature); err != nil || len(signature) != ed25519.SignatureSize {
		return nil, nil, nil, errors.New("invalid Ed25519 signature length")
	}
	var header map[uint64]cbor.RawMessage
	if err := decodeMajor(protected, 5, &header); err != nil || len(header) != 2 {
		return nil, nil, nil, errors.New("protected header must contain only alg and kid")
	}
	var algorithm int64
	if err := decodeMajor(header[1], 1, &algorithm); err != nil || algorithm != -8 {
		return nil, nil, nil, errors.New("protected algorithm must be EdDSA")
	}
	var kid []byte
	if err := decodeMajor(header[4], 2, &kid); err != nil || len(kid) < 1 || len(kid) > 64 {
		return nil, nil, nil, errors.New("protected key ID must be 1..64 bytes")
	}
	canonicalHeader, err := encodeMode.Marshal(map[int64]any{1: int64(-8), 4: kid})
	if err != nil || !bytes.Equal(protected, canonicalHeader) {
		return nil, nil, nil, errors.New("non-deterministic protected header")
	}
	canonicalEnvelope, err := encodeMode.Marshal(cbor.Tag{Number: 18, Content: []any{protected, map[int64]any{}, payload, signature}})
	if err != nil || !bytes.Equal(raw, canonicalEnvelope) {
		return nil, nil, nil, errors.New("non-deterministic COSE envelope")
	}
	message := cose.NewSign1Message()
	if err := message.UnmarshalCBOR(raw); err != nil {
		return nil, nil, nil, fmt.Errorf("COSE decode: %w", err)
	}
	return message, kid, payload, nil
}

func verify(message *cose.Sign1Message, key ed25519.PublicKey) error {
	verifier, err := cose.NewVerifier(cose.AlgorithmEdDSA, key)
	if err != nil {
		return fmt.Errorf("invalid issuer key: %w", err)
	}
	if err := message.Verify(nil, verifier); err != nil {
		return fmt.Errorf("invalid entitlement signature: %w", err)
	}
	return nil
}

func decodeMajor(raw []byte, major byte, dst any) error {
	if len(raw) == 0 || raw[0]>>5 != major {
		return errors.New("unexpected CBOR type")
	}
	return decodeMode.Unmarshal(raw, dst)
}

func decodeMap(raw []byte, fields int) (map[uint64]cbor.RawMessage, error) {
	var values map[uint64]cbor.RawMessage
	if err := decodeMajor(raw, 5, &values); err != nil {
		return nil, err
	}
	if len(values) != fields {
		return nil, errors.New("missing or unknown entitlement claim")
	}
	for label := 1; label <= fields; label++ {
		if _, ok := values[uint64(label)]; !ok {
			return nil, errors.New("missing or unknown entitlement claim")
		}
	}
	return values, nil
}

func field[T any](values map[uint64]cbor.RawMessage, label uint64, major byte) (T, error) {
	var value T
	if err := decodeMajor(values[label], major, &value); err != nil {
		return value, fmt.Errorf("claim %d: %w", label, err)
	}
	return value, nil
}

func decodeGrant(raw []byte) (Grant, error) {
	var out Grant
	values, err := decodeMap(raw, 15)
	if err != nil {
		return out, err
	}
	version, err := field[uint64](values, 1, 0)
	if err != nil || version != 1 {
		return out, errors.New("unsupported grant schema")
	}
	kind, err := field[string](values, 2, 3)
	if err != nil || kind != "grant" {
		return out, errors.New("wrong entitlement kind")
	}
	if out.Issuer, err = field[string](values, 3, 3); err != nil || !asciiIdentifier(out.Issuer) {
		return out, errors.New("invalid issuer")
	}
	issuance, err := field[[]byte](values, 4, 2)
	if err != nil {
		return out, err
	}
	if out.IssuanceID, err = parseUUID(issuance); err != nil {
		return out, err
	}
	sequence, err := field[uint64](values, 5, 0)
	if err != nil || sequence == 0 || sequence > math.MaxInt64 {
		return out, errors.New("invalid grant sequence")
	}
	out.Sequence = int64(sequence)
	if out.PluginID, err = field[string](values, 6, 3); err != nil || len(out.PluginID) > 255 || !pluginID.MatchString(out.PluginID) {
		return out, errors.New("invalid plugin ID")
	}
	installation, err := field[[]byte](values, 7, 2)
	if err != nil {
		return out, err
	}
	if out.InstallationID, err = parseUUID(installation); err != nil {
		return out, err
	}
	organization, err := field[[]byte](values, 8, 2)
	if err != nil {
		return out, err
	}
	if out.OrganizationID, err = parseUUID(organization); err != nil {
		return out, err
	}
	if out.Features, err = field[[]string](values, 9, 4); err != nil || len(out.Features) < 1 || len(out.Features) > 64 || !sortedSlugs(out.Features) {
		return out, errors.New("invalid grant features")
	}
	limits, err := field[map[string]uint64](values, 10, 5)
	if err != nil || len(limits) != 0 {
		return out, errors.New("unsupported grant limits")
	}
	out.Limits = map[string]int64{}
	issued, err := field[uint64](values, 11, 0)
	if err != nil || issued > math.MaxInt64 {
		return out, errors.New("invalid issued_at")
	}
	notBefore, err := field[uint64](values, 12, 0)
	if err != nil || notBefore > math.MaxInt64 {
		return out, errors.New("invalid not_before")
	}
	expires, err := field[uint64](values, 13, 0)
	if err != nil || expires > math.MaxInt64 {
		return out, errors.New("invalid expires_at")
	}
	graceUntil, err := field[uint64](values, 14, 0)
	if err != nil || graceUntil > math.MaxInt64 || issued > notBefore || notBefore >= expires || expires > graceUntil {
		return out, errors.New("invalid grant time order")
	}
	out.IssuedAt, out.NotBefore, out.ExpiresAt, out.GraceUntil = int64(issued), int64(notBefore), int64(expires), int64(graceUntil)
	trustClass, err := field[string](values, 15, 3)
	if err != nil || TrustClass(trustClass) != Production && TrustClass(trustClass) != Development {
		return out, errors.New("invalid signed grant trust class")
	}
	out.TrustClass = TrustClass(trustClass)
	canonical, err := encodeMode.Marshal(map[uint64]any{
		1: version, 2: kind, 3: out.Issuer, 4: issuance, 5: sequence,
		6: out.PluginID, 7: installation, 8: organization, 9: out.Features,
		10: limits, 11: issued, 12: notBefore, 13: expires, 14: graceUntil, 15: trustClass,
	})
	if err != nil || !bytes.Equal(raw, canonical) {
		return Grant{}, errors.New("non-deterministic grant payload")
	}
	return out, nil
}

func decodeRevocations(raw []byte) (Revocations, error) {
	var out Revocations
	values, err := decodeMap(raw, 7)
	if err != nil {
		return out, err
	}
	version, err := field[uint64](values, 1, 0)
	if err != nil || version != 1 {
		return out, errors.New("unsupported revocation schema")
	}
	kind, err := field[string](values, 2, 3)
	if err != nil || kind != "revocations" {
		return out, errors.New("wrong entitlement kind")
	}
	if out.Issuer, err = field[string](values, 3, 3); err != nil || !asciiIdentifier(out.Issuer) {
		return out, errors.New("invalid issuer")
	}
	sequence, err := field[uint64](values, 4, 0)
	if err != nil || sequence == 0 || sequence > math.MaxInt64 {
		return out, errors.New("invalid revocation sequence")
	}
	out.Sequence = int64(sequence)
	issued, err := field[uint64](values, 5, 0)
	if err != nil || issued > math.MaxInt64 {
		return out, errors.New("invalid issued_at")
	}
	out.IssuedAt = int64(issued)
	ids, err := field[[][]byte](values, 6, 4)
	if err != nil {
		return out, err
	}
	for i, id := range ids {
		parsed, parseErr := parseUUID(id)
		if parseErr != nil {
			return Revocations{}, parseErr
		}
		if i > 0 && bytes.Compare(ids[i-1], id) >= 0 {
			return Revocations{}, errors.New("revocation IDs must be sorted and unique")
		}
		out.IssuanceIDs = append(out.IssuanceIDs, parsed)
	}
	trustClass, err := field[string](values, 7, 3)
	if err != nil || TrustClass(trustClass) != Production && TrustClass(trustClass) != Development {
		return out, errors.New("invalid signed revocation trust class")
	}
	out.TrustClass = TrustClass(trustClass)
	canonical, err := encodeMode.Marshal(map[uint64]any{1: version, 2: kind, 3: out.Issuer, 4: sequence, 5: issued, 6: ids, 7: trustClass})
	if err != nil || !bytes.Equal(raw, canonical) {
		return Revocations{}, errors.New("non-deterministic revocation payload")
	}
	return out, nil
}

func parseUUID(raw []byte) (uuid.UUID, error) {
	if len(raw) != 16 {
		return uuid.Nil, errors.New("UUID claim must be 16 bytes")
	}
	id, err := uuid.FromBytes(raw)
	if err != nil {
		return uuid.Nil, err
	}
	if id == uuid.Nil || id.Variant() != uuid.RFC4122 || id.Version() == 0 || id.Version() > 8 {
		return uuid.Nil, errors.New("invalid canonical UUID")
	}
	return id, nil
}

func sortedSlugs(values []string) bool {
	for i, value := range values {
		if !slug.MatchString(value) || (i > 0 && values[i-1] >= value) {
			return false
		}
	}
	return true
}

func asciiIdentifier(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, char := range []byte(value) {
		if char < '!' || char > '~' {
			return false
		}
	}
	return true
}

func validateBinding(binding Binding) error {
	if len(binding.PluginID) > 255 || !pluginID.MatchString(binding.PluginID) || binding.InstallationID == uuid.Nil || binding.OrganizationID == uuid.Nil {
		return errors.New("invalid expected entitlement binding")
	}
	if !sortedSlugs(binding.DeclaredFeatures) {
		return errors.New("invalid declared features")
	}
	return nil
}

func selectKey(trust TrustStore, mode TrustClass, issuer string, kid []byte, purpose Purpose, plugin string, issuedAt int64) (ed25519.PublicKey, error) {
	if (mode != Production && mode != Development) || trust.Class != mode {
		return nil, errors.New("entitlement trust class mismatch")
	}
	var found ed25519.PublicKey
	for _, candidate := range trust.Issuers {
		if candidate.ID != issuer {
			continue
		}
		if plugin != "" && !slices.Contains(candidate.PluginIDs, plugin) {
			return nil, errors.New("issuer not authorized for plugin")
		}
		for _, key := range candidate.Keys {
			if !bytes.Equal(key.ID, kid) {
				continue
			}
			if found != nil {
				return nil, errors.New("ambiguous issuer key")
			}
			if len(key.PublicKey) != ed25519.PublicKeySize || !slices.Contains(key.Purposes, purpose) || key.ValidFrom < 0 || issuedAt < key.ValidFrom || (key.ValidUntil != 0 && (key.ValidUntil <= key.ValidFrom || issuedAt >= key.ValidUntil)) {
				return nil, errors.New("issuer key is invalid for document")
			}
			found = key.PublicKey
		}
	}
	if found == nil {
		return nil, errors.New("unknown entitlement issuer or key")
	}
	return found, nil
}

func checkIssuedAt(issuedAt int64, now time.Time) error {
	seconds := now.UTC().Unix()
	if seconds < 0 || (issuedAt > seconds && issuedAt-seconds > 30) {
		return errors.New("entitlement issued_at exceeds import clock skew")
	}
	return nil
}
