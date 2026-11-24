//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/hecho"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/api/middleware"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	iam "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func environmentReadData(t *testing.T) (model.Databaser, *model.Project, []*model.Environment) {
	t.Helper()
	db := pluginTestDatabase(t)
	now := time.Now().UTC()
	org, err := db.CreateOrg(t.Context(), nil, &model.Org{Id: "scoped-env", CreatedBy: uuid.New(), CreatedAt: now, Source: model.OrgSourceInternal, Plan: model.OrgPlanCustom})
	require.NoError(t, err)
	project, err := db.CreateProject(t.Context(), nil, &model.Project{OrgId: org.Id, OrgUuid: org.Uuid, Id: "project", DisplayName: "Private project", Status: model.ProjectStatusActive, CreatedAt: now, UpdatedAt: now})
	require.NoError(t, err)
	kind, err := db.CreateEnvironmentType(t.Context(), nil, &model.EnvType{OrgId: org.Id, OrgUuid: org.Uuid, Id: "development", DisplayName: "Development"})
	require.NoError(t, err)
	envs := []*model.Environment{}
	for _, id := range []string{"owned", "sibling"} {
		env, err := db.CreateEnvironment(t.Context(), nil, &model.Environment{OrgId: org.Id, OrgUuid: org.Uuid, ProjectId: project.Id, ProjectUuid: project.Uuid, Id: id, EnvTypeId: kind.Id, EnvTypeUuid: kind.Uuid, CreatedAt: now, UpdatedAt: now, Status: model.EnvironmentStatusActive})
		require.NoError(t, err)
		envs = append(envs, env)
	}
	foreignProject, err := db.CreateProject(t.Context(), nil, &model.Project{OrgId: org.Id, OrgUuid: org.Uuid, Id: "other-project", DisplayName: "Foreign project", Status: model.ProjectStatusActive, CreatedAt: now, UpdatedAt: now})
	require.NoError(t, err)
	_, err = db.CreateEnvironment(t.Context(), nil, &model.Environment{OrgId: org.Id, OrgUuid: org.Uuid, ProjectId: foreignProject.Id, ProjectUuid: foreignProject.Uuid, Id: "owned", EnvTypeId: kind.Id, EnvTypeUuid: kind.Uuid, CreatedAt: now, UpdatedAt: now, Status: model.EnvironmentStatusActive})
	require.NoError(t, err)
	otherOrg, err := db.CreateOrg(t.Context(), nil, &model.Org{Id: "other", CreatedBy: uuid.New(), CreatedAt: now, Source: model.OrgSourceInternal, Plan: model.OrgPlanCustom})
	require.NoError(t, err)
	otherProject, err := db.CreateProject(t.Context(), nil, &model.Project{OrgId: otherOrg.Id, OrgUuid: otherOrg.Uuid, Id: project.Id, DisplayName: "Foreign organization", Status: model.ProjectStatusActive, CreatedAt: now, UpdatedAt: now})
	require.NoError(t, err)
	otherKind, err := db.CreateEnvironmentType(t.Context(), nil, &model.EnvType{OrgId: otherOrg.Id, OrgUuid: otherOrg.Uuid, Id: kind.Id, DisplayName: "Development"})
	require.NoError(t, err)
	_, err = db.CreateEnvironment(t.Context(), nil, &model.Environment{OrgId: otherOrg.Id, OrgUuid: otherOrg.Uuid, ProjectId: otherProject.Id, ProjectUuid: otherProject.Uuid, Id: "owned", EnvTypeId: otherKind.Id, EnvTypeUuid: otherKind.Uuid, CreatedAt: now, UpdatedAt: now, Status: model.EnvironmentStatusActive})
	require.NoError(t, err)
	return db, project, envs
}

func environmentReadPeer(t *testing.T, actor uuid.UUID, statuses map[string]int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/internal/authorize", r.URL.Path)
		var body iam.InternalAuthorizeBody
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) || !assert.Len(t, body.Checks, 1) {
			w.WriteHeader(400)
			return
		}
		assert.Equal(t, actor, body.UserId)
		assert.Equal(t, PermissionEnvironmentRead, body.Checks[0].Permission)
		status := statuses[body.Checks[0].Resource]
		if status == 0 {
			status = 403
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == 403 {
			_, err := w.Write([]byte(`{"error":"HTTP-403","message":"denied","details":{}}`))
			assert.NoError(t, err)
		}
	}))
}

func environmentReadHTTP(t *testing.T, db model.Databaser, actor uuid.UUID, path string, statuses map[string]int) *httptest.ResponseRecorder {
	t.Helper()
	peer := environmentReadPeer(t, actor, statuses)
	defer peer.Close()
	client, err := iam.NewClientWithResponses(peer.URL)
	require.NoError(t, err)
	server := &Server{Database: db, IamClient: client, Logger: zap.NewNop()}
	e, err := hecho.DefaultEchoServerWithValidation(&hecho.ValidatedServerConfig{AppName: "scoped-env-read", Logger: zap.NewNop(), OpenAPIRawSchema: MustDecodeOpenApiSpec()})
	require.NoError(t, err)
	e.Use(middleware.EchoCtxMiddleware)
	server.MapRoutes(e)
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Set("From", actor.String())
	response := httptest.NewRecorder()
	e.ServeHTTP(response, request)
	return response
}

