package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/hecho"
	orchestratordp "github.com/stellwerk-labs/platform-orchestrator-dp/shared/v3/genclient"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// outcomeDataPlane is an in-process Data Plane HTTP peer that serves the two
// endpoints the acknowledgement uses, through the real generated client.
type outcomeDataPlane struct {
	deploymentStatus int
	projectID, envID string
	ackStatus        int
	ackBody          any
	acknowledgements atomic.Int32
	actor, reason    atomic.Value
}

func (d *outcomeDataPlane) server(t *testing.T, deploymentID uuid.UUID) *Server {
	t.Helper()
	return &Server{DpClient: d.client(t, "acme", deploymentID), Logger: zap.NewNop()}
}

func (d *outcomeDataPlane) client(t *testing.T, org string, deploymentID uuid.UUID) orchestratordp.ClientWithResponsesInterface {
	t.Helper()
	dp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/orgs/"+org+"/deployments/"+deploymentID.String():
			w.WriteHeader(d.deploymentStatus)
			if d.deploymentStatus == http.StatusOK {
				_ = json.NewEncoder(w).Encode(map[string]any{"id": deploymentID, "org_id": org, "project_id": d.projectID, "env_id": d.envID,
					"created_at": time.Now(), "created_by": uuid.New(), "mode": "deploy", "plan_only": false, "status": "failed",
					"status_message": "", "runner_id": "runner", "manifest": map[string]any{}, "metrics": map[string]any{}})
			} else {
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "HTTP-404", "message": "deployment not found"})
			}
		case r.Method == http.MethodPost && r.URL.Path == "/internal/orgs/"+org+"/deployments/"+deploymentID.String()+"/actions/acknowledge-outcome":
			d.acknowledgements.Add(1)
			d.actor.Store(r.Header.Get("From"))
			var body orchestratordp.DeploymentOutcomeAcknowledgementBody
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			d.reason.Store(body.Reason)
			w.WriteHeader(d.ackStatus)
			_ = json.NewEncoder(w).Encode(d.ackBody)
		default:
			t.Errorf("unexpected data plane request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(dp.Close)
	// The production client always sends the system user; the action must
	// replace it with the actor.
	client, err := orchestratordp.NewClientWithResponses(dp.URL, orchestratordp.WithRequestEditorFn(func(_ context.Context, r *http.Request) error {
		r.Header.Set("From", userid.InternalSystemUuid.String())
		return nil
	}))
	require.NoError(t, err)
	return client
}

