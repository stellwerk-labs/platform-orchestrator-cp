//go:build integration

package api

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/hecho"
	dpapi "github.com/stellwerk-labs/platform-orchestrator-dp/shared/v3/genclient"
	iamapi "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/api/middleware"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/plugincontext"
)

// Real Core routes, generated OpenAPI validation, signed HTTP and PostgreSQL.
// The DP peer controls receipt acknowledgements, not infrastructure execution.
func TestPluginMissingDispatchRecovery(t *testing.T) {
	db, grant := pluginDeploymentBoundaryDatabase(t)
	var reads, submissions atomic.Int32
	call := pluginRecoveryHTTP(t, db, grant, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			submissions.Add(1)
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"error":"before_commit"}`))
			return
		}
		reads.Add(1)
		w.WriteHeader(404)
		_ = json.NewEncoder(w).Encode(dpapi.Error{Error: "dispatch_not_found"})
	})
	request := recoveryForwardRequest(grant, "missing-forward")
	reconcile := ReconcilePluginDeploymentJSONBody{GrantFence: grant.Revision, OriginalRequest: &request}
	path := "/deployments/" + request.RequestId + "/reconcile"
	response := call(path, ReconcilePluginDeploymentJSONBody{GrantFence: grant.Revision})
	require.Equal(t, 404, response.Code, response.Body.String())
	require.Zero(t, reads.Load(), "generic missing Core intent is not DP absence")
	wrong := request
	wrong.RequestId = "another-dispatch"
	response = call(path, ReconcilePluginDeploymentJSONBody{GrantFence: grant.Revision, OriginalRequest: &wrong})
	require.Equal(t, 409, response.Code, response.Body.String())
	unknown := map[string]any{"grant_fence": grant.Revision, "withdraw": false, "original_request": map[string]any{"grant_fence": request.GrantFence, "request_id": request.RequestId, "environment_uuid": request.EnvironmentUuid, "mode": "deploy", "plan": request.Plan, "unreviewed_authority": true}}
	response = call(path, unknown)
	require.Equal(t, 400, response.Code, "nested unknown fields must not silently disappear")
	for _, forgedTargets := range []map[string]uuid.UUID{
		{uuid.NewString(): uuid.New()},
		{grant.Intent.Modules[0].String(): uuid.New()},
	} {
		forged := recoveryForwardRequest(grant, request.RequestId)
		forged.Plan.ModuleVersions = forgedTargets
		response = call(path, ReconcilePluginDeploymentJSONBody{GrantFence: grant.Revision, OriginalRequest: &forged})
		require.Equal(t, 409, response.Code, "missing intent cannot manufacture target authority: %s", response.Body.String())
	}
	require.Zero(t, reads.Load())
	response = call(path, reconcile)
	require.Equal(t, 200, response.Code, response.Body.String())
	require.Zero(t, submissions.Load(), "reconcile records authority without submitting work")
	stored, err := model.GetExternalDispatch(t.Context(), db, grant.OrgID, grant.PluginID, grant.OperationID, request.RequestId)
	require.NoError(t, err)
	require.Equal(t, grant.Revision, stored.Fence)
	require.Equal(t, grant.Intent.TargetVersions, stored.TargetVersions)
	response = call("/deployments", request)
	require.Equal(t, 200, response.Code, response.Body.String())
	require.EqualValues(t, 1, submissions.Load(), "unchanged explicit Submit reuses the recorded identity")
	changed := request
	changed.Plan = &struct {
		Fingerprint    string               `json:"fingerprint"`
		ModuleVersions map[string]uuid.UUID `json:"module_versions"`
		SnapshotId     uuid.UUID            `json:"snapshot_id"`
	}{Fingerprint: "sha256:" + strings.Repeat("f", 64), ModuleVersions: request.Plan.ModuleVersions, SnapshotId: request.Plan.SnapshotId}
	response = call(path, ReconcilePluginDeploymentJSONBody{GrantFence: grant.Revision, OriginalRequest: &changed})
	require.Equal(t, 409, response.Code, response.Body.String())
	response = call(path, ReconcilePluginDeploymentJSONBody{GrantFence: grant.Revision, Withdraw: true, OriginalRequest: &request})
	require.Equal(t, 200, response.Code, response.Body.String())
	response = call("/deployments", request)
	require.Equal(t, 409, response.Code, "a settled identity cannot be admitted late")
	require.EqualValues(t, 1, submissions.Load())
}

func TestPluginMissingDispatchWithdrawalPreservesOriginalFence(t *testing.T) {
	db, grant := pluginDeploymentBoundaryDatabase(t)
	request := recoveryForwardRequest(grant, "withdraw-before-record")
	var reads atomic.Int32
	call := pluginRecoveryHTTP(t, db, grant, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method, "withdrawal must not submit work")
		reads.Add(1)
		stored, err := model.GetExternalDispatch(r.Context(), db, grant.OrgID, grant.PluginID, grant.OperationID, request.RequestId)
		assert.NoError(t, err)
		if stored != nil {
			check := *stored
			check.TargetVersions = nil
			assert.Error(t, model.CheckExternalDispatch(r.Context(), db, check), "barrier is non-admitting before DP lookup")
		}
		w.WriteHeader(404)
		_ = json.NewEncoder(w).Encode(dpapi.Error{Error: "dispatch_not_found"})
	})
	suspended := recoveryGrantCommand(t, db, grant, "suspend")
	path := "/deployments/" + request.RequestId + "/reconcile"
	response := call(path, ReconcilePluginDeploymentJSONBody{GrantFence: suspended.Revision, OriginalRequest: &request})
	require.Equal(t, 409, response.Code, "old fence must never be refreshed for active replay")
	response = call(path, ReconcilePluginDeploymentJSONBody{GrantFence: grant.Revision, Withdraw: true, OriginalRequest: &request})
	require.Equal(t, 409, response.Code, "current safety authorization is independently fenced")
	require.Zero(t, reads.Load())
	response = call(path, ReconcilePluginDeploymentJSONBody{GrantFence: suspended.Revision, Withdraw: true, OriginalRequest: &request})
	require.Equal(t, 200, response.Code, response.Body.String())
	stored, err := model.GetExternalDispatch(t.Context(), db, grant.OrgID, grant.PluginID, grant.OperationID, request.RequestId)
	require.NoError(t, err)
	require.Equal(t, request.GrantFence, stored.Fence, "original digest/fence survives safety recovery")
	resumed := recoveryGrantCommand(t, db, suspended, "resume")
	request.GrantFence = resumed.Revision
	response = call("/deployments", request)
	require.Equal(t, 409, response.Code, "resume cannot resurrect a withdrawn identity at a new fence")
}

func TestPluginFailedForwardRollbackAndRetainedRecovery(t *testing.T) {
	for _, outcome := range []string{"succeeded", "failed", "failed-incomplete", "running", "cancelled", "other-operation", "other-environment", "missing-base", "absent-base"} {
		t.Run(outcome, func(t *testing.T) {
			db, grant := pluginDeploymentBoundaryDatabase(t)
			forward := uuid.New()
			env := grant.Intent.Environments[0]
			now := time.Now().UTC()
			receipt := dpapi.ExternalDeploymentStatusResponse{DeploymentId: forward, OperationRef: grant.OperationID, EnvironmentUuid: env.EnvironmentID, ProjectUuid: env.ProjectID, Mode: dpapi.ExternalDeploymentStatusModeDeploy, Status: dpapi.ExternalDeploymentStatusSucceeded, CompletedAt: &now, RollbackModuleVersions: grant.Intent.TargetVersions}
			expect := 409
			switch outcome {
			case "succeeded":
				expect = 200
			case "failed", "failed-incomplete":
				receipt.Status = dpapi.ExternalDeploymentStatusFailed
				if outcome == "failed" {
					expect = 200
				} else {
					receipt.CompletedAt = nil
				}
			case "running":
				receipt.Status = dpapi.ExternalDeploymentStatusExecuting
			case "cancelled":
				receipt.Status = dpapi.ExternalDeploymentStatusResponseStatus("cancelled")
			case "other-operation":
				receipt.OperationRef = uuid.New()
			case "other-environment":
				receipt.EnvironmentUuid = uuid.New()
			case "missing-base":
				receipt.RollbackModuleVersions = nil
				expect = 502
			case "absent-base":
				expect = 502
			}
			var admissions, baseReads atomic.Int32
			call := pluginRecoveryHTTP(t, db, grant, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					admissions.Add(1)
					var admission dpapi.ExternalDeploymentAdmissionBody
					assert.NoError(t, json.NewDecoder(r.Body).Decode(&admission))
					assert.Equal(t, forward, admission.Rollback.ForwardDeploymentId)
					w.WriteHeader(503)
					_ = json.NewEncoder(w).Encode(dpapi.Error{Error: "before_commit"})
					return
				}
				if strings.Contains(r.URL.Path, forward.String()) {
					baseReads.Add(1)
					if outcome != "absent-base" {
						_ = json.NewEncoder(w).Encode(receipt)
						return
					}
				}
				w.WriteHeader(404)
				_ = json.NewEncoder(w).Encode(dpapi.Error{Error: "dispatch_not_found"})
			})
			request := PluginDeploymentSubmitRequest{EnvironmentUuid: env.EnvironmentID, GrantFence: grant.Revision, Mode: PluginDeploymentSubmitModeRollback, RequestId: "recover-rollback", Rollback: &struct {
				ForwardDeploymentId         uuid.UUID  `json:"forward_deployment_id"`
				RetryOfRollbackDeploymentId *uuid.UUID `json:"retry_of_rollback_deployment_id,omitempty"`
			}{ForwardDeploymentId: forward}}
			path := "/deployments/" + request.RequestId + "/reconcile"
			response := call(path, ReconcilePluginDeploymentJSONBody{GrantFence: grant.Revision, OriginalRequest: &request})
			require.Equal(t, expect, response.Code, response.Body.String())
			require.Zero(t, admissions.Load(), "recovery never submits rollback work")
			if expect != 200 {
				return
			}
			response = call("/deployments", request)
			require.Equal(t, 200, response.Code, response.Body.String())
			require.EqualValues(t, 1, admissions.Load(), "explicit Submit reuses the same rollback identity")
			// Historical status and base may become unavailable after the intent
			// was verified. Retained recovery must not refresh those bindings.
			receipt.OperationRef, receipt.RollbackModuleVersions = uuid.New(), nil
			response = call(path, ReconcilePluginDeploymentJSONBody{GrantFence: grant.Revision, Withdraw: true, OriginalRequest: &request})
			require.Equal(t, 200, response.Code, response.Body.String())
			require.EqualValues(t, 2, baseReads.Load(), "Submit rechecks eligibility; retained reconcile never refreshes history")
		})
	}
}

func TestPluginRecordedDispatchRecoveryAfterActorRotation(t *testing.T) {
	for _, withProof := range []bool{false, true} {
		for _, committed := range []bool{false, true} {
			t.Run(fmt.Sprintf("proof=%t/committed=%t", withProof, committed), func(t *testing.T) {
				h := newPluginOperationEntitlementHTTP(t)
				body := h.activation("resume")
				response := h.call(h.item.ServiceUserID, h.grant.OperationID, body)
				require.Equal(t, 200, response.Code, response.Body.String())
				var err error
				h.grant, err = model.GetExternalOperationGrant(t.Context(), h.f.db, "acme", h.item.ID, h.grant.OperationID)
				require.NoError(t, err)
				originalActor := h.grant.ActorID
				request := recoveryForwardRequest(h.grant, "rotated-operator-dispatch")
				deployment := uuid.New()
				var reads atomic.Int32
				dp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if r.Method == http.MethodPost {
						w.WriteHeader(503)
						_ = json.NewEncoder(w).Encode(dpapi.Error{Error: "before_commit"})
						return
					}
					reads.Add(1)
					stored, err := model.GetExternalDispatch(r.Context(), h.f.db, "acme", h.item.ID, h.grant.OperationID, request.RequestId)
					assert.NoError(t, err)
					if !committed {
						w.WriteHeader(404)
						_ = json.NewEncoder(w).Encode(dpapi.Error{Error: "dispatch_not_found"})
						return
					}
					now := time.Now().UTC()
					_ = json.NewEncoder(w).Encode(dpapi.ExternalDeploymentStatusResponse{DeploymentId: deployment, DispatchId: stored.DispatchID, OperationRef: stored.OperationID, Fence: stored.Fence, RequestDigest: stored.RequestDigest, ProjectUuid: stored.ProjectUUID, EnvironmentUuid: stored.EnvironmentUUID, Mode: dpapi.ExternalDeploymentStatusModeDeploy, Status: dpapi.ExternalDeploymentStatusFailed, CompletedAt: &now})
				}))
				t.Cleanup(dp.Close)
				h.f.server.PluginDataPlane, err = plugins.NewDataPlanePort(dp.URL, h.f.server.PluginGateway, true)
				require.NoError(t, err)
				response = h.deploymentCall(h.item.ServiceUserID, h.grant.OperationID, "/deployments", request)
				require.Equal(t, 200, response.Code, response.Body.String())
				h.secondActor = uuid.New()
				body = h.activation("rollback")
				body.Proof = h.actorProof("rollback", h.secondActor)
				response = h.call(h.item.ServiceUserID, h.grant.OperationID, body)
				require.Equal(t, 200, response.Code, response.Body.String())
				h.grant, err = model.GetExternalOperationGrant(t.Context(), h.f.db, "acme", h.item.ID, h.grant.OperationID)
				require.NoError(t, err)
				require.Equal(t, h.secondActor, h.grant.ActorID)
				h.f.clock.Store(h.f.base + 21)
				reconcile := ReconcilePluginDeploymentJSONBody{GrantFence: h.grant.Revision, Withdraw: true}
				if withProof {
					reconcile.OriginalRequest = &request
				}
				path := "/deployments/" + request.RequestId + "/reconcile"
				h.denied.Store(true)
				response = h.deploymentCall(h.item.ServiceUserID, h.grant.OperationID, path, reconcile)
				require.Equal(t, 403, response.Code, "fresh operator still needs current scoped IAM")
				require.Zero(t, reads.Load())
				h.denied.Store(false)
				response = h.deploymentCall(h.item.ServiceUserID, h.grant.OperationID, path, reconcile)
				require.Equal(t, 200, response.Code, response.Body.String())
				stored, err := model.GetExternalDispatch(t.Context(), h.f.db, "acme", h.item.ID, h.grant.OperationID, request.RequestId)
				require.NoError(t, err)
				require.Equal(t, originalActor, stored.ActorID, "receipt attribution never follows actor rotation")
				require.Equal(t, request.GrantFence, stored.Fence)
				forged := *stored
				forged.ActorID = uuid.New()
				require.Error(t, model.RecoverWithdrawnExternalDispatch(t.Context(), h.f.db, forged, h.grant.Revision), "arbitrary historical actor is not accepted")
				tx, err := h.f.db.BeginTx(t.Context(), nil)
				require.NoError(t, err)
				defer func() { _ = tx.Rollback() }()
				var raw []byte
				require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT completion FROM external_operation_dispatches WHERE org_id=$1 AND plugin_id=$2 AND operation_id=$3 AND dispatch_id=$4`, "acme", h.item.ID, h.grant.OperationID, request.RequestId).Scan(&raw))
				var completion model.ExternalDispatchCompletion
				require.NoError(t, json.Unmarshal(raw, &completion))
				require.Equal(t, !committed, completion.NoEffect)
				if committed {
					require.Equal(t, deployment, completion.DeploymentID, "committed receipt wins over withdrawal")
				}
			})
		}
	}
}

