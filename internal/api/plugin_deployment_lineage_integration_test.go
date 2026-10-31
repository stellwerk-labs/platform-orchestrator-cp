//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	dpapi "github.com/stellwerk-labs/platform-orchestrator-dp/shared/v3/genclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/plugincontext"
)

// Real generated Core routes and PostgreSQL dispatches; the signed DP peer
// supplies metadata. Runner/head/lease proof belongs to DP integration tests.
func TestPluginExplicitFailedRollbackRetryLineage(t *testing.T) {
	for _, outcome := range []string{"valid", "valid-later-retry", "wrong-operation", "wrong-environment", "wrong-project", "wrong-id", "wrong-mode", "succeeded", "running", "no-completion", "wrong-first", "wrong-base", "wrong-admission-base", "wrong-forward", "missing-forward", "self-predecessor", "missing-anchor", "forward-missing-anchor", "forward-wrong-anchor", "missing-predecessor"} {
		t.Run(outcome, func(t *testing.T) {
			db, grant := pluginDeploymentBoundaryDatabase(t)
			env := grant.Intent.Environments[0]
			first, forwardID, baseID, retryID, other := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
			now := time.Now().UTC()
			baseVersion, err := db.GetCoreModuleVersion(t.Context(), nil, grant.OrgID, "module", "1.0.0", model.GetModeDefault)
			require.NoError(t, err)
			versions := map[string]uuid.UUID{grant.Intent.Modules[0].String(): baseVersion.UUID}
			forward := dpapi.ExternalDeploymentStatusResponse{DeploymentId: forwardID, OperationRef: grant.OperationID, EnvironmentUuid: env.EnvironmentID, ProjectUuid: env.ProjectID,
				Mode: dpapi.ExternalDeploymentStatusModeDeploy, Status: dpapi.ExternalDeploymentStatusSucceeded, CompletedAt: &now,
				FirstForwardDeploymentId: &first, RollbackBaseDeploymentId: &baseID, RollbackModuleVersions: versions}
			retry := forward
			retry.DeploymentId, retry.Mode, retry.Status, retry.AdmissionBaseDeploymentId = retryID, dpapi.ExternalDeploymentStatusModeRollback, dpapi.ExternalDeploymentStatusFailed, &forwardID
			retry.ForwardDeploymentId = &forwardID
			expected := 409
			switch outcome {
			case "valid":
				expected = 200
			case "valid-later-retry":
				expected = 200
				retry.RetryOfRollbackDeploymentId, retry.AdmissionBaseDeploymentId = &other, &other
			case "wrong-operation":
				retry.OperationRef = other
			case "wrong-environment":
				retry.EnvironmentUuid = other
			case "wrong-project":
				retry.ProjectUuid = other
			case "wrong-id":
				retry.DeploymentId = other
			case "wrong-mode":
				retry.Mode = dpapi.ExternalDeploymentStatusModeDeploy
			case "succeeded":
				retry.Status = dpapi.ExternalDeploymentStatusSucceeded
			case "running":
				retry.Status = dpapi.ExternalDeploymentStatusExecuting
			case "no-completion":
				retry.CompletedAt = nil
			case "wrong-first":
				retry.FirstForwardDeploymentId = &other
			case "wrong-base":
				retry.RollbackBaseDeploymentId = &other
			case "wrong-admission-base":
				retry.AdmissionBaseDeploymentId = &other
			case "wrong-forward":
				retry.ForwardDeploymentId = &other
			case "missing-forward":
				retry.ForwardDeploymentId = nil
			case "self-predecessor":
				retry.RetryOfRollbackDeploymentId, retry.AdmissionBaseDeploymentId = &retryID, &retryID
			case "missing-anchor":
				retry.FirstForwardDeploymentId, retry.RollbackBaseDeploymentId = nil, nil
			case "forward-missing-anchor":
				forward.FirstForwardDeploymentId, forward.RollbackBaseDeploymentId = nil, nil
			case "forward-wrong-anchor":
				forward.FirstForwardDeploymentId = &other
			case "missing-predecessor":
				expected = 200 // Typed downstream404 is carried by the Core envelope.
			}
			var submits, reads atomic.Int32
			var downstream []byte
			call := pluginRecoveryHTTP(t, db, grant, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					submits.Add(1)
					var admission dpapi.ExternalDeploymentAdmissionBody
					assert.NoError(t, json.NewDecoder(r.Body).Decode(&admission))
					assert.Equal(t, &first, admission.FirstForwardDeploymentId)
					assert.Equal(t, forwardID, admission.Rollback.ForwardDeploymentId)
					assert.Equal(t, &retryID, admission.Rollback.RetryOfRollbackDeploymentId)
					downstream, err = json.Marshal(admission)
					assert.NoError(t, err)
					w.WriteHeader(http.StatusServiceUnavailable)
					_ = json.NewEncoder(w).Encode(dpapi.Error{Error: "before_commit"})
					return
				}
				reads.Add(1)
				switch {
				case strings.Contains(r.URL.Path, forwardID.String()):
					_ = json.NewEncoder(w).Encode(forward)
				case strings.Contains(r.URL.Path, retryID.String()) && outcome != "missing-predecessor":
					_ = json.NewEncoder(w).Encode(retry)
				default:
					w.WriteHeader(http.StatusNotFound)
					_ = json.NewEncoder(w).Encode(dpapi.Error{Error: "dispatch_not_found"})
				}
			})
			body := map[string]any{"grant_fence": grant.Revision, "environment_uuid": env.EnvironmentID, "request_id": "explicit-rollback-retry", "mode": "rollback", "first_forward_deployment_id": first,
				"rollback": map[string]any{"forward_deployment_id": forwardID, "retry_of_rollback_deployment_id": retryID}}
			response := call("/deployments", body)
			require.Equal(t, expected, response.Code, response.Body.String())
			if !strings.HasPrefix(outcome, "valid") {
				require.Zero(t, submits.Load(), "invalid lineage must not dispatch")
				_, err := model.GetExternalDispatch(t.Context(), db, grant.OrgID, grant.PluginID, grant.OperationID, "explicit-rollback-retry")
				require.Error(t, err, "invalid lineage must not leave admitting intent")
				return
			}
			require.EqualValues(t, 1, submits.Load())
			stored, err := model.GetExternalDispatch(t.Context(), db, grant.OrgID, grant.PluginID, grant.OperationID, "explicit-rollback-retry")
			require.NoError(t, err)
			require.Equal(t, plugincontext.Digest(downstream), stored.RequestDigest)
			require.Equal(t, versions, stored.TargetVersions, "Core authorizes original base inventory, not retry admission inventory")
			var original PluginDeploymentSubmitRequest
			encoded, err := json.Marshal(body)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(encoded, &original))
			// A changed anchor or retry predecessor under the same retained key
			// cannot replace the recorded signed intent.
			for _, changeFirst := range []bool{true, false} {
				altered := original
				if changeFirst {
					altered.FirstForwardDeploymentId = &other
				} else {
					copyRollback := *original.Rollback
					copyRollback.RetryOfRollbackDeploymentId = &other
					altered.Rollback = &copyRollback
				}
				response = call("/deployments/explicit-rollback-retry/reconcile", ReconcilePluginDeploymentJSONBody{GrantFence: grant.Revision, OriginalRequest: &altered})
				require.Equal(t, 409, response.Code, response.Body.String())
			}
			baseReads := reads.Load()
			forward.FirstForwardDeploymentId, retry.FirstForwardDeploymentId = &other, &other
			response = call("/deployments/explicit-rollback-retry/reconcile", ReconcilePluginDeploymentJSONBody{GrantFence: grant.Revision, Withdraw: true, OriginalRequest: &original})
			require.Equal(t, 200, response.Code, response.Body.String())
			require.Equal(t, baseReads+1, reads.Load(), "retained withdrawal reads only dispatch receipt, never refreshes mutable lineage")
			require.EqualValues(t, 1, submits.Load(), "reconcile never creates another infrastructure attempt")
		})
	}
}

