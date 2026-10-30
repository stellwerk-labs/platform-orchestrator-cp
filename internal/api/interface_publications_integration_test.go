//go:build integration

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stellwerk-labs/golib/hecho"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/api/middleware"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/moduleversions"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
)

func TestInterfacePublicationRoutesPreserveSelectionAndRejectUnknownFields(t *testing.T) {
	db := pluginTestDatabase(t)
	grant := seedPluginOperation(t, db, "interface-publications")
	server := &Server{Logger: zap.NewNop(), Database: db}
	e, err := hecho.DefaultEchoServerWithValidation(&hecho.ValidatedServerConfig{AppName: "interface-publications", Logger: zap.NewNop(), OpenAPIRawSchema: MustDecodeOpenApiSpec()})
	require.NoError(t, err)
	e.Use(middleware.EchoCtxMiddleware)
	server.MapRoutes(e)
	call := func(path, key string, body any) *httptest.ResponseRecorder {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("From", userid.InternalSystemUuid.String())
		r.Header.Set("Idempotency-Key", key)
		result := httptest.NewRecorder()
		e.ServeHTTP(result, r)
		return result
	}
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	previous, err := db.GetCoreModuleVersion(t.Context(), tx, grant.OrgID, "module", "1.1.0", model.GetModeForUpdate)
	require.NoError(t, err)
	_, err = db.TransitionCoreModuleVersion(t.Context(), tx, grant.OrgID, "module", previous.UUID.String(), moduleversions.LifecycleDeprecated,
		previous.ResourceVersion, grant.ActorID, "Prepare the new interface", nil)
	require.NoError(t, err)
	output := map[string]any{"type": "object", "properties": map[string]any{"endpoint": map[string]any{"type": "string"}}}
	contract, err := db.PublishResourceTypeVersion(t.Context(), tx, grant.OrgID, "generic", model.ResourceTypeVersionPublication{Version: 2, ExpectedResourceVersion: 1, OutputSchema: output, ModuleContract: nil, Actor: grant.ActorID, Reason: "Introduce endpoint interface"})
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	body := ModuleVersionPublishBody{SemanticVersion: "1.2.0-rc.1", ModuleSource: "inline", ResourceTypeVersion: ref.Ref(int64(2)),
		ModuleSourceCode: ref.Ref(`output "endpoint" { value = "example" }`), OutputSchema: ref.Ref(ModuleOutputSchema(output)),
		ModuleInputs: map[string]any{}, ModuleParams: map[string]ModuleParamItem{}, ProviderMapping: map[string]string{},
		Dependencies: map[string]ModuleDependencyManifest{}, Coprovisioned: []ModuleCoProvisionManifest{}}
	path := "/orgs/" + grant.OrgID + "/modules/module/version-publications"
	published := call(path, "interface-prerelease", body)
	require.Equal(t, http.StatusCreated, published.Code, published.Body.String())
	var prerelease CoreModuleVersion
	require.NoError(t, json.Unmarshal(published.Body.Bytes(), &prerelease))
	require.Equal(t, &contract.UUID, prerelease.ResourceTypeVersionUuid)
	missing := body
	missing.ResourceTypeVersion = nil
	rejected := call(path, "missing-selection", missing)
	require.Equal(t, http.StatusBadRequest, rejected.Code, rejected.Body.String())
	encoded, err := json.Marshal(body)
	require.NoError(t, err)
	var unknown map[string]any
	require.NoError(t, json.Unmarshal(encoded, &unknown))
	unknown["resource_type_version_typo"] = 3
	for _, route := range []string{path, "/orgs/" + grant.OrgID + "/modules/module/versions"} {
		rejected = call(route, "unknown-field", unknown)
		require.Equal(t, http.StatusBadRequest, rejected.Code, rejected.Body.String())
	}
	body.SemanticVersion = "1.2.0"
	stable := call("/orgs/"+grant.OrgID+"/modules/module/versions/"+prerelease.Uuid.String()+"/successor-publications", "interface-stable",
		StableModuleVersionSuccessorBody{ExpectedPrereleaseResourceVersion: 1, Reason: "Graduate the exact interface candidate", Version: body})
	require.Equal(t, http.StatusCreated, stable.Code, stable.Body.String())
	var result StableModuleVersionSuccessorResult
	require.NoError(t, json.Unmarshal(stable.Body.Bytes(), &result))
	require.Equal(t, &contract.UUID, result.Stable.ResourceTypeVersionUuid)
	require.Equal(t, ModuleVersionSemanticStatusDeprecated, result.Prerelease.LifecycleStatus)
	require.Equal(t, ModuleVersionSemanticStatusProposed, result.Stable.LifecycleStatus)
}
