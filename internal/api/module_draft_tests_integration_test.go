//go:build integration

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/hecho"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/api/middleware"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
)

func TestModuleDraftTestSnapshotHTTPCapturesReviewedScopedMaterial(t *testing.T) {
	db := pluginTestDatabase(t)
	grant := seedPluginOperation(t, db, "draft-tests")
	server := &Server{Logger: zap.NewNop(), Database: db}
	e, err := hecho.DefaultEchoServerWithValidation(&hecho.ValidatedServerConfig{AppName: "draft-test-snapshots", Logger: zap.NewNop(), OpenAPIRawSchema: MustDecodeOpenApiSpec()})
	require.NoError(t, err)
	e.Use(middleware.EchoCtxMiddleware)
	server.MapRoutes(e)
	call := func(method, path, key string, body any) *httptest.ResponseRecorder {
		var raw []byte
		if body != nil {
			raw, err = json.Marshal(body)
			require.NoError(t, err)
		}
		request := httptest.NewRequest(method, path, bytes.NewReader(raw))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("From", userid.InternalSystemUuid.String())
		if key != "" {
			request.Header.Set("Idempotency-Key", key)
		}
		response := httptest.NewRecorder()
		e.ServeHTTP(response, request)
		return response
	}
	ctx := t.Context()
	baseline, err := db.GetModuleDefinitionVersion(ctx, nil, grant.OrgID, "module", "1.0.0")
	require.NoError(t, err)
	definition := ModuleVersionPublishBody{SemanticVersion: "1.2.0", ModuleSource: "inline", ModuleSourceCode: ref.Ref(`output "name" { value = "A" }`), ModuleInputs: map[string]any{}, ModuleParams: map[string]ModuleParamItem{}, ProviderMapping: map[string]string{}, Dependencies: map[string]ModuleDependencyManifest{}, Coprovisioned: []ModuleCoProvisionManifest{}, OutputSchema: (*ModuleOutputSchema)(&baseline.OutputSchema)}
	base := "/orgs/" + grant.OrgID
	created := call(http.MethodPost, base+"/modules/module/drafts", "authoring", ModuleDraftCreateBody{Definition: definition, Reason: "Try the first implementation"})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var draft ModuleDraft
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &draft))
	scope := base + "/projects/project/envs/development/modules/module"
	path := scope + "/drafts/" + draft.Id.String() + "/test-snapshots"
	command := ModuleDraftTestCaptureBody{ExpectedResourceVersion: 1, Reason: "Capture reviewed A"}
	captured := call(http.MethodPost, path, "capture-A", command)
	require.Equal(t, http.StatusCreated, captured.Code, captured.Body.String())
	var snapshot ModuleDraftTestSnapshot
	require.NoError(t, json.Unmarshal(captured.Body.Bytes(), &snapshot))
	require.Equal(t, DraftTest, snapshot.Provenance)
	require.Equal(t, int64(1), snapshot.DraftRevision)
	require.Equal(t, definition.ModuleSourceCode, snapshot.Definition.ModuleSourceCode)
	replay := call(http.MethodPost, path, "capture-A", command)
	require.Equal(t, http.StatusCreated, replay.Code, replay.Body.String())
	require.JSONEq(t, captured.Body.String(), replay.Body.String())
	unknown := call(http.MethodPost, path, "replacement-is-not-capture", map[string]any{"expected_resource_version": 1, "reason": "Attempt replacement", "definition": definition})
	require.Equal(t, http.StatusBadRequest, unknown.Code, unknown.Body.String())
	definition.ModuleSourceCode = ref.Ref(`output "name" { value = "B" }`)
	edited := call(http.MethodPut, base+"/modules/module/drafts/"+draft.Id.String(), "save-B-revision", ModuleDraftUpdateBody{ExpectedResourceVersion: 1, Definition: definition, Reason: "Try B without changing intended SemVer"})
	require.Equal(t, http.StatusOK, edited.Code, edited.Body.String())
	stale := call(http.MethodPost, path, "stale-A-revision", command)
	require.Equal(t, http.StatusConflict, stale.Code, stale.Body.String())
	read := call(http.MethodGet, scope+"/draft-test-snapshots/"+snapshot.Id.String(), "", nil)
	require.Equal(t, http.StatusOK, read.Code, read.Body.String())
	require.JSONEq(t, captured.Body.String(), read.Body.String())
	wrong := call(http.MethodGet, base+"/projects/project/envs/development/modules/foreign/draft-test-snapshots/"+snapshot.Id.String(), "", nil)
	require.Equal(t, http.StatusNotFound, wrong.Code, wrong.Body.String())
	_, err = db.GetCoreModuleVersion(ctx, nil, grant.OrgID, "module", snapshot.Id.String(), model.GetModeDefault)
	_, missing := model.IsErrNotFound(err)
	require.True(t, missing)
	require.NotEqual(t, uuid.Nil, snapshot.EnvironmentUuid)
	oldEnvironment, err := db.GetEnvironment(ctx, nil, grant.OrgID, "project", "development", model.GetModeDefault)
	require.NoError(t, err)
	require.NoError(t, db.DeleteEnvironment(ctx, nil, grant.OrgID, "project", "development"))
	recreated, err := db.CreateEnvironment(ctx, nil, oldEnvironment)
	require.NoError(t, err)
	require.NotEqual(t, snapshot.EnvironmentUuid, recreated.Uuid)
	replayInNewScope := call(http.MethodPost, path, "capture-A", command)
	require.Equal(t, http.StatusConflict, replayInNewScope.Code, "a reused slug must not replay another Environment UUID's receipt: %s", replayInNewScope.Body.String())
	oldSnapshotInNewScope := call(http.MethodGet, scope+"/draft-test-snapshots/"+snapshot.Id.String(), "", nil)
	require.Equal(t, http.StatusNotFound, oldSnapshotInNewScope.Code, oldSnapshotInNewScope.Body.String())

}
