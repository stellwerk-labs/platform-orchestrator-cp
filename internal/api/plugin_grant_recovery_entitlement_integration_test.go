//go:build integration

package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/hecho"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/api/middleware"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/entitlements"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/pluginentitlements"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type pluginGrantRecoveryHTTP struct {
	fixture *pluginEntitlementAPIFixture
	grant   *model.ExternalOperationGrant
	item    plugins.Installation
	call    func(string, string) *httptest.ResponseRecorder
	callAt  func(string, string) *httptest.ResponseRecorder
}

func newPluginGrantRecoveryHTTP(t *testing.T, secondFeature, freshInstallation bool) pluginGrantRecoveryHTTP {
	t.Helper()
	f := newPluginEntitlementAPIFixture(t)
	var operationID uuid.UUID
	tx, err := f.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT operation_id FROM external_operation_grants WHERE org_id=$1 AND plugin_id=$2`, "acme", f.item.ID).Scan(&operationID))
	require.NoError(t, tx.Rollback())
	grant, err := model.GetExternalOperationGrant(t.Context(), f.db, "acme", f.item.ID, operationID)
	require.NoError(t, err)
	item := f.item
	for _, action := range grant.Intent.Actions {
		feature := "feature-a"
		if secondFeature && action == "external-deployment.rollback" {
			feature = "feature-b"
		}
		item.ServiceGrants = append(item.ServiceGrants, plugins.ServiceGrant{Action: action, RequiredFeature: feature, Permission: "plugin." + item.ID + ".control", Scope: "environment"})
	}
	if secondFeature || freshInstallation {
		// A package update can add B while retaining installation identity; a
		// replacement installation instead gets a new UUID and no matching grant.
		if secondFeature {
			item.Entitlement = &plugins.EntitlementDeclaration{Mode: "required", Features: []string{"feature-a", "feature-b"}, Limits: []string{}}
		}
		if freshInstallation {
			item.InstallationID = uuid.NewString()
		}
		digest := sha256.Sum256([]byte("different verified package manifest"))
		item.ManifestSHA256 = hex.EncodeToString(digest[:])
		clock := func() time.Time { return time.Unix(f.clock.Load(), 0).UTC() }
		f.adapter, err = pluginentitlements.New(t.Context(), f.db, []plugins.Installation{item}, entitlements.Development, f.trustPath, "", clock)
		require.NoError(t, err)
	}
	encoded, err := json.Marshal([]plugins.Installation{item})
	require.NoError(t, err)
	registry, err := plugins.LoadInstallations(bytes.NewReader(encoded), false)
	require.NoError(t, err)
	registry.Entitlements = f.adapter
	f.server.Logger = zap.NewNop()
	f.server.PluginGateway = &plugins.Gateway{Registry: registry}
	f.server.PluginEntitlements = f.adapter
	e, err := hecho.DefaultEchoServerWithValidation(&hecho.ValidatedServerConfig{AppName: "plugin-grant-recovery-test", Logger: zap.NewNop(), OpenAPIRawSchema: MustDecodeOpenApiSpec()})
	require.NoError(t, err)
	e.Use(middleware.EchoCtxMiddleware)
	f.server.MapRoutes(e)
	callAt := func(actor, path string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("From", actor)
		response := httptest.NewRecorder()
		e.ServeHTTP(response, request)
		return response
	}
	path := "/orgs/acme/plugins/" + item.ID
	return pluginGrantRecoveryHTTP{fixture: f, grant: grant, item: item, callAt: callAt, call: func(actor, suffix string) *httptest.ResponseRecorder {
		return callAt(actor, path+suffix)
	}}
}

func pluginRecoveryActivationCount(t *testing.T, db model.Databaser) int {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	var count int
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT count(*) FROM plugin_entitlement_feature_activations`).Scan(&count))
	return count
}

func TestPluginGrantRecoveryUsesExactServiceAndLiveEntitlement(t *testing.T) {
	h := newPluginGrantRecoveryHTTP(t, false, false)
	operationPath := "/operations/" + h.grant.OperationID.String()
	for _, actor := range []string{uuid.NewString(), h.item.AdmissionCheckerID, userid.NewServiceUserTokenId().String(), "ffffffff-ffff-ffff-ffff-ffffffffffff"} {
		response := h.call(actor, operationPath)
		require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
	}
	require.Zero(t, pluginRecoveryActivationCount(t, h.fixture.db), "unauthorized preflights must not activate a feature")
	for _, path := range []string{
		"/orgs/another/plugins/" + h.item.ID + operationPath,
		"/orgs/acme/plugins/dev.example.other" + operationPath,
	} {
		response := h.callAt(h.item.ServiceUserID, path)
		require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
	}
	response := h.call(h.item.ServiceUserID, "/operations/"+uuid.NewString())
	require.Equal(t, http.StatusNotFound, response.Code, response.Body.String())
	response = h.call(h.item.ServiceUserID, operationPath)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var view GetPluginOperationGrant200JSONResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &view))
	require.Equal(t, PluginAvailabilityState("available"), *view.Availability)
	require.NotNil(t, view.Entitlement)
	require.Equal(t, "valid", view.Entitlement.Features["feature-a"].State)
	require.Equal(t, "development", view.Entitlement.Features["feature-a"].TrustClass)
	require.True(t, view.Entitlement.Features["feature-a"].Forward)
	require.NotContains(t, response.Body.String(), "development.example")
	require.NotContains(t, response.Body.String(), "cose")
	require.NotContains(t, response.Body.String(), h.fixture.grant.IssuanceID.String())

	h.fixture.clock.Store(h.fixture.base + 21)
	response = h.call(h.item.ServiceUserID, operationPath)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &view))
	require.Equal(t, PluginAvailabilityState("not_entitled"), *view.Availability)
	require.Equal(t, "expired", view.Entitlement.Features["feature-a"].State)
	require.False(t, view.Entitlement.Features["feature-a"].Forward)
	require.True(t, view.Entitlement.Features["feature-a"].Read)
	require.True(t, view.Entitlement.Features["feature-a"].Safety)
	// The frozen service read is not a capability to restart work after expiry.
	for _, actor := range []string{uuid.NewString(), h.item.AdmissionCheckerID} {
		response = h.call(actor, operationPath)
		require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
	}
}

