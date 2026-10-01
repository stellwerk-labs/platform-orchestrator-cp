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

	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/hecho"
	dpapi "github.com/stellwerk-labs/platform-orchestrator-dp/shared/v2/genclient"
	iamapi "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/api/middleware"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/moduleversions"
)

func TestDeploymentAdmissionRequiresDataPlaneAndIndependentUserScope(t *testing.T) {
	db, grant := pluginDeploymentBoundaryDatabase(t)
	env := grant.Intent.Environments[0]
	service := userid.NewServiceUserTokenId()
	var denied, denyProposed atomic.Bool
	var proposedChecks atomic.Int64
	iam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body iamapi.InternalAuthorizeBody
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
			w.WriteHeader(500)
			return
		}
		assert.Equal(t, grant.ActorID, body.UserId)
		if !assert.Len(t, body.Checks, 1) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		switch body.Checks[0] {
		case environmentCheck(env.EnvironmentID, "deployment_write"):
		case environmentCheck(env.EnvironmentID, PermissionModuleVersionUseProposed):
			proposedChecks.Add(1)
		default:
			t.Errorf("unexpected admission authorization check: %#v", body.Checks)
		}
		if denied.Load() || denyProposed.Load() && body.Checks[0].Permission == PermissionModuleVersionUseProposed {
			w.WriteHeader(403)
		} else {
			w.WriteHeader(204)
		}
	}))
	t.Cleanup(iam.Close)
	client, err := iamapi.NewClientWithResponses(iam.URL)
	require.NoError(t, err)
	s := &Server{Logger: zap.NewNop(), Database: db, IamClient: client, DeploymentAdmissionIdentities: map[string]uuid.UUID{grant.OrgID: service}}
	e, err := hecho.DefaultEchoServerWithValidation(&hecho.ValidatedServerConfig{AppName: "ordinary-admission-test", Logger: zap.NewNop(), OpenAPIRawSchema: MustDecodeOpenApiSpec()})
	require.NoError(t, err)
	e.Use(middleware.EchoCtxMiddleware)
	s.MapRoutes(e)
	admission := uuid.New()
	path := "/orgs/" + grant.OrgID + "/deployment-admissions/" + admission.String()
	call := func(actor, suffix string, body any) *httptest.ResponseRecorder {
		payload, err := json.Marshal(body)
		require.NoError(t, err)
		request := httptest.NewRequest(http.MethodPost, path+suffix, bytes.NewReader(payload))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("From", actor)
		response := httptest.NewRecorder()
		e.ServeHTTP(response, request)
		return response
	}
	body := DeploymentAdmissionRequest{ActorId: grant.ActorID, ProjectUuid: env.ProjectID, EnvironmentUuid: env.EnvironmentID, RequestDigest: strings.Repeat("c", 64), Mode: AdmissionModeDeploy, ModuleVersions: grant.Intent.TargetVersions}
	for _, actor := range []string{grant.ActorID.String(), uuid.NewString(), "ffffffff-ffff-ffff-ffff-ffffffffffff"} {
		response := call(actor, "", body)
		require.Equal(t, 403, response.Code, response.Body.String())
	}
	denied.Store(true)
	response := call(service.String(), "", body)
	require.Equal(t, 403, response.Code, response.Body.String())
	denied.Store(false)
	denyProposed.Store(true)
	response = call(service.String(), "", body)
	require.Equal(t, 409, response.Code, "fresh Proposed use must fail closed without its current capability: "+response.Body.String())
	denyProposed.Store(false)
	response = call(service.String(), "", body)
	require.Equal(t, 200, response.Code, response.Body.String())
	require.Equal(t, int64(2), proposedChecks.Load())
	var receipt DeploymentAdmissionReceipt
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &receipt))
	require.Equal(t, admission, receipt.AdmissionId)
	require.Equal(t, AdmissionStateActive, receipt.State)
	require.Equal(t, int64(1), receipt.Epoch)
	denied.Store(true)
	settle := SettleDeploymentAdmissionJSONBody{ProjectUuid: env.ProjectID, EnvironmentUuid: env.EnvironmentID, RequestDigest: body.RequestDigest, Outcome: AdmissionOutcomeAbsent}
	response = call(grant.ActorID.String(), "/settle", settle)
	require.Equal(t, 403, response.Code)
	response = call(service.String(), "/settle", settle)
	require.Equal(t, 200, response.Code, "DP safety reconciliation must not need revoked actor authority: "+response.Body.String())
	admission = uuid.New()
	path = "/orgs/" + grant.OrgID + "/deployment-admissions/" + admission.String()
	carryForward := body.ModuleVersions
	carryBody := body
	carryBody.CarryForwardModuleVersions = &carryForward
	beforeCarry := proposedChecks.Load()
	denied.Store(false)
	response = call(service.String(), "", carryBody)
	require.Equal(t, 200, response.Code, "carried-forward Proposed versions must not require fresh-adoption authority: "+response.Body.String())
	require.Equal(t, beforeCarry, proposedChecks.Load())
	response = call(service.String(), "/seal-absence", carryBody)
	require.Equal(t, 200, response.Code, response.Body.String())
	denied.Store(false)
	response = call(service.String(), "", body)
	require.Equal(t, 409, response.Code, response.Body.String())
	for _, admitted := range []bool{false, true} {
		admission = uuid.New()
		path = "/orgs/" + grant.OrgID + "/deployment-admissions/" + admission.String()
		denied.Store(false)
		if admitted {
			response = call(service.String(), "", body)
			require.Equal(t, 200, response.Code, response.Body.String())
		}
		denied.Store(true)
		response = call(grant.ActorID.String(), "/seal-absence", body)
		require.Equal(t, 403, response.Code)
		response = call(service.String(), "/seal-absence", body)
		require.Equal(t, 200, response.Code, "durable absence must recover despite revoked actor permission: "+response.Body.String())
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &receipt))
		require.Equal(t, admission, receipt.AdmissionId)
		require.Equal(t, AdmissionStateSettled, receipt.State)
		require.NotNil(t, receipt.Outcome)
		require.Equal(t, "absent", *receipt.Outcome)
		altered := body
		altered.ActorId = uuid.New()
		response = call(service.String(), "/seal-absence", altered)
		require.Equal(t, 409, response.Code, response.Body.String())
		denied.Store(false)
		response = call(service.String(), "", body)
		require.Equal(t, 409, response.Code, response.Body.String())
	}
}