func recoveryForwardRequest(grant *model.ExternalOperationGrant, dispatch string) PluginDeploymentSubmitRequest {
	return PluginDeploymentSubmitRequest{EnvironmentUuid: grant.Intent.Environments[0].EnvironmentID, GrantFence: grant.Revision, Mode: PluginDeploymentSubmitModeDeploy, RequestId: dispatch, Plan: &struct {
		Fingerprint    string               `json:"fingerprint"`
		ModuleVersions map[string]uuid.UUID `json:"module_versions"`
		SnapshotId     uuid.UUID            `json:"snapshot_id"`
	}{Fingerprint: "sha256:" + grant.Intent.Fingerprint, ModuleVersions: grant.Intent.TargetVersions, SnapshotId: uuid.New()}}
}

func recoveryGrantCommand(t *testing.T, db model.Databaser, grant *model.ExternalOperationGrant, action string) *model.ExternalOperationGrant {
	t.Helper()
	commandAction := action
	if action == "resume" {
		commandAction = "activate"
	}
	updated, err := model.ApplyExternalGrantCommand(t.Context(), db, model.ExternalGrantCommand{OrgID: grant.OrgID, PluginID: grant.PluginID, OperationID: grant.OperationID, ActorID: grant.ActorID, SourceOperation: "prepare", SourceDigest: strings.Repeat("a", 64), RequestID: action, Action: commandAction, ExpectedRevision: grant.Revision, Reason: "Explicit recovery test"})
	require.NoError(t, err)
	return updated
}

