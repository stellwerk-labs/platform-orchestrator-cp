//go:build integration

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/hecho"
	iamapi "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/api/middleware"
)

func TestDeploymentOutcomeAcknowledgementEndpointAuthorizesScopesAndForwards(t *testing.T) {
	db, grant := pluginDeploymentBoundaryDatabase(t)
	env := grant.Intent.Environments[0]
	actor, deploymentID := userid.NewHumanUserId(), uuid.New()
	var deny atomic.Bool
	var iamChecks atomic.Int32
	iam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		iamChecks.Add(1)
		var body iamapi.InternalAuthorizeBody
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		assert.Equal(t, actor, body.UserId)
		assert.Contains(t, []iamapi.ResourcePermissionCheck{
			environmentCheck(env.EnvironmentID, PermissionDeploymentOutcomeAcknowledge),
			orgCheck(grant.OrgID, PermissionDeploymentOutcomeAcknowledge),
		}, body.Checks[0], "only the acknowledgement permission, on the Environment or the organization fallback")
		if deny.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"HTTP-403","message":"denied","details":{}}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(iam.Close)
	iamClient, err := iamapi.NewClientWithResponses(iam.URL)
	require.NoError(t, err)
	s := &Server{Logger: zap.NewNop(), Database: db, IamClient: iamClient}
	e, err := hecho.DefaultEchoServerWithValidation(&hecho.ValidatedServerConfig{AppName: "outcome-acknowledgement-test", Logger: zap.NewNop(), OpenAPIRawSchema: MustDecodeOpenApiSpec()})
	require.NoError(t, err)
	e.Use(middleware.EchoCtxMiddleware)
	s.MapRoutes(e)
	call := func(envID, reason string, header map[string]string) *httptest.ResponseRecorder {
		payload, err := json.Marshal(map[string]string{"reason": reason})
		require.NoError(t, err)
		request := httptest.NewRequest(http.MethodPost, "/orgs/"+grant.OrgID+"/projects/project/envs/"+envID+"/deployments/"+deploymentID.String()+"/actions/acknowledge-outcome", bytes.NewReader(payload))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("From", actor.String())
		for key, value := range header {
			request.Header.Set(key, value)
		}
		response := httptest.NewRecorder()
		e.ServeHTTP(response, request)
		return response
	}
	ack := map[string]any{"deployment_id": deploymentID, "environment_id": env.EnvironmentID, "plugin_id": grant.PluginID,
		"operation_ref": grant.OperationID.String(), "actor_id": actor, "reason": "Runner job deleted", "acknowledged_at": time.Now().UTC(), "replayed": false}
	dataPlane := func(dp *outcomeDataPlane) *outcomeDataPlane {
		s.DpClient = dp.client(t, grant.OrgID, deploymentID)
		return dp
	}

	dp := dataPlane(&outcomeDataPlane{deploymentStatus: http.StatusOK, projectID: "project", envID: "development", ackStatus: http.StatusCreated, ackBody: ack})
	deny.Store(true)
	response := call("development", "Runner job deleted", nil)
	require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
	require.Zero(t, dp.acknowledgements.Load(), "an IAM denial never reaches the data plane")
	deny.Store(false)

	checks := iamChecks.Load()
	response = call("development", "Runner job deleted", map[string]string{"Po-Org-Id": grant.OrgID})
	require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
	require.Equal(t, checks, iamChecks.Load(), "the organization token is rejected before IAM")
	require.Zero(t, dp.acknowledgements.Load())

	require.Equal(t, http.StatusNotFound, call("missing", "Runner job deleted", nil).Code)
	require.Equal(t, http.StatusBadRequest, call("development", "   ", nil).Code)
	require.Equal(t, http.StatusBadRequest, call("development", strings.Repeat("r", 1001), nil).Code)
	require.Zero(t, dp.acknowledgements.Load())

	other := dataPlane(&outcomeDataPlane{deploymentStatus: http.StatusOK, projectID: "project", envID: "production", ackStatus: http.StatusCreated, ackBody: ack})
	require.Equal(t, http.StatusNotFound, call("development", "Runner job deleted", nil).Code, "a Deployment of another Environment")
	require.Zero(t, other.acknowledgements.Load())

	dp = dataPlane(&outcomeDataPlane{deploymentStatus: http.StatusOK, projectID: "project", envID: "development", ackStatus: http.StatusCreated, ackBody: ack})
	response = call("development", "  Runner job deleted  ", nil)
	require.Equal(t, http.StatusCreated, response.Code, response.Body.String())
	var created DeploymentOutcomeAcknowledgement
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &created))
	require.Equal(t, actor, created.ActorId)
	require.Equal(t, actor.String(), dp.actor.Load())
	require.Equal(t, "Runner job deleted", dp.reason.Load(), "the trimmed reason is forwarded")

	conflict := dataPlane(&outcomeDataPlane{deploymentStatus: http.StatusOK, projectID: "project", envID: "development", ackStatus: http.StatusConflict,
		ackBody: map[string]any{"error": "HTTP-409", "message": "another acknowledgement exists for this Deployment"}})
	response = call("development", "a different reason", nil)
	require.Equal(t, http.StatusConflict, response.Code)
	require.Contains(t, response.Body.String(), "another acknowledgement exists for this Deployment")
	require.Equal(t, int32(1), conflict.acknowledgements.Load())
}
