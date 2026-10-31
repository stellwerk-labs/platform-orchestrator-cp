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

func TestModuleDraftHTTPPublishesOnlyTheReviewedStoredRevision(t *testing.T) {
	db := pluginTestDatabase(t)
	grant := seedPluginOperation(t, db, "draft-authoring")
	server := &Server{Logger: zap.NewNop(), Database: db}
	e, err := hecho.DefaultEchoServerWithValidation(&hecho.ValidatedServerConfig{AppName: "module-drafts", Logger: zap.NewNop(), OpenAPIRawSchema: MustDecodeOpenApiSpec()})
	require.NoError(t, err)
	e.Use(middleware.EchoCtxMiddleware)
	server.MapRoutes(e)
	call := func(method, path, key string, body any) *httptest.ResponseRecorder {
		var raw []byte
		if body != nil {
			raw, err = json.Marshal(body)
			require.NoError(t, err)
		}
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("From", userid.InternalSystemUuid.String())
		if key != "" {
			r.Header.Set("Idempotency-Key", key)
		}
		response := httptest.NewRecorder()
		e.ServeHTTP(response, r)
		return response
	}
	path := "/orgs/" + grant.OrgID + "/modules/module/drafts"
	definition := ModuleVersionPublishBody{SemanticVersion: "1.2.0", ModuleSource: "inline",
		ModuleSourceCode: ref.Ref(`output "name" { value = "first attempt" }`),
		ModuleInputs:     map[string]any{}, ModuleParams: map[string]ModuleParamItem{}, ProviderMapping: map[string]string{},
		Dependencies: map[string]ModuleDependencyManifest{}, Coprovisioned: []ModuleCoProvisionManifest{},
		OutputSchema: ref.Ref(ModuleOutputSchema{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}})}
	create := ModuleDraftCreateBody{Definition: definition, Reason: "Start authoring the next version"}
	created := call(http.MethodPost, path, "create-draft", create)
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var draft ModuleDraft
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &draft))
	require.Equal(t, int64(1), draft.ResourceVersion)
	replay := call(http.MethodPost, path, "create-draft", create)
	require.Equal(t, http.StatusCreated, replay.Code, replay.Body.String())
	require.JSONEq(t, created.Body.String(), replay.Body.String())
	draftPath := path + "/" + draft.Id.String()
	definition.ModuleSourceCode = ref.Ref(`output "name" { value = "second attempt" }`)
	save := ModuleDraftUpdateBody{Definition: definition, ExpectedResourceVersion: 1, Reason: "Try the second implementation"}
	saved := call(http.MethodPut, draftPath, "save-draft", save)
	require.Equal(t, http.StatusOK, saved.Code, saved.Body.String())
	require.NoError(t, json.Unmarshal(saved.Body.Bytes(), &draft))
	require.Equal(t, int64(2), draft.ResourceVersion)
	stale := call(http.MethodPut, draftPath, "stale-save", save)
	require.Equal(t, http.StatusConflict, stale.Code, stale.Body.String())
	old := call(http.MethodGet, draftPath+"/revisions/1", "", nil)
	require.Equal(t, http.StatusOK, old.Code, old.Body.String())
	var first ModuleDraftRevision
	require.NoError(t, json.Unmarshal(old.Body.Bytes(), &first))
	require.Equal(t, create.Definition.ModuleSourceCode, first.Definition.ModuleSourceCode)
	_, err = db.GetCoreModuleVersion(t.Context(), nil, grant.OrgID, "module", "1.2.0", model.GetModeDefault)
	_, missing := model.IsErrNotFound(err)
	require.True(t, missing, "saving two implementations must not publish or reserve 1.2.0")
	command := ModuleReasonedCommand{ExpectedResourceVersion: 1, Reason: "Publish reviewed source"}
	stale = call(http.MethodPost, draftPath+"/publish", "stale-publish", command)
	require.Equal(t, http.StatusConflict, stale.Code, stale.Body.String())
	command.ExpectedResourceVersion = 2
	blocked := call(http.MethodPost, draftPath+"/publish", "blocked-by-proposed", command)
	require.Equal(t, http.StatusConflict, blocked.Code, blocked.Body.String())
	unchanged, err := db.GetModuleDraft(t.Context(), nil, grant.OrgID, "module", draft.Id, model.GetModeDefault)
	require.NoError(t, err)
	require.Equal(t, int64(2), unchanged.ResourceVersion)
	require.Equal(t, "open", unchanged.Status)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	proposed, err := db.GetCoreModuleVersion(t.Context(), tx, grant.OrgID, "module", "1.1.0", model.GetModeForUpdate)
	require.NoError(t, err)
	_, err = db.TransitionCoreModuleVersion(t.Context(), tx, grant.OrgID, "module", proposed.UUID.String(), moduleversions.LifecycleDeprecated,
		proposed.ResourceVersion, grant.ActorID, "Retire the previous candidate", nil)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	invalidCommand := command
	invalidCommand.Reason = "   "
	invalid := call(http.MethodPost, draftPath+"/publish", "rejected-publication", invalidCommand)
	require.Equal(t, http.StatusBadRequest, invalid.Code, invalid.Body.String())
	_, err = db.GetCoreModuleVersion(t.Context(), nil, grant.OrgID, "module", "1.2.0", model.GetModeDefault)
	_, missing = model.IsErrNotFound(err)
	require.True(t, missing, "failure to close the Draft must roll back the entire publication")
	published := call(http.MethodPost, draftPath+"/publish", "publish-draft", command)
	require.Equal(t, http.StatusCreated, published.Code, published.Body.String())
	var publication ModuleDraftPublication
	require.NoError(t, json.Unmarshal(published.Body.Bytes(), &publication))
	require.Equal(t, ModuleDraftStatusPublished, publication.Draft.Status)
	require.Equal(t, int64(3), publication.Draft.ResourceVersion)
	require.Equal(t, "1.2.0", *publication.Version.SemanticVersion)
	require.Equal(t, ModuleVersionSemanticStatusProposed, publication.Version.LifecycleStatus)
	replay = call(http.MethodPost, draftPath+"/publish", "publish-draft", command)
	require.Equal(t, http.StatusCreated, replay.Code, replay.Body.String())
	require.JSONEq(t, published.Body.String(), replay.Body.String())
	frozen, err := db.GetModuleDefinitionVersion(t.Context(), nil, grant.OrgID, "module", publication.Version.Uuid.String())
	require.NoError(t, err)
	require.Equal(t, definition.ModuleSourceCode, frozen.ModuleSourceCode.Ref())
	current, err := db.GetModuleCatalogue(t.Context(), nil, grant.OrgID, "module", model.GetModeDefault)
	require.NoError(t, err)
	require.NotEqual(t, publication.Version.Uuid, *current.CurrentDefaultVersionUUID, "publishing must never promote implicitly")
	save.ExpectedResourceVersion = 3
	closed := call(http.MethodPut, draftPath, "edit-closed", save)
	require.Equal(t, http.StatusConflict, closed.Code, closed.Body.String())
	definition.SemanticVersion = "1.3.0"
	definition.ModuleSource = "git::https://example.com/module.git?ref=work-in-progress"
	definition.ModuleSourceCode = nil
	create.Definition = definition
	create.Reason = "Save a source before choosing the exact revision"
	floating := call(http.MethodPost, path, "floating-draft", create)
	require.Equal(t, http.StatusCreated, floating.Code, floating.Body.String())
	var unresolved ModuleDraft
	require.NoError(t, json.Unmarshal(floating.Body.Bytes(), &unresolved))
	invalid = call(http.MethodPost, path+"/"+unresolved.Id.String()+"/publish", "unresolved-publication",
		ModuleReasonedCommand{ExpectedResourceVersion: 1, Reason: "Attempt publication without an exact revision"})
	require.Equal(t, http.StatusBadRequest, invalid.Code, invalid.Body.String())
}
