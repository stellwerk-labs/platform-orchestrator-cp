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
	dpapi "github.com/stellwerk-labs/platform-orchestrator-dp/shared/v3/genclient"
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
		if r.Method == http.MethodGet && r.URL.Path == "/platform-principals/"+service.String() {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(iamapi.PlatformPrincipal{Id: service, DisplayName: "Data Plane", Credential: iamapi.PlatformPrincipalCredentialStaticToken,
				GeneratedAt: time.Now(), GeneratedBy: uuid.New()})
			return
		}
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
	s := &Server{Logger: zap.NewNop(), Database: db, IamClient: client, DeploymentAdmissionIdentity: service}
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

// admissionCalls serves s and returns a call that asks Core, as caller, to
// admit a Deployment of grant's Environment under grant's organization.
func admissionCalls(t *testing.T, s *Server) func(grant *model.ExternalOperationGrant, caller uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	e, err := hecho.DefaultEchoServerWithValidation(&hecho.ValidatedServerConfig{AppName: "admission-identity-test", Logger: zap.NewNop(), OpenAPIRawSchema: MustDecodeOpenApiSpec()})
	require.NoError(t, err)
	e.Use(middleware.EchoCtxMiddleware)
	s.MapRoutes(e)
	return func(grant *model.ExternalOperationGrant, caller uuid.UUID) *httptest.ResponseRecorder {
		t.Helper()
		env := grant.Intent.Environments[0]
		payload, err := json.Marshal(DeploymentAdmissionRequest{ActorId: grant.ActorID, ProjectUuid: env.ProjectID, EnvironmentUuid: env.EnvironmentID,
			RequestDigest: strings.Repeat("c", 64), Mode: AdmissionModeDeploy, ModuleVersions: grant.Intent.TargetVersions})
		require.NoError(t, err)
		request := httptest.NewRequest(http.MethodPost, "/orgs/"+grant.OrgID+"/deployment-admissions/"+uuid.NewString(), bytes.NewReader(payload))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("From", caller.String())
		response := httptest.NewRecorder()
		e.ServeHTTP(response, request)
		return response
	}
}

// admissionGuards counts the admission guards Core holds for org.
func admissionGuards(t *testing.T, db model.Databaser, org string) int {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	var count int
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT count(*) FROM deployment_admission_guards WHERE org_id = $1`, org).Scan(&count))
	return count
}

func TestDeploymentAdmissionAcceptsOnlyAStaticPlatformPrincipal(t *testing.T) {
	db, grant := pluginDeploymentBoundaryDatabase(t)
	iam := operatorIAM(t)
	static, workload, organizationUser := userid.NewServiceUserTokenId(), userid.NewServiceUserTokenId(), userid.NewServiceUserTokenId()
	iam.createStaticPrincipal(static)
	iam.bind("dev.example.other", uuid.New(), workload)
	s := &Server{Logger: zap.NewNop(), Database: db, IamClient: iam.client}
	admit := admissionCalls(t, s)

	// Unset, Core admits for nobody and asks IAM about nobody.
	for _, caller := range []uuid.UUID{uuid.Nil, static} {
		response := admit(grant, caller)
		require.Equal(t, http.StatusForbidden, response.Code, "%s: %s", caller, response.Body.String())
		require.Contains(t, response.Body.String(), "The Data Plane's admission identity is required.", caller)
	}
	require.Zero(t, iam.lookups.Load())

	// The Data Plane holds one token for every organization: a service user
	// of one organization, or a plugin workload, is never the admission
	// authority, even when the operator configured it.
	for name, identity := range map[string]uuid.UUID{"a service user of an organization": organizationUser, "the platform principal of a plugin workload": workload} {
		s.DeploymentAdmissionIdentity = identity
		response := admit(grant, identity)
		require.Equal(t, http.StatusForbidden, response.Code, "%s: %s", name, response.Body.String())
		require.Contains(t, response.Body.String(), `"error":"deployment_admission_service_forbidden"`, name)
		require.Contains(t, response.Body.String(), "platform principal with a static token", name)
	}
	s.DeploymentAdmissionIdentity = static
	iam.principalsDown.Store(true)
	response := admit(grant, static)
	require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), `"error":"authorization_unavailable"`)
	iam.principalsDown.Store(false)
	require.Zero(t, admissionGuards(t, db, grant.OrgID), "a refused identity admits nothing")

	response = admit(grant, static)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Equal(t, 1, admissionGuards(t, db, grant.OrgID))
}

// Core names one admission identity for every organization, so an
// organization created while Core runs is admitted at once, without a
// configuration change or a restart. IAM vouches for the identity on every
// callback, so revoking it stops the next admission in every organization.
func TestOneAdmissionIdentityServesEveryOrganization(t *testing.T) {
	db, acme := pluginDeploymentBoundaryDatabase(t)
	iam := operatorIAM(t)
	dataPlane, otherStatic := userid.NewServiceUserTokenId(), userid.NewServiceUserTokenId()
	iam.createStaticPrincipal(dataPlane)
	iam.createStaticPrincipal(otherStatic)
	s := &Server{Logger: zap.NewNop(), Database: db, IamClient: iam.client, DeploymentAdmissionIdentity: dataPlane}
	admit := admissionCalls(t, s)

	response := admit(acme, dataPlane)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	beta := seedPluginOperation(t, db, "beta")
	lookups := iam.lookups.Load()
	response = admit(beta, dataPlane)
	require.Equal(t, http.StatusOK, response.Code, "an organization created after start-up: %s", response.Body.String())
	require.Equal(t, lookups+1, iam.lookups.Load(), "Core asks IAM about the identity on every callback")

	// IAM vouching for a static principal is not enough: Core admits only
	// as the identity it names, and refuses any other without asking IAM.
	response = admit(beta, otherStatic)
	require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), `"error":"deployment_admission_service_forbidden"`)
	require.Contains(t, response.Body.String(), "The Data Plane's admission identity is required.")
	require.Equal(t, lookups+1, iam.lookups.Load())

	iam.deletePrincipal(dataPlane)
	for _, grant := range []*model.ExternalOperationGrant{acme, beta} {
		response := admit(grant, dataPlane)
		require.Equal(t, http.StatusForbidden, response.Code, "%s: %s", grant.OrgID, response.Body.String())
		require.Contains(t, response.Body.String(), "platform principal with a static token", grant.OrgID)
	}
	require.Equal(t, lookups+3, iam.lookups.Load())
	require.Equal(t, 1, admissionGuards(t, db, acme.OrgID))
	require.Equal(t, 1, admissionGuards(t, db, beta.OrgID))
}