func pluginRecoveryHTTP(t *testing.T, db model.Databaser, grant *model.ExternalOperationGrant, dpHandler http.HandlerFunc) func(string, any) *httptest.ResponseRecorder {
	t.Helper()
	service := userid.NewServiceUserTokenId().String()
	permission := "plugin." + grant.PluginID + ".control"
	item := plugins.Installation{ID: grant.PluginID, DisplayName: "Workflow", Version: "1.0.0", Backend: "https://workflow.test", ServiceUserID: service, AdmissionCheckerID: userid.NewServiceUserTokenId().String(), Organizations: map[string]plugins.AvailabilityState{grant.OrgID: plugins.StateAvailable}, AuthorizationPolicies: []plugins.AuthorizationPolicy{{Permission: permission, Scopes: []string{"environment"}}}}
	for _, action := range append([]string{"authorization.check"}, grant.Intent.Actions...) {
		item.ServiceGrants = append(item.ServiceGrants, plugins.ServiceGrant{Action: action, Permission: permission, Scope: "environment"})
	}
	registry, item := installedRegistry(t, db, item)
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	iam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body iamapi.InternalAuthorizeBody
		if assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
			assert.Equal(t, grant.ActorID, body.UserId)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(iam.Close)
	iamClient, err := iamapi.NewClientWithResponses(iam.URL)
	require.NoError(t, err)
	dp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			w.WriteHeader(500)
			return
		}
		action := pluginDeploymentReadAction
		if r.Method == http.MethodPost {
			action = "external-deployment.admit"
		}
		_, err = plugincontext.Verify(publicKey, r.Header.Get(plugincontext.Header), plugincontext.Request{Issuer: "cp", Audience: "platform-orchestrator-dp", PluginID: grant.PluginID, OrgID: grant.OrgID, OperationID: action, Method: r.Method, Path: r.URL.EscapedPath(), Body: body, IdempotencyKey: r.Header.Get("Idempotency-Key")}, time.Now())
		if !assert.NoError(t, err) {
			w.WriteHeader(401)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		w.Header().Set("Content-Type", "application/json")
		dpHandler(w, r)
	}))
	t.Cleanup(dp.Close)
	s := &Server{Logger: zap.NewNop(), Database: db, IamClient: iamClient, PluginGateway: &plugins.Gateway{Registry: registry, Issuer: "cp", SigningKey: privateKey}}
	s.PluginDataPlane, err = plugins.NewDataPlanePort(dp.URL, s.PluginGateway, true)
	require.NoError(t, err)
	e, err := hecho.DefaultEchoServerWithValidation(&hecho.ValidatedServerConfig{AppName: "plugin-recovery-test", Logger: zap.NewNop(), OpenAPIRawSchema: MustDecodeOpenApiSpec()})
	require.NoError(t, err)
	e.Use(middleware.EchoCtxMiddleware)
	s.MapRoutes(e)
	operationPath := "/orgs/" + grant.OrgID + "/plugins/" + grant.PluginID + "/operations/" + grant.OperationID.String()
	return func(suffix string, body any) *httptest.ResponseRecorder {
		payload, err := json.Marshal(body)
		require.NoError(t, err)
		request := httptest.NewRequest(http.MethodPost, operationPath+suffix, bytes.NewReader(payload))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("From", service)
		response := httptest.NewRecorder()
		e.ServeHTTP(response, request)
		return response
	}
}
