package plugins

import (
	"crypto/ed25519"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/plugincontext"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestAdminOnlyOperationFailsClosedWithoutCanonicalIAM(t *testing.T) {
	_, key, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	item := Installation{ID: testPluginID, DisplayName: "Observer", Version: "1.0.0", Backend: "http://127.0.0.1:1", Organizations: map[string]AvailabilityState{"acme": StateAvailable}, Operations: []Operation{{ID: "read", Method: "GET", Path: "/orgs/{orgId}/read", RequireOrgAdmin: true}}}
	registry, err := NewInstallationRegistry([]Installation{item}, true)
	require.NoError(t, err)
	gateway := &Gateway{Registry: registry, SigningKey: key, Issuer: "cp"}
	_, err = gateway.Invoke(t.Context(), "acme", item.ID, testActor, Invocation{OperationID: "read"})
	require.ErrorContains(t, err, "Canonical organization Admin membership")
	var legacy Operation
	require.NoError(t, json.Unmarshal([]byte(`{"id":"legacy","method":"GET","path":"/orgs/{orgId}/legacy"}`), &legacy))
	require.False(t, legacy.RequireOrgAdmin)
}
func TestUnknownOptionalProtocolMakesInstallationIncompatible(t *testing.T) {
	item := Installation{ID: testPluginID, DisplayName: "Observer", Version: "1.0.0", Backend: "http://127.0.0.1:1", Organizations: map[string]AvailabilityState{"acme": StateAvailable}, RequiredProtocols: []string{"environment-observation/v2"}}
	// Protocol requirements are declarations, so a future version must install
	// as incompatible rather than accidentally receiving a lower protocol.
	registry, err := NewInstallationRegistry([]Installation{item}, true)
	require.NoError(t, err)
	descriptor, ok := registry.Get(t.Context(), "acme", item.ID)
	require.True(t, ok)
	require.Equal(t, StateIncompatible, descriptor.State)
}
func TestObservationDataPlanePortsAreClosedAndExact(t *testing.T) {
	request := DataPlaneRequest{OrgID: "acme", PluginID: testPluginID, ActorID: uuid.New(), RequestID: uuid.NewString(), Action: "observation.request.get", ObservationID: uuid.New()}
	method, path, err := dataPlaneRequestPath(request)
	require.NoError(t, err)
	require.Equal(t, "GET", method)
	require.Equal(t, "/orgs/acme/plugins/"+testPluginID+"/observations/v1/requests/"+request.ObservationID.String(), path)
	request.Body = json.RawMessage(`{}`)
	_, _, err = dataPlaneRequestPath(request)
	require.Error(t, err)
	request.Action = "observation.apply"
	_, _, err = dataPlaneRequestPath(request)
	require.Error(t, err)
}

