//go:build integration

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/hecho"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/api/middleware"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
)

// This exercises the real HTTP validation, command receipts and PostgreSQL
// transactions as the internal system principal. Human RBAC is covered by the
// service integration suite against IAM, not by a replacement authorization peer.
func TestResourceTypeVersionsHTTPPublicationAndHistory(t *testing.T) {
	db := pluginTestDatabase(t)
	org := "contracts-" + uuid.NewString()
	actor := userid.InternalSystemUuid
	_, err := db.CreateOrg(t.Context(), nil, &model.Org{Id: org, CreatedAt: time.Now(), CreatedBy: actor, Source: model.OrgSourceInternal, Plan: model.OrgPlanCustom})
	require.NoError(t, err)
	_, err = db.CreateResourceType(t.Context(), nil, &model.ResourceType{OrgId: opt.Of(org), Id: "service", CreatedAt: time.Now(), OutputsSchema: map[string]any{}})
	require.NoError(t, err)
	_, err = db.CreateResourceType(t.Context(), nil, &model.ResourceType{Id: "builtin", CreatedAt: time.Now(), OutputsSchema: map[string]any{}})
	require.NoError(t, err)
	server := &Server{Logger: zap.NewNop(), Database: db}
	e, err := hecho.DefaultEchoServerWithValidation(&hecho.ValidatedServerConfig{AppName: "resource-type-versions", Logger: zap.NewNop(), OpenAPIRawSchema: MustDecodeOpenApiSpec()})
	require.NoError(t, err)
	e.Use(middleware.EchoCtxMiddleware)
	server.MapRoutes(e)
	call := func(method, path, key, principal string, body any) *httptest.ResponseRecorder {
		var raw []byte
		if body != nil {
			raw, err = json.Marshal(body)
			require.NoError(t, err)
		}
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("From", principal)
		if key != "" {
			r.Header.Set("Idempotency-Key", key)
		}
		result := httptest.NewRecorder()
		e.ServeHTTP(result, r)
		return result
	}
	path := "/orgs/" + org + "/resource-types/service/versions"
	rejectedEdit := call(http.MethodPatch, "/orgs/"+org+"/resource-types/service", "", actor.String(), map[string]any{"output_schema": map[string]any{}})
	require.Equal(t, http.StatusConflict, rejectedEdit.Code, rejectedEdit.Body.String())
	require.Contains(t, rejectedEdit.Body.String(), "publish a new interface version")
	body := ResourceTypeVersionPublishBody{Version: 2, ExpectedResourceVersion: 1, Reason: "Introduce endpoint", OutputSchema: map[string]any{"type": "object"}}
	skipped := body
	skipped.Version = 3
	rejectedNumber := call(http.MethodPost, path, "skip-version", actor.String(), skipped)
	require.Equal(t, http.StatusConflict, rejectedNumber.Code, rejectedNumber.Body.String())
	created := call(http.MethodPost, path, "publish-2", actor.String(), body)
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var version ResourceTypeVersion
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &version))
	require.Equal(t, int64(2), version.Version)
	require.Equal(t, &actor, version.CreatedBy)
	duplicate := body
	duplicate.ExpectedResourceVersion = 2
	rejectedNumber = call(http.MethodPost, path, "duplicate-version", actor.String(), duplicate)
	require.Equal(t, http.StatusConflict, rejectedNumber.Code, rejectedNumber.Body.String())
	replayed := call(http.MethodPost, path, "publish-2", actor.String(), body)
	require.Equal(t, http.StatusCreated, replayed.Code, replayed.Body.String())
	require.JSONEq(t, created.Body.String(), replayed.Body.String())
	denied := call(http.MethodPost, path, "publish-2", "", body)
	require.Equal(t, http.StatusUnauthorized, denied.Code, denied.Body.String())
	body.Reason = "Changed command"
	conflict := call(http.MethodPost, path, "publish-2", actor.String(), body)
	require.Equal(t, http.StatusConflict, conflict.Code, conflict.Body.String())
	stale := call(http.MethodPost, path, "stale-command", actor.String(), body)
	require.Equal(t, http.StatusConflict, stale.Code, stale.Body.String())
	page := call(http.MethodGet, path+"?per_page=1", "", actor.String(), nil)
	require.Equal(t, http.StatusOK, page.Code, page.Body.String())
	var history ListResourceTypeVersions200JSONResponse
	require.NoError(t, json.Unmarshal(page.Body.Bytes(), &history))
	require.Len(t, history.Items, 1)
	require.Equal(t, int64(1), history.Items[0].Version)
	require.NotNil(t, history.NextPageToken)
	page = call(http.MethodGet, path+"?per_page=1&page="+*history.NextPageToken, "", actor.String(), nil)
	require.Equal(t, http.StatusOK, page.Code, page.Body.String())
	require.NoError(t, json.Unmarshal(page.Body.Bytes(), &history))
	require.Len(t, history.Items, 1)
	require.Equal(t, version.Uuid, history.Items[0].Uuid)
	for _, invalid := range []string{"?page=-1", "?page=bad", "?per_page=101"} {
		result := call(http.MethodGet, path+invalid, "", actor.String(), nil)
		require.Equal(t, http.StatusBadRequest, result.Code, result.Body.String())
	}
	read := call(http.MethodGet, path+"/2", "", actor.String(), nil)
	require.Equal(t, http.StatusOK, read.Code, read.Body.String())
	require.JSONEq(t, created.Body.String(), read.Body.String())
	deleted := call(http.MethodDelete, "/orgs/"+org+"/resource-types/service", "", actor.String(), nil)
	require.Equal(t, http.StatusConflict, deleted.Code, deleted.Body.String())
	foreign := call(http.MethodGet, "/orgs/foreign/resource-types/service/versions/2", "", actor.String(), nil)
	require.Equal(t, http.StatusNotFound, foreign.Code, foreign.Body.String())
	builtin := call(http.MethodPost, "/orgs/"+org+"/resource-types/builtin/versions", "builtin-write", actor.String(), body)
	require.Equal(t, http.StatusNotFound, builtin.Code, builtin.Body.String())
	body.ExpectedResourceVersion = 2
	body.Reason = "  "
	invalid := call(http.MethodPost, path, "empty-reason", actor.String(), body)
	require.Equal(t, http.StatusBadRequest, invalid.Code, invalid.Body.String())
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = db.SetResourceTypeCatalogueStatus(t.Context(), tx, org, "service", "archived", actor, "Retire catalogue", 2)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	body.ExpectedResourceVersion, body.Reason = 3, "Publish while archived"
	archived := call(http.MethodPost, path, "archived-write", actor.String(), body)
	require.Equal(t, http.StatusConflict, archived.Code, archived.Body.String())
	versions, err := db.ListResourceTypeVersions(t.Context(), nil, org, "service", 0, 100)
	require.NoError(t, err)
	require.Len(t, versions, 2, "replays and rejected commands must not allocate versions")
}

