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
	iam "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/api/middleware"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
)

func TestGetProjectScopedReadThroughStrictHTTP(t *testing.T) {
	db := pluginTestDatabase(t)
	actor, now := uuid.New(), time.Now().UTC()
	org, err := db.CreateOrg(t.Context(), nil, &model.Org{Id: "project-read-org", CreatedBy: actor, CreatedAt: now, Source: model.OrgSourceInternal, Plan: model.OrgPlanCustom})
	require.NoError(t, err)
	projects := map[string]*model.Project{}
	for _, id := range []string{"owned", "foreign"} {
		project, err := db.CreateProject(t.Context(), nil, &model.Project{OrgId: org.Id, OrgUuid: org.Uuid, Id: id, DisplayName: "Private project " + id, CreatedAt: now, UpdatedAt: now, Status: model.ProjectStatusActive})
		require.NoError(t, err)
		projects[id] = project
	}
	ownedResource := "project:" + projects["owned"].Uuid.String()
	foreignResource := "project:" + projects["foreign"].Uuid.String()
	orgResource := "organization:" + org.Id
	for _, tc := range []struct {
		name, target          string
		unauthenticated, list bool
		statuses              map[string]int
		want                  int
		checks                []string
	}{
		{name: "exact project scope", target: "owned", statuses: map[string]int{ownedResource: 204}, want: 200, checks: []string{ownedResource}},
		{name: "foreign project refused", target: "foreign", statuses: map[string]int{ownedResource: 204}, want: 403, checks: []string{foreignResource, orgResource}},
		{name: "organization compatibility fallback", target: "foreign", statuses: map[string]int{orgResource: 204}, want: 200, checks: []string{foreignResource, orgResource}},
		{name: "IAM failure refuses", target: "owned", statuses: map[string]int{ownedResource: 503, orgResource: 503}, want: 500, checks: []string{ownedResource, orgResource}},
		{name: "missing project is404 without marker fabrication", target: "missing", want: 404},
		{name: "unauthenticated remains401", target: "owned", unauthenticated: true, want: 401},
		{name: "project permission does not enumerate organization", list: true, statuses: map[string]int{ownedResource: 204}, want: 403, checks: []string{orgResource}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := make(chan string, 8)
			peer := projectReadIAMPeer(t, actor, tc.statuses, calls)
			defer peer.Close()
			iamClient, err := iam.NewClientWithResponses(peer.URL)
			require.NoError(t, err)
			server := &Server{Logger: zap.NewNop(), Database: db, IamClient: iamClient}
			echo, err := hecho.DefaultEchoServerWithValidation(&hecho.ValidatedServerConfig{AppName: "project-read-test", Logger: zap.NewNop(), OpenAPIRawSchema: MustDecodeOpenApiSpec()})
			require.NoError(t, err)
			echo.Use(middleware.EchoCtxMiddleware)
			server.MapRoutes(echo)
			path := "/orgs/" + org.Id + "/projects"
			if !tc.list {
				path += "/" + tc.target
			}
			request := httptest.NewRequest(http.MethodGet, path, nil)
			if !tc.unauthenticated {
				request.Header.Set("From", actor.String())
			}
			response := httptest.NewRecorder()
			echo.ServeHTTP(response, request)
			require.Equal(t, tc.want, response.Code, response.Body.String())
			var checks []string
			for len(calls) > 0 {
				checks = append(checks, <-calls)
			}
			assert.Equal(t, tc.checks, checks)
			if tc.want == 200 {
				var result Project
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
				require.Equal(t, projects[tc.target].Uuid, result.Uuid)
				require.Equal(t, tc.target, result.Id)
			} else {
				require.NotContains(t, response.Body.String(), "Private project")
			}
		})
	}
}

// This peer checks the actual generated IAM request; PostgreSQL and the strict Core route are real.
func projectReadIAMPeer(t *testing.T, actor uuid.UUID, statuses map[string]int, calls chan<- string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/internal/authorize", r.URL.Path)
		var body iam.InternalAuthorizeBody
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) || !assert.Len(t, body.Checks, 1) {
			w.WriteHeader(400)
			return
		}
		assert.Equal(t, actor, body.UserId)
		assert.Equal(t, "project_read", body.Checks[0].Permission)
		select {
		case calls <- body.Checks[0].Resource:
		default:
			t.Error("Unexpected repeated IAM requests")
			w.WriteHeader(500)
			return
		}
		status := statuses[body.Checks[0].Resource]
		if status == 0 {
			status = http.StatusForbidden
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusForbidden {
			_, err := w.Write([]byte(`{"error":"HTTP-403","message":"Forbidden","details":{}}`))
			assert.NoError(t, err)
		}
	}))
}