func TestDeploymentAdmissionDerivesCurrentLifecyclePermissions(t *testing.T) {
	db, grant := pluginDeploymentBoundaryDatabase(t)
	env := grant.Intent.Environments[0]
	s := &Server{Database: db}
	body := DeploymentAdmissionRequest{
		ActorId: grant.ActorID, ProjectUuid: env.ProjectID, EnvironmentUuid: env.EnvironmentID,
		RequestDigest: strings.Repeat("c", 64), Mode: AdmissionModeDeploy, ModuleVersions: grant.Intent.TargetVersions,
	}
	checks, freshProposed, err := s.deploymentAdmissionAuthorizationChecks(t.Context(), grant.OrgID, &body)
	require.NoError(t, err)
	require.Equal(t, []iamapi.ResourcePermissionCheck{environmentCheck(env.EnvironmentID, "deployment_write")}, checks)
	require.True(t, freshProposed)

	moduleUUID := grant.Intent.Modules[0]
	versionUUID := grant.Intent.TargetVersions[moduleUUID.String()]
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = db.CreateEnvironmentModuleVersionPin(t.Context(), tx, grant.OrgID, env.ProjectID, "project", env.EnvironmentID, "development", moduleUUID, versionUUID, grant.ActorID, "user", "Protect candidate", nil, false)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	version, err := db.GetCoreModuleVersion(t.Context(), nil, grant.OrgID, moduleUUID.String(), versionUUID.String(), model.GetModeDefault)
	require.NoError(t, err)
	tx, err = db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = db.TransitionCoreModuleVersion(t.Context(), tx, grant.OrgID, moduleUUID.String(), versionUUID.String(), moduleversions.LifecycleDefective, version.ResourceVersion, grant.ActorID, "Unsafe candidate", nil)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	carryForward := map[string]uuid.UUID{}
	for module, target := range body.ModuleVersions {
		if module != moduleUUID.String() {
			carryForward[module] = target
		}
	}
	body.CarryForwardModuleVersions = &carryForward
	confirmations := []uuid.UUID{versionUUID}
	body.ConfirmedRestrictedVersionUuids = &confirmations
	checks, freshProposed, err = s.deploymentAdmissionAuthorizationChecks(t.Context(), grant.OrgID, &body)
	require.NoError(t, err)
	require.ElementsMatch(t, []iamapi.ResourcePermissionCheck{
		environmentCheck(env.EnvironmentID, "deployment_write"),
		environmentCheck(env.EnvironmentID, PermissionModuleVersionPinDefective),
	}, checks)
	require.False(t, freshProposed)
	body.Mode = AdmissionModeRollback
	checks, freshProposed, err = s.deploymentAdmissionAuthorizationChecks(t.Context(), grant.OrgID, &body)
	require.NoError(t, err)
	require.ElementsMatch(t, []iamapi.ResourcePermissionCheck{
		environmentCheck(env.EnvironmentID, "deployment_write"),
		environmentCheck(env.EnvironmentID, PermissionModuleRollbackRestricted),
	}, checks)
	require.False(t, freshProposed)
}