func TestPluginForwardLineageAssertionIsSignedAndImmutable(t *testing.T) {
	db, grant := pluginDeploymentBoundaryDatabase(t)
	first := uuid.New()
	request := recoveryForwardRequest(grant, "retry-forward-lineage")
	request.FirstForwardDeploymentId = &first
	var submitted []byte
	call := pluginRecoveryHTTP(t, db, grant, func(w http.ResponseWriter, r *http.Request) {
		if !assert.Equal(t, http.MethodPost, r.Method) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var body dpapi.ExternalDeploymentAdmissionBody
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.Equal(t, &first, body.FirstForwardDeploymentId)
		if assert.NotNil(t, body.Plan) {
			assert.Equal(t, request.Plan.SnapshotId, body.Plan.SnapshotId, "retry still carries its fresh admission snapshot")
		}
		var err error
		submitted, err = json.Marshal(body)
		assert.NoError(t, err)
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(dpapi.Error{Error: "before_commit"})
	})
	response := call("/deployments", request)
	require.Equal(t, 200, response.Code, response.Body.String())
	stored, err := model.GetExternalDispatch(t.Context(), db, grant.OrgID, grant.PluginID, grant.OperationID, request.RequestId)
	require.NoError(t, err)
	require.Equal(t, plugincontext.Digest(submitted), stored.RequestDigest)
	request.FirstForwardDeploymentId = nil
	response = call("/deployments/"+request.RequestId+"/reconcile", ReconcilePluginDeploymentJSONBody{GrantFence: grant.Revision, OriginalRequest: &request})
	require.Equal(t, 409, response.Code, "removing a recorded lineage assertion changes exact signed intent")
}
