package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v2/plugincontext"
)

const (
	testOrg    = "plugin-demo"
	testActor  = "22222222-2222-4222-8222-222222222222"
	testPlugin = "dev.example.read-only"
	testOrigin = "http://127.0.0.1:18090"
)

func TestProtocolHarnessUsesRealGatewayAndFailsClosed(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	now := time.Unix(2_000_000_000, 0).UTC()
	var backendCalls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		backendCalls.Add(1)
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
			return
		}
		claims, err := plugincontext.Verify(public, request.Header.Get(plugincontext.Header), plugincontext.Request{
			Issuer: "harness", Audience: "plugin:" + testPlugin, PluginID: testPlugin,
			OrgID: testOrg, Method: request.Method, Path: request.URL.RequestURI(), Body: body,
		}, now)
		if err != nil {
			t.Error(err)
			return
		}
		assert.Equal(t, testActor, claims.ActorID)
		writeJSON(writer, http.StatusOK, map[string]any{"items": []any{}})
	}))
	defer backend.Close()

	registry, err := plugins.LoadInstallations(strings.NewReader(`[{
		"id":"dev.example.read-only","display_name":"Read Only","version":"0.1.0",
		"backend":"`+backend.URL+`","organizations":{"plugin-demo":"available"},
		"capabilities":["read"],"ui":{"sdk_version":"1","origin":"http://127.0.0.1:18092",
		"entrypoint":"/embed/","route_prefix":"/read-only","navigation_label":"Read Only"},
		"operations":[{"id":"list","method":"GET","path":"/orgs/{orgId}/items","mutating":false}]
	}]`), true)
	require.NoError(t, err)
	gateway := &plugins.Gateway{Registry: registry, SigningKey: private, Issuer: "harness", Now: func() time.Time { return now }}
	handler, err := newHandler(config{ListenAddress: "127.0.0.1:18093", BrowserOrigin: testOrigin, OrgID: testOrg, ActorID: testActor}, gateway)
	require.NoError(t, err)

	request := httptest.NewRequest(http.MethodGet, "/orgs/plugin-demo/plugins", nil)
	request.Header.Set(actorHeader, testActor)
	request.Header.Set("Origin", testOrigin)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assert.Equal(t, http.StatusOK, response.Code)
	assert.Contains(t, response.Body.String(), testPlugin)
	assert.Equal(t, testOrigin, response.Header().Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "true", response.Header().Get("Access-Control-Allow-Credentials"))

	invocation, err := json.Marshal(plugins.Invocation{OperationID: "list"})
	require.NoError(t, err)
	request = httptest.NewRequest(http.MethodPost, "/orgs/plugin-demo/plugins/"+testPlugin+"/invoke", strings.NewReader(string(invocation)))
	request.Header.Set(actorHeader, testActor)
	request.Header.Set("Origin", testOrigin)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assert.Equal(t, http.StatusOK, response.Code)
	assert.Equal(t, int32(1), backendCalls.Load())

	for name, mutate := range map[string]func(*http.Request){
		"wrong actor":  func(request *http.Request) { request.Header.Set(actorHeader, "33333333-3333-4333-8333-333333333333") },
		"wrong origin": func(request *http.Request) { request.Header.Set("Origin", "http://127.0.0.1:19000") },
		"wrong org":    func(request *http.Request) { request.URL.Path = "/orgs/other/plugins/" + testPlugin + "/invoke" },
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/orgs/plugin-demo/plugins/"+testPlugin+"/invoke", strings.NewReader(string(invocation)))
			request.Header.Set(actorHeader, testActor)
			request.Header.Set("Origin", testOrigin)
			mutate(request)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			assert.GreaterOrEqual(t, response.Code, 400)
		})
	}
	unknown, err := json.Marshal(plugins.Invocation{OperationID: "not-registered"})
	require.NoError(t, err)
	request = httptest.NewRequest(http.MethodPost, "/orgs/plugin-demo/plugins/"+testPlugin+"/invoke", strings.NewReader(string(unknown)))
	request.Header.Set(actorHeader, testActor)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assert.Equal(t, http.StatusBadRequest, response.Code)
	assert.Equal(t, int32(1), backendCalls.Load(), "unregistered operation must not reach the backend")
}

func TestProtocolHarnessRequiresLoopbackAndExactOrigin(t *testing.T) {
	for _, cfg := range []config{
		{ListenAddress: ":18093", BrowserOrigin: testOrigin, OrgID: testOrg, ActorID: testActor},
		{ListenAddress: "0.0.0.0:18093", BrowserOrigin: testOrigin, OrgID: testOrg, ActorID: testActor},
		{ListenAddress: "127.0.0.1:18093", BrowserOrigin: "*", OrgID: testOrg, ActorID: testActor},
	} {
		require.Error(t, validateConfig(cfg))
	}
	require.NoError(t, validateConfig(config{ListenAddress: "127.0.0.1:18093", BrowserOrigin: testOrigin, OrgID: testOrg, ActorID: testActor}))
}