func TestResourceTypeVersionsCatalogueKeepsHistoricalContract(t *testing.T) {
	db := pluginTestDatabase(t)
	grant := seedPluginOperation(t, db, "catalogue-contracts")
	server := &Server{Logger: zap.NewNop(), Database: db}
	definition, err := db.GetModuleDefinitionVersion(t.Context(), nil, grant.OrgID, "module", "1.0.0")
	require.NoError(t, err)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = db.PublishResourceTypeVersion(t.Context(), tx, grant.OrgID, "generic", model.ResourceTypeVersionPublication{Version: 2, ExpectedResourceVersion: 1, OutputSchema: map[string]any{"type": "object", "properties": map[string]any{"endpoint": map[string]any{"type": "string"}}}, ModuleContract: nil, Actor: grant.ActorID, Reason: "Introduce the endpoint interface"})
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	response, err := server.GenerateInternalModuleCatalogue(t.Context(), GenerateInternalModuleCatalogueRequestObject{
		OrgId: grant.OrgID, ProjectId: "project", EnvId: "development",
		Body: &InternalModuleCatalogueGenerateBody{AreRulesIgnored: true, PinnedModuleVersions: []string{"module@1.0.0"}},
	})
	require.NoError(t, err)
	catalogue, ok := response.(GenerateInternalModuleCatalogue200JSONResponse)
	require.True(t, ok, "unexpected catalogue response: %T", response)
	require.Len(t, catalogue.Modules, 1)
	selected := catalogue.Modules[0]
	require.NotNil(t, selected.ResourceTypeVersionUuid)
	require.Equal(t, definition.ResourceTypeVersionUUID, *selected.ResourceTypeVersionUuid)
	require.NotNil(t, selected.ResourceTypeVersion)
	require.Equal(t, int64(1), *selected.ResourceTypeVersion)
	require.NotNil(t, selected.OutputSchema)
	require.Equal(t, definition.OutputSchema, map[string]any(*selected.OutputSchema))
}