func TestEnvironmentReadScopedStrictHTTP(t *testing.T) {
	db, project, envs := environmentReadData(t)
	root := "/orgs/scoped-env/projects/project/envs"
	envScope := "env:" + envs[0].Uuid.String()
	projectScope := "project:" + project.Uuid.String()
	orgScope := "organization:scoped-env"
	for _, actor := range []uuid.UUID{uuid.New(), userid.NewServiceUserTokenId()} {
		for _, trial := range []struct {
			name, path string
			grants     map[string]int
			want       int
		}{
			{"exact Environment", root + "/owned", map[string]int{envScope: 204}, 200},
			{"project children", root, map[string]int{projectScope: 204}, 200},
			{"Environment cannot list siblings", root, map[string]int{envScope: 204}, 403},
			{"sibling denied", root + "/sibling", map[string]int{envScope: 204}, 403},
			{"organization fallback", root + "/owned", map[string]int{orgScope: 204}, 200},
			{"organization enumeration remains organization-only", "/orgs/scoped-env/envs", map[string]int{envScope: 204, projectScope: 204}, 403},
			{"missing Environment", root + "/missing", nil, 404},
			{"foreign organization", "/orgs/other/projects/project/envs/owned", map[string]int{envScope: 204}, 403},
			{"foreign project", "/orgs/scoped-env/projects/other-project/envs/owned", map[string]int{envScope: 204}, 403},
			{"missing organization", "/orgs/missing/projects/project/envs/owned", nil, 404},
			{"missing project", "/orgs/scoped-env/projects/missing/envs", nil, 404},
			{"IAM unavailable", root + "/owned", map[string]int{envScope: 503, orgScope: 503}, 500},
		} {
			t.Run(trial.name+actor.String(), func(t *testing.T) {
				response := environmentReadHTTP(t, db, actor, trial.path, trial.grants)
				require.Equal(t, trial.want, response.Code, response.Body.String())
				if trial.want != 200 {
					require.NotContains(t, response.Body.String(), envs[0].Uuid.String())
					require.NotContains(t, response.Body.String(), "Private project")
				}
			})
		}
	}
}

func TestEnvironmentReadScopedPaginationAndStorageFailure(t *testing.T) {
	db, project, _ := environmentReadData(t)
	actor := uuid.New()
	root := "/orgs/scoped-env/projects/project/envs"
	grants := map[string]int{"project:" + project.Uuid.String(): 204}
	response := environmentReadHTTP(t, db, actor, root+"?per_page=1", grants)
	require.Equal(t, 200, response.Code, response.Body.String())
	var page ListEnvironments200JSONResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &page))
	require.Len(t, page.Items, 1)
	require.NotNil(t, page.NextPageToken)
	next := environmentReadHTTP(t, db, actor, root+"?per_page=1&page="+*page.NextPageToken, grants)
	require.Equal(t, 200, next.Code, next.Body.String())
	var second ListEnvironments200JSONResponse
	require.NoError(t, json.Unmarshal(next.Body.Bytes(), &second))
	require.Len(t, second.Items, 1)
	require.NotEqual(t, page.Items[0].Uuid, second.Items[0].Uuid)
	require.NoError(t, db.Close())
	failed := environmentReadHTTP(t, db, actor, root+"/owned", grants)
	require.Equal(t, 500, failed.Code, failed.Body.String())
}

func TestEnvironmentReadMalformedIAMForbiddens(t *testing.T) {
	db, _, _ := environmentReadData(t)
	actor := uuid.New()
	server := &Server{Database: db, Logger: zap.NewNop()}
	e, err := hecho.DefaultEchoServerWithValidation(&hecho.ValidatedServerConfig{AppName: "malformed-IAM", Logger: zap.NewNop(), OpenAPIRawSchema: MustDecodeOpenApiSpec()})
	require.NoError(t, err)
	e.Use(middleware.EchoCtxMiddleware)
	server.MapRoutes(e)
	for _, trial := range []struct {
		name, media, body string
		want              int
	}{
		{"empty", "text/plain", "", 403},
		{"proxy HTML", "text/html", "<html>private-IAM-canary</html>", 403},
		{"missing details", "application/json", `{"error":"private-IAM-canary","message":"private-IAM-canary"}`, 403},
		{"null", "application/json", `null`, 403},
		{"valid failed checks", "application/json", `{"error":"ignored","details":{"failed_checks":[{"permission":"environment_read","resource":"env:denied"}]}}`, 403},
		{"invalid JSON", "application/json", `<private-IAM-canary>`, 500},
	} {
		t.Run(trial.name, func(t *testing.T) {
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body iam.InternalAuthorizeBody
				assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				assert.Equal(t, actor, body.UserId)
				w.Header().Set("Content-Type", trial.media)
				w.WriteHeader(403)
				_, err := w.Write([]byte(trial.body))
				assert.NoError(t, err)
			}))
			defer peer.Close()
			client, err := iam.NewClientWithResponses(peer.URL)
			require.NoError(t, err)
			server.IamClient = client
			request := httptest.NewRequest(http.MethodGet, "/orgs/scoped-env/projects/project/envs/owned", nil)
			request.Header.Set("From", actor.String())
			response := httptest.NewRecorder()
			e.ServeHTTP(response, request)
			require.Equal(t, trial.want, response.Code, response.Body.String())
			require.NotContains(t, response.Body.String(), "private-IAM-canary")
			if trial.name == "valid failed checks" {
				require.Contains(t, response.Body.String(), "failed_checks")
				require.Contains(t, response.Body.String(), "environment_read")
			}
		})
	}
}
