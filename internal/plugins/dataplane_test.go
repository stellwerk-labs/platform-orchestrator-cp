package plugins

import (
	"crypto/ed25519"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v2/plugincontext"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDataPlanePortSignsExactRequestWithoutAmbientCredentials(t *testing.T) {
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	now := time.Now().UTC()
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.Empty(t, r.Header.Get("Authorization"))
		assert.Empty(t, r.Header.Get("From"))
		assert.Empty(t, r.Header.Get("Cookie"))
		claims, err := plugincontext.Verify(public, r.Header.Get(plugincontext.Header), plugincontext.Request{Issuer: "core", Audience: "platform-orchestrator-dp", PluginID: "dev.example.workflow", OrgID: "acme", OperationID: "external-deployment.readiness", Method: r.Method, Path: r.URL.EscapedPath(), Body: body, IdempotencyKey: r.Header.Get("Idempotency-Key")}, now)
		if !assert.NoError(t, err) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		assert.Equal(t, "request-1", claims.RequestID)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer server.Close()
	registry, err := LoadInstallations(strings.NewReader("[]"), false)
	require.NoError(t, err)
	port, err := NewDataPlanePort(server.URL, &Gateway{Registry: registry, SigningKey: private, Issuer: "core", Now: func() time.Time { return now }}, true)
	require.NoError(t, err)
	request := DataPlaneRequest{OrgID: "acme", PluginID: "dev.example.workflow", ActorID: uuid.New(), Action: "external-deployment.readiness", RequestID: "request-1", Body: []byte("{\n\"environments\": []\n}")}
	result, err := port.Call(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, result.Status)
	require.JSONEq(t, `{"items":[]}`, string(result.Body))
	request.Action = "https://external.example.test/steal"
	_, err = port.Call(t.Context(), request)
	require.Error(t, err)
	request.Action = "external-deployment.readiness"
	request.OrgID = "../elsewhere"
	_, err = port.Call(t.Context(), request)
	require.Error(t, err)
	require.Equal(t, int64(1), calls.Load())
}

func TestDataPlanePortDoesNotFollowRedirects(t *testing.T) {
	_, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	var leaked atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { leaked.Add(1); w.WriteHeader(200) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	registry, err := LoadInstallations(strings.NewReader("[]"), false)
	require.NoError(t, err)
	port, err := NewDataPlanePort(server.URL, &Gateway{Registry: registry, SigningKey: private, Issuer: "core"}, true)
	require.NoError(t, err)
	_, err = port.Call(t.Context(), DataPlaneRequest{OrgID: "acme", PluginID: "dev.example.workflow", ActorID: uuid.New(), Action: "external-deployment.plan", RequestID: "plan-1", Body: []byte(`{}`)})
	require.ErrorContains(t, err, "redirect")
	require.Zero(t, leaked.Load())
}
