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
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
)

func TestModuleRuleHTTPAllowsFirstDraftWithoutPublishingADefault(t *testing.T) {
	db := pluginTestDatabase(t)
	grant := seedPluginOperation(t, db, "first-draft-rule")
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = db.CreateEmptyModule(t.Context(), tx, grant.OrgID, "first-implementation", "First implementation", "", "generic", map[string]string{})
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	server := &Server{Database: db, Logger: zap.NewNop()}
	e, err := hecho.DefaultEchoServerWithValidation(&hecho.ValidatedServerConfig{AppName: "first-draft-rule", Logger: zap.NewNop(), OpenAPIRawSchema: MustDecodeOpenApiSpec()})
	require.NoError(t, err)
	e.Use(middleware.EchoCtxMiddleware)
	server.MapRoutes(e)
	raw, err := json.Marshal(RuleCreateBody{ModuleId: "first-implementation", ProjectId: ref.Ref("project"), EnvId: ref.Ref("development")})
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, "/orgs/"+grant.OrgID+"/module-rules", bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("From", userid.InternalSystemUuid.String())
	result := httptest.NewRecorder()
	e.ServeHTTP(result, request)
	require.Equal(t, http.StatusCreated, result.Code, result.Body.String())
	globalRaw, err := json.Marshal(RuleCreateBody{ModuleId: "first-implementation"})
	require.NoError(t, err)
	global := httptest.NewRequest(http.MethodPost, "/orgs/"+grant.OrgID+"/module-rules", bytes.NewReader(globalRaw))
	global.Header.Set("Content-Type", "application/json")
	global.Header.Set("From", userid.InternalSystemUuid.String())
	denied := httptest.NewRecorder()
	e.ServeHTTP(denied, global)
	require.Equal(t, http.StatusBadRequest, denied.Code, denied.Body.String())
	require.Contains(t, denied.Body.String(), "Environment-scoped")
	normalRaw, err := json.Marshal(RuleCreateBody{ModuleId: "module"})
	require.NoError(t, err)
	normal := httptest.NewRequest(http.MethodPost, "/orgs/"+grant.OrgID+"/module-rules", bytes.NewReader(normalRaw))
	normal.Header.Set("Content-Type", "application/json")
	normal.Header.Set("From", userid.InternalSystemUuid.String())
	allowed := httptest.NewRecorder()
	e.ServeHTTP(allowed, normal)
	require.Equal(t, http.StatusCreated, allowed.Code, allowed.Body.String())

	catalogue, err := db.GetModuleCatalogue(t.Context(), nil, grant.OrgID, "first-implementation", model.GetModeDefault)
	require.NoError(t, err)
	require.Nil(t, catalogue.CurrentDefaultVersionUUID, "creating a rule must not manufacture a publication or Default")
	_, err = db.GetModuleDefinition(t.Context(), nil, grant.OrgID, "first-implementation", model.GetModeDefault)
	_, missing := model.IsErrNotFound(err)
	require.True(t, missing, "ordinary compilation still requires a publication or explicit test selection")
}
