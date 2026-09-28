package plugins

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v2/plugincontext"
)

const testPluginID = "dev.example.change-manager"
const testActor = "b6636ae8-e388-4d4a-b5e6-c4f84bfa538c"

func testRegistry(t *testing.T, backend string, state AvailabilityState) *InstallationRegistry {
	t.Helper()
	data, err := json.Marshal([]Installation{{ID: testPluginID, DisplayName: "Change Manager", Version: "1.0.0", Backend: backend, Organizations: map[string]AvailabilityState{"acme": state}, Operations: []Operation{
		{ID: "list", Method: "GET", Path: "/orgs/{orgId}/changes", QueryKeys: []string{"state"}},
		{ID: "command", Method: "POST", Path: "/orgs/{orgId}/changes/{changeId}/{command}", Mutating: true, PathValues: map[string][]string{"command": {"start", "cancel", "rollback"}}, SafetyPathValues: map[string][]string{"command": {"cancel", "rollback"}}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := LoadInstallations(strings.NewReader(string(data)), true)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestGatewayRequestBindingAndCredentialIsolation(t *testing.T) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2000000000, 0)
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		claims, err := plugincontext.Verify(public, r.Header.Get(plugincontext.Header), plugincontext.Request{Issuer: "cp", Audience: "plugin:" + testPluginID, PluginID: testPluginID, OrgID: "acme", Method: r.Method, Path: r.URL.RequestURI(), Body: body, OperationID: "command", IdempotencyKey: r.Header.Get("Idempotency-Key")}, now)
		if err != nil {
			t.Errorf("delegation: %v", err)
		}
		if claims.ActorID != testActor || claims.RequestID != "request-1" || r.Header.Get("Idempotency-Key") != "request-1" {
			t.Error("lost actor/idempotency binding")
		}
		for _, header := range []string{"Authorization", "Cookie", "From"} {
			if r.Header.Get(header) != "" {
				t.Errorf("leaked %s", header)
			}
		}
		w.Header().Set("Set-Cookie", "never-forward=this")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"accepted":true}`))
	}))
	defer backend.Close()
	gateway := Gateway{Registry: testRegistry(t, backend.URL, StateAvailable), SigningKey: key, Issuer: "cp", Now: func() time.Time { return now }}
	invocation := Invocation{OperationID: "command", PathParameters: map[string]string{"changeId": "release-1", "command": "start"}, Body: json.RawMessage(`{"revision":1}`), IdempotencyKey: "request-1"}
	result, err := gateway.Invoke(context.Background(), "acme", testPluginID, testActor, invocation)
	if err != nil || result.Status != 202 || string(result.Body) != `{"accepted":true}` {
		t.Fatalf("invoke: %#v %v", result, err)
	}
	for name, alter := range map[string]func(*Invocation){
		"unregistered":        func(i *Invocation) { i.OperationID = "delete-everything" },
		"missing-idempotency": func(i *Invocation) { i.IdempotencyKey = "" },
		"path-escape":         func(i *Invocation) { i.PathParameters = map[string]string{"changeId": "../escape", "command": "start"} },
		"forged-org": func(i *Invocation) {
			i.PathParameters = map[string]string{"orgId": "other", "changeId": "x", "command": "start"}
		},
		"unknown-command":    func(i *Invocation) { i.PathParameters = map[string]string{"changeId": "x", "command": "delete"} },
		"unregistered-query": func(i *Invocation) { i.Query = map[string][]string{"target": {"http://elsewhere"}} },
	} {
		t.Run(name, func(t *testing.T) {
			changed := invocation
			alter(&changed)
			if _, err := gateway.Invoke(context.Background(), "acme", testPluginID, testActor, changed); err == nil {
				t.Fatal("accepted invalid invocation")
			}
		})
	}
	if _, err := gateway.Invoke(context.Background(), "other", testPluginID, testActor, invocation); err == nil {
		t.Fatal("cross-org invocation accepted")
	}
	if calls.Load() != 1 {
		t.Fatalf("unsafe request reached backend: %d", calls.Load())
	}
}

func TestGatewayAvailabilityAndSafetyOperations(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	defer backend.Close()
	for _, state := range []AvailabilityState{StateAvailable, StateNotEnabled, StateNotEntitled, StateIncompatible, StateUnavailable} {
		t.Run(string(state), func(t *testing.T) {
			gateway := Gateway{Registry: testRegistry(t, backend.URL, state), SigningKey: key, Issuer: "cp"}
			for _, command := range []string{"start", "cancel", "rollback"} {
				invocation := Invocation{OperationID: "command", PathParameters: map[string]string{"changeId": "x", "command": command}, IdempotencyKey: "request"}
				_, err := gateway.Invoke(context.Background(), "acme", testPluginID, testActor, invocation)
				want := state == StateAvailable || (state == StateNotEnabled || state == StateNotEntitled) && command != "start"
				if (err == nil) != want {
					t.Errorf("%s allowed=%v err=%v", command, want, err)
				}
			}
		})
	}
}

func TestGatewayRefusesRedirectAndInvalidJSON(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer target.Close()
	for _, mode := range []string{"redirect", "invalid-json"} {
		t.Run(mode, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "redirect" {
					http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
				} else {
					_, _ = w.Write([]byte("not json"))
				}
			}))
			defer backend.Close()
			gateway := Gateway{Registry: testRegistry(t, backend.URL, StateAvailable), SigningKey: key, Issuer: "cp"}
			_, err := gateway.Invoke(context.Background(), "acme", testPluginID, testActor, Invocation{OperationID: "list", Query: map[string][]string{"state": {"running", "paused"}}})
			var gatewayError *GatewayError
			if !errors.As(err, &gatewayError) || gatewayError.Status != 502 {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
	if redirected.Load() != 0 {
		t.Fatal("followed a plugin redirect")
	}
}

func TestInstallationValidationAndIsolation(t *testing.T) {
	registry := testRegistry(t, "http://127.0.0.1:1234", StateAvailable)
	if len(registry.List(context.Background(), "other")) != 0 {
		t.Fatal("leaked another org's installation")
	}
	item, _, _ := registry.Installation(context.Background(), "acme", testPluginID)
	item.Organizations["other"] = StateAvailable
	item.Operations[0].QueryKeys[0] = "evil"
	fresh, _, _ := registry.Installation(context.Background(), "acme", testPluginID)
	if fresh.Operations[0].QueryKeys[0] != "state" || len(registry.List(context.Background(), "other")) != 0 {
		t.Fatal("caller mutated registry")
	}
	op := fresh.Operations[0]
	path, err := op.RequestPath("acme", nil, map[string][]string{"state": {"running", "paused"}})
	if err != nil || path != "/orgs/acme/changes?state=running&state=paused" {
		t.Fatalf("repeated query lost: %s %v", path, err)
	}
	for _, url := range []string{"https://user:secret@example.org", "https://example.org/path", "https://example.org?token=x", "file:///tmp/plugin", "http://localhost:1234"} {
		if err := validateOrigin(url, false); err == nil {
			t.Errorf("accepted unsafe origin %s", url)
		}
	}
	if _, err := LoadInstallations(strings.NewReader(`[] {}`), false); err == nil {
		t.Fatal("accepted trailing JSON")
	}
	if _, err := LoadInstallations(strings.NewReader(`[{"unknown":true}]`), false); err == nil {
		t.Fatal("accepted unknown fields")
	}
}