func TestRequiredObservationProtocolRejectsOldDPWithoutAffectingLegacyPlugin(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusOK} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			public, key, err := ed25519.GenerateKey(nil)
			require.NoError(t, err)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, err := plugincontext.Verify(public, r.Header.Get(plugincontext.Header), plugincontext.Request{Issuer: "cp", Audience: "platform-orchestrator-dp", PluginID: testPluginID, OrgID: "acme", OperationID: "observation.capabilities", Method: http.MethodGet, Path: r.URL.RequestURI(), IdempotencyKey: r.Header.Get("Idempotency-Key")}, time.Now().UTC())
				if err != nil {
					t.Errorf("capability assertion verification failed: %v", err)
					w.WriteHeader(http.StatusForbidden)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"protocols":[]}`))
			}))
			defer server.Close()
			permission := "plugin." + testPluginID + ".manage"
			item := Installation{InstallationID: uuid.NewString(), ID: testPluginID, DisplayName: "Observer", Version: "1.0.0", Backend: server.URL, Organizations: map[string]AvailabilityState{"acme": StateAvailable}, RequiredProtocols: []string{EnvironmentObservationProtocol}, ServiceUserID: userid.NewServiceUserTokenId().String(), AdmissionCheckerID: userid.NewServiceUserTokenId().String(), Capabilities: []Capability{{ID: permission, DisplayName: "Manage", Description: "Manage observer", Category: "Observer", Level: "manage", Scopes: []string{"organization"}}}, ServiceGrants: []ServiceGrant{{Action: "observation.capabilities", Permission: permission, Scope: "organization"}}}
			legacy := Installation{ID: "dev.example.legacy", DisplayName: "Legacy", Version: "1.0.0", Backend: server.URL, Organizations: map[string]AvailabilityState{"acme": StateAvailable}}
			registry, err := NewInstallationRegistry([]Installation{item, legacy}, true)
			require.NoError(t, err)
			gateway := &Gateway{Registry: registry, SigningKey: key, Issuer: "cp"}
			port, err := NewDataPlanePort(server.URL, gateway, true)
			require.NoError(t, err)
			registry.Protocols = port
			for range 2 {
				descriptor, ok := registry.Get(t.Context(), "acme", item.ID)
				require.True(t, ok)
				require.Equal(t, StateIncompatible, descriptor.State)
			}
			descriptor, ok := registry.Get(t.Context(), "acme", legacy.ID)
			require.True(t, ok)
			require.Equal(t, StateAvailable, descriptor.State)
			require.Equal(t, int32(1), calls.Load(), "bounded compatibility result is cached; unrelated plugins do not probe DP")
		})
	}
}

func TestObservationRecoveryRequiresSeparateCapabilityWithoutChangingOldProtocol(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		name := "old-dp"
		if recovery {
			name = "recovery-dp"
		}
		t.Run(name, func(t *testing.T) {
			_, key, e := ed25519.GenerateKey(nil)
			require.NoError(t, e)
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				protocols := []string{EnvironmentObservationProtocol}
				if recovery {
					protocols = append(protocols, EnvironmentObservationRecoveryProtocol)
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"protocols": protocols})
			}))
			t.Cleanup(peer.Close)
			old := Installation{ID: "dev.example.old-observer", InstallationID: uuid.NewString(), ManifestSHA256: "old", DisplayName: "Old observer", Version: "1.0.0", Backend: peer.URL, Organizations: map[string]AvailabilityState{"acme": StateAvailable}, RequiredProtocols: []string{EnvironmentObservationProtocol}, ServiceUserID: userid.NewServiceUserTokenId().String()}
			current := old
			current.ID = "dev.example.new-observer"
			current.InstallationID = uuid.NewString()
			current.RequiredProtocols = []string{EnvironmentObservationProtocol, EnvironmentObservationRecoveryProtocol}
			for _, item := range []*Installation{&old, &current} {
				permission := "plugin." + item.ID + ".manage"
				item.AdmissionCheckerID = userid.NewServiceUserTokenId().String()
				item.ServiceGrants = []ServiceGrant{{Action: "observation.capabilities", Permission: permission, Scope: "organization"}}
				item.Capabilities = []Capability{{ID: permission, DisplayName: "Manage", Description: "Manage observer", Category: "Observer", Level: "manage", Scopes: []string{"organization"}}}
			}
			registry, e := NewInstallationRegistry([]Installation{old, current}, true)
			require.NoError(t, e)
			gateway := &Gateway{Registry: registry, SigningKey: key, Issuer: "cp"}
			port, e := NewDataPlanePort(peer.URL, gateway, true)
			require.NoError(t, e)
			registry.Protocols = port
			descriptor, ok := registry.Get(t.Context(), "acme", old.ID)
			require.True(t, ok)
			require.Equal(t, StateAvailable, descriptor.State)
			descriptor, ok = registry.Get(t.Context(), "acme", current.ID)
			require.True(t, ok)
			expected := StateIncompatible
			if recovery {
				expected = StateAvailable
			}
			require.Equal(t, expected, descriptor.State)
		})
	}
	for _, test := range []struct{ action, collection string }{{"observation.request.resolve", "requests"}, {"observation.redeploy.resolve", "redeployments"}} {
		request := DataPlaneRequest{OrgID: "acme", PluginID: testPluginID, ActorID: uuid.New(), RequestID: uuid.NewString(), Action: test.action, ObservationID: uuid.New(), Body: json.RawMessage(`{}`)}
		method, path, e := dataPlaneRequestPath(request)
		require.NoError(t, e)
		require.Equal(t, "POST", method)
		require.Equal(t, "/orgs/acme/plugins/"+testPluginID+"/observations/v1/"+test.collection+"/"+request.ObservationID.String()+"/resolve", path)
		request.Body = nil
		_, _, e = dataPlaneRequestPath(request)
		require.Error(t, e)
	}
}
