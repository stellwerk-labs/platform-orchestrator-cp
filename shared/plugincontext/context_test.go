package plugincontext

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"
)

func TestRequestBoundContext(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2000000000, 0)
	req := Request{Issuer: "cp", Audience: "plugin:dev.example.demo", PluginID: "dev.example.demo", OrgID: "demo", Method: "POST", Path: "/v1/commands?view=exact", Body: []byte(`{"action":"start"}`)}
	claims := Claims{Version: 1, Issuer: req.Issuer, Audience: req.Audience, PluginID: req.PluginID, OrgID: req.OrgID, ActorID: "actor", OperationID: "start", Method: req.Method, Path: req.Path, BodySHA256: Digest(req.Body), RequestID: "request", IssuedAt: now.Unix(), ExpiresAt: now.Add(MaxLifetime).Unix()}
	token, err := Sign(key, claims, now)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Verify(pub, token, req, now)
	if err != nil || got != claims {
		t.Fatalf("round trip: %v", err)
	}
	for name, mutate := range map[string]func(*Request){
		"issuer":   func(r *Request) { r.Issuer = "other" },
		"audience": func(r *Request) { r.Audience = "dp" },
		"plugin":   func(r *Request) { r.PluginID = "dev.other.demo" },
		"org":      func(r *Request) { r.OrgID = "other" },
		"method":   func(r *Request) { r.Method = "DELETE" },
		"path":     func(r *Request) { r.Path = "/v1/other" },
		"body":     func(r *Request) { r.Body = []byte(`{"action":"delete"}`) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := req
			mutate(&changed)
			if _, err := Verify(pub, token, changed, now); err == nil {
				t.Fatal("accepted mismatched request")
			}
		})
	}
	if _, err := Verify(pub, token, req, now.Add(MaxLifetime)); err == nil {
		t.Fatal("accepted expired assertion")
	}
	if _, err := Verify(pub, token, req, now.Add(-6*time.Second)); err == nil {
		t.Fatal("accepted future assertion")
	}
	if _, err := Verify(pub, strings.Replace(token, "v1.", "v2.", 1), req, now); err == nil {
		t.Fatal("accepted unknown protocol")
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := Verify(other, token, req, now); err == nil {
		t.Fatal("accepted wrong signer")
	}
	claims.ExpiresAt++
	if _, err := Sign(key, claims, now); err == nil {
		t.Fatal("signed excessive lifetime")
	}
}
