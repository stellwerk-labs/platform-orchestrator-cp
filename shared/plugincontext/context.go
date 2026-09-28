// Package plugincontext implements the experimental v1 request-bound context
// passed from the Orchestrator gateway to a separately running add-on. It is
// deliberately not a bearer session or a grant of arbitrary Core permissions.
package plugincontext

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const Header = "X-Stellwerk-Plugin-Context"
const MaxLifetime = time.Minute

var ErrInvalid = errors.New("invalid plugin request context")

type Claims struct {
	Version     int    `json:"version"`
	Issuer      string `json:"issuer"`
	Audience    string `json:"audience"`
	PluginID    string `json:"plugin_id"`
	OrgID       string `json:"org_id"`
	ActorID     string `json:"actor_id"`
	OperationID string `json:"operation_id"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	BodySHA256  string `json:"body_sha256"`
	RequestID   string `json:"request_id"`
	IssuedAt    int64  `json:"issued_at"`
	ExpiresAt   int64  `json:"expires_at"`
}

type Request struct {
	Issuer, Audience, PluginID, OrgID, Method, Path string
	OperationID, IdempotencyKey                     string
	Body                                            []byte
}

func Digest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func (c Claims) valid(now time.Time) bool {
	return c.Version == 1 && c.Issuer != "" && c.Audience != "" &&
		c.PluginID != "" && c.OrgID != "" && c.ActorID != "" &&
		c.OperationID != "" && c.Method != "" && strings.HasPrefix(c.Path, "/") &&
		len(c.BodySHA256) == 64 && c.RequestID != "" &&
		c.IssuedAt > 0 && c.IssuedAt <= now.Unix()+5 && c.ExpiresAt > now.Unix() &&
		c.ExpiresAt > c.IssuedAt && c.ExpiresAt-c.IssuedAt <= int64(MaxLifetime.Seconds())
}

func Sign(key ed25519.PrivateKey, claims Claims, now time.Time) (string, error) {
	if len(key) != ed25519.PrivateKeySize || !claims.valid(now) {
		return "", ErrInvalid
	}
	data, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	message := "v1." + base64.RawURLEncoding.EncodeToString(data)
	signature := ed25519.Sign(key, []byte(message))
	return message + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// Verify binds an assertion to one exact request and receiver. Mutation handlers
// must additionally reauthorize and durably deduplicate claims.RequestID with
// the body digest. The operation must come from the selected server handler,
// not the assertion. IdempotencyKey is the received HTTP header, required for
// writes and checked against the signed identity. Verification alone does not
// provide exactly-once side effects.
func Verify(key ed25519.PublicKey, token string, request Request, now time.Time) (Claims, error) {
	var claims Claims
	if len(key) != ed25519.PublicKeySize || len(token) > 16384 {
		return claims, ErrInvalid
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return claims, ErrInvalid
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), signature) {
		return claims, ErrInvalid
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return claims, ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&claims) != nil {
		return Claims{}, ErrInvalid
	}
	canonical, err := json.Marshal(claims)
	if err != nil || !bytes.Equal(data, canonical) || !claims.valid(now) {
		return Claims{}, ErrInvalid
	}
	if request.Issuer == "" || request.Audience == "" || request.PluginID == "" || request.OrgID == "" || request.OperationID == "" ||
		claims.Issuer != request.Issuer || claims.Audience != request.Audience ||
		claims.PluginID != request.PluginID || claims.OrgID != request.OrgID ||
		claims.OperationID != request.OperationID ||
		claims.Method != request.Method || claims.Path != request.Path || claims.BodySHA256 != Digest(request.Body) {
		return Claims{}, ErrInvalid
	}
	if (request.Method != "GET" && request.Method != "HEAD" && request.IdempotencyKey == "") ||
		(request.IdempotencyKey != "" && request.IdempotencyKey != claims.RequestID) {
		return Claims{}, ErrInvalid
	}
	return claims, nil
}