func TestDeploymentOutcomeAcknowledgementForwardsActorAndMapsDataPlaneOutcomes(t *testing.T) {
	actor, deploymentID := userid.NewHumanUserId(), uuid.New()
	ack := func(replayed bool) map[string]any {
		return map[string]any{"deployment_id": deploymentID, "environment_id": uuid.New(), "plugin_id": "dev.example.workflow",
			"operation_ref": "operation", "actor_id": actor, "reason": "Runner job deleted", "acknowledged_at": time.Now().UTC(), "replayed": replayed}
	}
	conflict := map[string]any{"error": "outcome_window_open", "message": "the Runner dispatch window has not closed", "details": map[string]any{"deadline": "later"}}
	for name, tc := range map[string]struct {
		status int
		body   any
		assert func(t *testing.T, response AcknowledgeDeploymentOutcomeResponseObject)
	}{
		"recorded": {http.StatusCreated, ack(false), func(t *testing.T, response AcknowledgeDeploymentOutcomeResponseObject) {
			created, ok := response.(AcknowledgeDeploymentOutcome201JSONResponse)
			require.True(t, ok, "%T", response)
			require.Equal(t, actor, created.ActorId)
			require.False(t, created.Replayed)
		}},
		"replayed": {http.StatusOK, ack(true), func(t *testing.T, response AcknowledgeDeploymentOutcomeResponseObject) {
			replayed, ok := response.(AcknowledgeDeploymentOutcome200JSONResponse)
			require.True(t, ok, "%T", response)
			require.True(t, replayed.Replayed)
		}},
		"conflict passes through": {http.StatusConflict, conflict, func(t *testing.T, response AcknowledgeDeploymentOutcomeResponseObject) {
			rejected, ok := response.(AcknowledgeDeploymentOutcome409JSONResponse)
			require.True(t, ok, "%T", response)
			require.Equal(t, "outcome_window_open", rejected.Error)
			require.Equal(t, "the Runner dispatch window has not closed", rejected.Message)
			require.Equal(t, map[string]any{"deadline": "later"}, *rejected.Details)
		}},
		"data plane denial": {http.StatusForbidden, map[string]any{"error": "HTTP-403", "message": "missing deployment.outcome.acknowledge"}, func(t *testing.T, response AcknowledgeDeploymentOutcomeResponseObject) {
			denied, ok := response.(AcknowledgeDeploymentOutcome403JSONResponse)
			require.True(t, ok, "%T", response)
			require.Equal(t, "missing deployment.outcome.acknowledge", denied.Message)
		}},
		"data plane validation": {http.StatusBadRequest, map[string]any{"error": "HTTP-400", "message": "a reason of 1 to 1000 characters is required"}, func(t *testing.T, response AcknowledgeDeploymentOutcomeResponseObject) {
			_, ok := response.(AcknowledgeDeploymentOutcome400JSONResponse)
			require.True(t, ok, "%T", response)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			dp := &outcomeDataPlane{deploymentStatus: http.StatusOK, projectID: "project", envID: "development", ackStatus: tc.status, ackBody: tc.body}
			response, err := dp.server(t, deploymentID).forwardDeploymentOutcomeAcknowledgement(t.Context(), actor, "acme", "project", "development", deploymentID, "Runner job deleted")
			require.NoError(t, err)
			tc.assert(t, response)
			require.Equal(t, actor.String(), dp.actor.Load(), "the actor, not the system user, reaches the data plane")
			require.Equal(t, "Runner job deleted", dp.reason.Load())
		})
	}

	t.Run("an acknowledgement for another actor is rejected", func(t *testing.T) {
		foreign := ack(false)
		foreign["actor_id"] = userid.NewHumanUserId()
		dp := &outcomeDataPlane{deploymentStatus: http.StatusOK, projectID: "project", envID: "development", ackStatus: http.StatusCreated, ackBody: foreign}
		_, err := dp.server(t, deploymentID).forwardDeploymentOutcomeAcknowledgement(t.Context(), actor, "acme", "project", "development", deploymentID, "Runner job deleted")
		require.ErrorContains(t, err, "another actor")
	})
}

func TestDeploymentOutcomeAcknowledgementStaysInTheAddressedEnvironment(t *testing.T) {
	actor, deploymentID := userid.NewHumanUserId(), uuid.New()
	for name, dp := range map[string]*outcomeDataPlane{
		"other environment": {deploymentStatus: http.StatusOK, projectID: "project", envID: "production"},
		"other project":     {deploymentStatus: http.StatusOK, projectID: "other", envID: "development"},
		"unknown":           {deploymentStatus: http.StatusNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			response, err := dp.server(t, deploymentID).forwardDeploymentOutcomeAcknowledgement(t.Context(), actor, "acme", "project", "development", deploymentID, "Runner job deleted")
			require.NoError(t, err)
			missing, ok := response.(AcknowledgeDeploymentOutcome404JSONResponse)
			require.True(t, ok, "%T", response)
			require.Contains(t, missing.Message, "this Environment")
			require.Zero(t, dp.acknowledgements.Load(), "nothing is forwarded outside the addressed Environment")
		})
	}
}

func TestDeploymentOutcomeAcknowledgementRequiresAnAttributableUser(t *testing.T) {
	server := &Server{Logger: zap.NewNop()}
	request := AcknowledgeDeploymentOutcomeRequestObject{OrgId: "acme", ProjectId: "project", EnvId: "development", DeploymentId: uuid.New(),
		Body: &AcknowledgeDeploymentOutcomeJSONRequestBody{Reason: "Runner job deleted"}}
	ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, userid.InternalSystemUuid.String())
	response, err := server.AcknowledgeDeploymentOutcome(ctx, request)
	require.NoError(t, err)
	denied, ok := response.(AcknowledgeDeploymentOutcome403JSONResponse)
	require.True(t, ok, "%T", response)
	require.True(t, denied.IsAuthZFailureResponse(), "the denial counts as an authorization decision")
}
