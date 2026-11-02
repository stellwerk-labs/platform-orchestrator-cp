//go:build integration

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/hecho"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/api/middleware"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	observation "github.com/stellwerk-labs/platform-orchestrator-cp/internal/observationapi"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestObservationGrantMetadataSurvivesControllerLossAndRotation(t *testing.T) {
	db, baseline := pluginDeploymentBoundaryDatabase(t)
	now := time.Now().UTC()
	item := plugins.Installation{ID: "dev.example.observer", DisplayName: "Observer", Version: "1.0.0", Backend: "https://observer.example.test", Organizations: map[string]plugins.AvailabilityState{"acme": plugins.StateAvailable}, ServiceUserID: userid.NewServiceUserTokenId().String(), AdmissionCheckerID: userid.NewServiceUserTokenId().String(), ServiceGrants: []plugins.ServiceGrant{{Action: "observation.grant.get", Permission: "plugin.dev.example.observer.manage", Scope: "organization"}, {Action: "observation.try-start", Permission: "plugin.dev.example.observer.manage", Scope: "organization"}}}
	registry, item := installedRegistry(t, db, item)
	// No canonical Admin authority can be established after controller loss. Do
	// not invent a positive authorization peer merely to serve retained metadata.
	server := &Server{Database: db, Logger: zap.NewNop(), PluginGateway: &plugins.Gateway{Registry: registry}}
	router, err := hecho.DefaultEchoServerWithValidation(&hecho.ValidatedServerConfig{AppName: "observation-recovery", Logger: zap.NewNop(), OpenAPIRawSchema: MustDecodeOpenApiSpec()})
	require.NoError(t, err)
	router.Use(middleware.EchoCtxMiddleware)
	server.MapRoutes(router)
	call := func(method, path, actor string, body any) *httptest.ResponseRecorder {
		t.Helper()
		encoded, err := json.Marshal(body)
		require.NoError(t, err)
		request := httptest.NewRequest(method, path, bytes.NewReader(encoded))
		if body == nil {
			request = httptest.NewRequest(method, path, nil)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("From", actor)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		return response
	}
	command := model.ObservationGrantCommand{OrgID: "acme", PluginID: item.ID, GrantID: uuid.New(), ActorID: baseline.ActorID, SourceOperation: "configure", SourceDigest: strings.Repeat("a", 64), RequestID: uuid.NewString(), Action: "replace", Reason: "Configure explicit target", Targets: baseline.Intent.Environments}
	apply := func() *model.ObservationGrant {
		t.Helper()
		tx, err := db.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		grant, err := model.ApplyObservationGrantCommandTx(t.Context(), tx, command, now)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
		return grant
	}
	grant := apply()
	base := "/orgs/acme/plugins/" + item.ID + "/observations/v1"
	path := base + "/grants/" + grant.GrantID.String()
	read := func(wantRevision int64) {
		t.Helper()
		response := call(http.MethodGet, path, item.ServiceUserID, nil)
		require.Equal(t, 200, response.Code, response.Body.String())
		var metadata observation.ObservationGrant
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &metadata))
		require.Equal(t, wantRevision, metadata.Revision)
		require.Equal(t, command.ActorID, metadata.ActorId)
	}
	read(1)
	env, err := db.GetEnvironmentByUuid(t.Context(), nil, "acme", command.Targets[0].EnvironmentID, model.GetModeDefault)
	require.NoError(t, err)
	request := observation.ObservationRequest{GrantId: grant.GrantID, GrantRevision: 1, RequestId: uuid.New(), ProjectUuid: env.ProjectUuid, EnvironmentUuid: env.Uuid, ProjectId: env.ProjectId, EnvId: env.Id, Occurrence: now, AdmissionDeadline: now.Add(time.Minute)}
	response := call(http.MethodPost, base+"/runs", item.ServiceUserID, request)
	require.Equal(t, 503, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), "organization_admin_unavailable")
	command.ExpectedRevision = 1
	command.RequestID = uuid.NewString()
	command.ActorID = userid.NewHumanUserId()
	grant = apply()
	read(2)
	response = call(http.MethodPost, base+"/runs", item.ServiceUserID, request)
	require.Equal(t, 409, response.Code, response.Body.String())
	request.GrantRevision = grant.Revision
	response = call(http.MethodPost, base+"/runs", item.ServiceUserID, request)
	require.Equal(t, 503, response.Code, response.Body.String())
	for _, denied := range []struct {
		path, actor string
		status      int
	}{{path, userid.NewServiceUserTokenId().String(), 403}, {strings.Replace(path, "/orgs/acme/", "/orgs/foreign/", 1), item.ServiceUserID, 403}, {strings.Replace(path, item.ID, "dev.example.foreign", 1), item.ServiceUserID, 403}, {base + "/grants/" + uuid.NewString(), item.ServiceUserID, 404}} {
		response := call(http.MethodGet, denied.path, denied.actor, nil)
		require.Equal(t, denied.status, response.Code, response.Body.String())
	}
	command.ExpectedRevision = 2
	command.RequestID = uuid.NewString()
	command.Action = "revoke"
	command.Targets = nil
	grant = apply()
	read(3)
	request.GrantRevision = grant.Revision
	response = call(http.MethodPost, base+"/runs", item.ServiceUserID, request)
	require.Equal(t, 409, response.Code, response.Body.String())
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	var starts int
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT count(*) FROM environment_observation_requests WHERE org_id='acme' AND plugin_id=$1`, item.ID).Scan(&starts))
	require.Zero(t, starts)
	require.NoError(t, tx.Rollback())
}