func TestPinCreationRejectsDeploymentCompletedDuringRemoteEvidence(t *testing.T) {
	db, grant := pluginDeploymentBoundaryDatabase(t)
	env, module := grant.Intent.Environments[0], grant.Intent.Modules[0]
	base, err := db.GetCoreModuleVersion(t.Context(), nil, grant.OrgID, module.String(), grant.BaselineDefaults[module.String()].String(), model.GetModeDefault)
	require.NoError(t, err)
	iam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	t.Cleanup(iam.Close)
	iamClient, err := iamapi.NewClientWithResponses(iam.URL)
	require.NoError(t, err)
	var raced atomic.Bool
	dp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "last-deployments") {
			admission := model.DeploymentAdmission{OrgID: grant.OrgID, AdmissionID: uuid.New(), ProjectUUID: env.ProjectID, EnvironmentUUID: env.EnvironmentID, ActorID: grant.ActorID, RequestDigest: strings.Repeat("c", 64), Mode: "deploy", ModuleVersions: grant.Intent.TargetVersions}
			_, err := model.AuthorizeDeploymentAdmission(r.Context(), db, admission, model.DeploymentAdmissionPolicy{AllowFreshProposed: true})
			if !assert.NoError(t, err) {
				w.WriteHeader(500)
				return
			}
			_, err = model.SettleDeploymentAdmission(r.Context(), db, admission.OrgID, admission.AdmissionID, admission.ProjectUUID, admission.EnvironmentUUID, admission.RequestDigest, "succeeded")
			if !assert.NoError(t, err) {
				w.WriteHeader(500)
				return
			}
			raced.Store(true)
			_, _ = w.Write([]byte(`{"items":[{"status":"succeeded"}]}`))
		} else if strings.Contains(r.URL.Path, "active-resources") {
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{map[string]any{"module_id": base.ModuleSlug, "module_version": base.OpaqueVersionID}}})
		} else {
			t.Errorf("unexpected DP path: %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	t.Cleanup(dp.Close)
	dpClient, err := dpapi.NewClientWithResponses(dp.URL)
	require.NoError(t, err)
	s := &Server{Logger: zap.NewNop(), Database: db, IamClient: iamClient, DpClient: dpClient}
	e, err := hecho.DefaultEchoServerWithValidation(&hecho.ValidatedServerConfig{AppName: "pin-epoch-test", Logger: zap.NewNop(), OpenAPIRawSchema: MustDecodeOpenApiSpec()})
	require.NoError(t, err)
	e.Use(middleware.EchoCtxMiddleware)
	s.MapRoutes(e)
	body, err := json.Marshal(map[string]any{"project_uuid": env.ProjectID, "environment_uuid": env.EnvironmentID, "module_uuid": module, "version_uuid": base.UUID, "reason": "Protect observed baseline"})
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, "/orgs/"+grant.OrgID+"/module-version-pins", bytes.NewReader(body))
	request.Header.Set("From", grant.ActorID.String())
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "pin-racing-completed-admission")
	response := httptest.NewRecorder()
	e.ServeHTTP(response, request)
	require.True(t, raced.Load(), response.Body.String())
	require.Equal(t, 409, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), "changed while inspecting")
	pins, err := db.ListEnvironmentModuleVersionPins(t.Context(), nil, grant.OrgID, &env.EnvironmentID, nil, true)
	require.NoError(t, err)
	require.Empty(t, pins)
}