func TestPluginGrantRecoveryRejectsNeverActivatedAndUngrantableFeature(t *testing.T) {
	t.Run("expired before activation", func(t *testing.T) {
		h := newPluginGrantRecoveryHTTP(t, false, false)
		h.fixture.clock.Store(h.fixture.base + 21)
		response := h.call(h.item.ServiceUserID, "/operations/"+h.grant.OperationID.String())
		require.Equal(t, http.StatusConflict, response.Code, response.Body.String())
		require.Zero(t, pluginRecoveryActivationCount(t, h.fixture.db))
	})
	t.Run("another valid feature cannot expose this grant", func(t *testing.T) {
		h := newPluginGrantRecoveryHTTP(t, true, false)
		response := h.call(h.item.ServiceUserID, "/operations/"+h.grant.OperationID.String())
		require.Equal(t, http.StatusConflict, response.Code, response.Body.String())
		_, descriptor, found := h.fixture.server.PluginGateway.Registry.Installation(t.Context(), "acme", h.item.ID)
		require.True(t, found)
		require.Equal(t, plugins.StateAvailable, descriptor.State)
		require.True(t, descriptor.Entitlement.Features["feature-a"].Forward)
		require.False(t, descriptor.Entitlement.Features["feature-b"].Read)
	})
}

func TestPluginEntitlementPreflightIsServiceOnlyBeforeOperationGrant(t *testing.T) {
	t.Run("new installation with no grant", func(t *testing.T) {
		h := newPluginGrantRecoveryHTTP(t, false, true)
		for _, actor := range []string{uuid.NewString(), h.item.AdmissionCheckerID, userid.NewServiceUserTokenId().String()} {
			response := h.call(actor, "/entitlement")
			require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
		}
		for _, path := range []string{
			"/orgs/another/plugins/" + h.item.ID + "/entitlement",
			"/orgs/acme/plugins/dev.example.other/entitlement",
		} {
			response := h.callAt(h.item.ServiceUserID, path)
			require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
		}
		require.Zero(t, pluginRecoveryActivationCount(t, h.fixture.db))
		response := h.call(h.item.ServiceUserID, "/entitlement")
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		var capability GetPluginEntitlement200JSONResponse
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &capability))
		require.Equal(t, PluginAvailabilityState("not_entitled"), capability.State)
		require.NotNil(t, capability.Entitlement)
		require.Equal(t, "not_entitled", capability.Entitlement.Features["feature-a"].State)
		require.False(t, capability.Entitlement.Features["feature-a"].Forward)
		require.Zero(t, pluginRecoveryActivationCount(t, h.fixture.db))
		require.NotContains(t, response.Body.String(), "development.example")
		require.NotContains(t, response.Body.String(), h.fixture.grant.IssuanceID.String())
	})
	t.Run("expired before activation", func(t *testing.T) {
		h := newPluginGrantRecoveryHTTP(t, false, false)
		h.fixture.clock.Store(h.fixture.base + 21)
		response := h.call(h.item.ServiceUserID, "/entitlement")
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		var capability GetPluginEntitlement200JSONResponse
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &capability))
		require.Equal(t, PluginAvailabilityState("not_entitled"), capability.State)
		require.Equal(t, "expired", capability.Entitlement.Features["feature-a"].State)
		require.False(t, capability.Entitlement.Features["feature-a"].Read)
		require.Zero(t, pluginRecoveryActivationCount(t, h.fixture.db))
		response = h.call(h.item.ServiceUserID, "/operations/"+h.grant.OperationID.String())
		require.Equal(t, http.StatusConflict, response.Code, response.Body.String())
	})
	t.Run("valid before this operation exists", func(t *testing.T) {
		h := newPluginGrantRecoveryHTTP(t, false, false)
		missingOperation := uuid.NewString()
		require.Zero(t, pluginRecoveryActivationCount(t, h.fixture.db))
		response := h.call(h.item.ServiceUserID, "/entitlement")
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		var capability GetPluginEntitlement200JSONResponse
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &capability))
		require.Equal(t, PluginAvailabilityState("available"), capability.State)
		require.True(t, capability.Entitlement.Features["feature-a"].Forward)
		require.Equal(t, "development", capability.Entitlement.Features["feature-a"].TrustClass)
		require.Equal(t, 1, pluginRecoveryActivationCount(t, h.fixture.db))
		response = h.call(h.item.ServiceUserID, "/operations/"+missingOperation)
		require.Equal(t, http.StatusNotFound, response.Code, response.Body.String())
	})
}
