//go:build integration

package api

import (
	"bytes"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/pgtest"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v2/plugincontext"
)

// This integration exercises the real Core HTTP routes, generated contracts,
// signature boundary and PostgreSQL. The controlled IAM/DP peers test failure
// and acknowledgement boundaries, not Runner execution or infrastructure.
func TestPluginDeploymentBoundaryApprovalAndNoEffectRecovery(t *testing.T) {
	db, grant := pluginDeploymentBoundaryDatabase(t)
	env := grant.Intent.Environments[0]
	service, checker, approver := userid.NewServiceUserTokenId().String(), userid.NewServiceUserTokenId().String(), uuid.New()
	permission := "plugin." + grant.PluginID + ".control"
	approvalPermission := "plugin." + grant.PluginID + ".approve"
	item := plugins.Installation{ID: grant.PluginID, DisplayName: "Workflow", Version: "1.0.0", Backend: "https://workflow.test", ServiceUserID: service, AdmissionCheckerID: checker, Organizations: map[string]plugins.AvailabilityState{grant.OrgID: plugins.StateAvailable}, AuthorizationPolicies: []plugins.AuthorizationPolicy{{Permission: permission, Scopes: []string{"environment"}}, {Permission: approvalPermission, Scopes: []string{"environment", "project"}}}, DeploymentApprovalPolicies: map[string]string{"environment": approvalPermission, "project": approvalPermission}}
	for _, action := range append([]string{"authorization.check"}, grant.Intent.Actions...) {
		item.ServiceGrants = append(item.ServiceGrants, plugins.ServiceGrant{Action: action, Permission: permission, Scope: "environment"})
	}
	registry, item := installedRegistry(t, db, item)
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	var denyApprover, denyAtCallback, noEffect, temporaryApprover, denyController atomic.Bool
	var admissions, approvalChecks atomic.Int32
	denyApprover.Store(true)
	iam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body iamapi.InternalAuthorizeBody
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
			w.WriteHeader(500)
			return
		}
		if body.UserId == approver {
			approvalChecks.Add(1)
			assert.Equal(t, []iamapi.ResourcePermissionCheck{projectCheck(env.ProjectID, approvalPermission)}, body.Checks)
			if temporaryApprover.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			if denyApprover.Load() {
				w.WriteHeader(http.StatusForbidden)
				return
			}
		} else {
			assert.Equal(t, grant.ActorID, body.UserId)
			if denyController.Load() {
				w.WriteHeader(http.StatusForbidden)
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(iam.Close)
	iamClient, err := iamapi.NewClientWithResponses(iam.URL)
	require.NoError(t, err)
	s := &Server{Logger: zap.NewNop(), Database: db, IamClient: iamClient, PluginGateway: &plugins.Gateway{Registry: registry, Issuer: "cp", SigningKey: privateKey}}
	e, err := hecho.DefaultEchoServerWithValidation(&hecho.ValidatedServerConfig{AppName: "plugin-boundary-test", Logger: zap.NewNop(), OpenAPIRawSchema: MustDecodeOpenApiSpec()})
	require.NoError(t, err)
	e.Use(middleware.EchoCtxMiddleware)
	s.MapRoutes(e)
	operationPath := "/orgs/" + grant.OrgID + "/plugins/" + grant.PluginID + "/operations/" + grant.OperationID.String()
	call := func(actor, suffix string, body any) *httptest.ResponseRecorder {
		payload, marshalErr := json.Marshal(body)
		require.NoError(t, marshalErr)
		request := httptest.NewRequest(http.MethodPost, operationPath+suffix, bytes.NewReader(payload))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("From", actor)
		response := httptest.NewRecorder()
		e.ServeHTTP(response, request)
		return response
	}
	dp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, readErr := io.ReadAll(r.Body)
		if !assert.NoError(t, readErr) {
			w.WriteHeader(500)
			return
		}
		action := "external-deployment.admit"
		if r.Method == http.MethodGet {
			action = pluginDeploymentReadAction
		}
		_, verifyErr := plugincontext.Verify(publicKey, r.Header.Get(plugincontext.Header), plugincontext.Request{Issuer: "cp", Audience: "platform-orchestrator-dp", PluginID: grant.PluginID, OrgID: grant.OrgID, OperationID: action, Method: r.Method, Path: r.URL.EscapedPath(), Body: body, IdempotencyKey: r.Header.Get("Idempotency-Key")}, time.Now())
		if !assert.NoError(t, verifyErr) {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			w.WriteHeader(404)
			_ = json.NewEncoder(w).Encode(dpapi.Error{Error: "dispatch_not_found", Message: "No committed dispatch"})
			return
		}
		admissions.Add(1)
		var admission dpapi.ExternalDeploymentAdmissionBody
		if !assert.NoError(t, json.Unmarshal(body, &admission)) {
			w.WriteHeader(400)
			return
		}
		assert.Equal(t, grant.OperationID, admission.OperationRef)
		assert.Equal(t, grant.Intent.TargetVersions, admission.Plan.ExpectedModuleVersions)
		assert.NotContains(t, string(body), "approved_pin")
		assert.NotContains(t, string(body), "request_id")
		if noEffect.Load() {
			w.WriteHeader(503)
			_ = json.NewEncoder(w).Encode(dpapi.Error{Error: "runner_unavailable", Message: "No admission"})
			return
		}
		if denyAtCallback.Load() {
			denyApprover.Store(true)
		}
		response := call(checker, "/check", PluginDispatchCheck{Action: ExternalDeploymentAdmit, Fence: grant.Revision, DispatchId: r.Header.Get("Idempotency-Key"), ProjectUuid: env.ProjectID, EnvironmentUuid: env.EnvironmentID, RequestDigest: plugincontext.Digest(body)})
		w.WriteHeader(response.Code)
		_, _ = w.Write(response.Body.Bytes())
	}))
	t.Cleanup(dp.Close)
	s.PluginDataPlane, err = plugins.NewDataPlanePort(dp.URL, s.PluginGateway, true)
	require.NoError(t, err)
	pinBoundaries := map[string]uuid.UUID{}
	for module := range grant.Intent.TargetVersions {
		pinBoundaries[module] = uuid.Nil
	}
	submit := map[string]any{"grant_fence": grant.Revision, "request_id": "dispatch-1", "environment_uuid": env.EnvironmentID, "mode": "deploy", "plan": map[string]any{"snapshot_id": uuid.New(), "fingerprint": "sha256:" + strings.Repeat("c", 64), "module_versions": grant.Intent.TargetVersions}, "approval": map[string]any{"actor_id": approver, "scope": "project"}, "approved_pin_boundaries": pinBoundaries}
	temporaryApprover.Store(true)
	response := call(service, "/deployments", submit)
	require.Equal(t, 503, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), `"error":"authorization_unavailable"`)
	require.Zero(t, admissions.Load())
	temporaryApprover.Store(false)
	denyController.Store(true)
	response = call(service, "/deployments", submit)
	require.Equal(t, 403, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), `"error":"plugin_actor_forbidden"`)
	require.Zero(t, admissions.Load())
	denyController.Store(false)
	response = call(service, "/deployments", submit)
	require.Equal(t, 403, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), `"error":"plugin_approval_forbidden"`)
	require.Zero(t, admissions.Load(), "revoked approval must stop before dispatch")
	denyApprover.Store(false)
	denyAtCallback.Store(true)
	response = call(service, "/deployments", submit)
	require.Equal(t, 200, response.Code, response.Body.String())
	var upstream PluginInvocationResult
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &upstream))
	require.Equal(t, 403, upstream.Status, "approval revoked between recording and admission must still fail")
	var racedDenial Error
	require.NoError(t, json.Unmarshal(upstream.Body, &racedDenial))
	require.Equal(t, "plugin_approval_forbidden", racedDenial.Error)
	require.GreaterOrEqual(t, approvalChecks.Load(), int32(3))
	recorded, err := model.GetExternalDispatch(t.Context(), db, grant.OrgID, grant.PluginID, grant.OperationID, "dispatch-1")
	require.NoError(t, err)
	require.Equal(t, approver, recorded.Approval.ActorID)
	// No-effect recovery is allowed with the current controller, but it never
	// replays execution and cannot depend on a now-revoked approver.
	checksBeforeWithdrawal := approvalChecks.Load()
	response = call(service, "/deployments/dispatch-1/reconcile", ReconcilePluginDeploymentJSONBody{GrantFence: grant.Revision, Withdraw: true})
	require.Equal(t, 200, response.Code, response.Body.String())
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &upstream))
	require.Equal(t, 404, upstream.Status)
	require.Equal(t, checksBeforeWithdrawal, approvalChecks.Load(), "withdrawal must not reauthorize the old approver")
	check := *recorded
	check.Approval, check.TargetVersions, check.ApprovedPins = nil, nil, nil
	require.ErrorContains(t, model.CheckExternalDispatch(t.Context(), db, check), "settled")
	// A new attempt is possible only with a distinct dispatch and fresh current
	// approval. The previous identity remains permanently reserved.
	denyApprover.Store(false)
	denyAtCallback.Store(false)
	noEffect.Store(true)
	submit["request_id"] = "dispatch-2"
	response = call(service, "/deployments", submit)
	require.Equal(t, 200, response.Code, response.Body.String())
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &upstream))
	require.Equal(t, 503, upstream.Status)
	response = call(service, "/deployments/dispatch-2/reconcile", ReconcilePluginDeploymentJSONBody{GrantFence: grant.Revision, Withdraw: false})
	require.Equal(t, 200, response.Code, response.Body.String())
	second, err := model.GetExternalDispatch(t.Context(), db, grant.OrgID, grant.PluginID, grant.OperationID, "dispatch-2")
	require.NoError(t, err)
	second.Approval, second.TargetVersions, second.ApprovedPins = nil, nil, nil
	require.NoError(t, model.CheckExternalDispatch(t.Context(), db, *second), "absence alone must not withdraw or settle authority")
	response = call(checker, "/deployments", submit)
	require.Equal(t, 403, response.Code, "admission-checker credentials must not become plugin credentials")
}

func pluginDeploymentBoundaryDatabase(t *testing.T, extraActions ...string) (model.Databaser, *model.ExternalOperationGrant) {
	t.Helper()
	dsn := os.Getenv("PLUGIN_TEST_DATABASE_URL")
	require.NotEmpty(t, dsn)
	admin, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	schema := "plugin_api_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = admin.Exec(`CREATE SCHEMA ` + schema)
	require.NoError(t, err)
	db, err := model.NewDatabaser(t.Context(), zap.NewNop(), pgtest.WithSearchPath(dsn, schema),
		model.WithExtensionResourceJournal())
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
		_, err := admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		require.NoError(t, err)
		require.NoError(t, admin.Close())
	})
	ctx, actor, now := t.Context(), uuid.New(), time.Now().UTC()
	org, err := db.CreateOrg(ctx, nil, &model.Org{Id: "acme", CreatedBy: actor, CreatedAt: now, Source: model.OrgSourceInternal, Plan: model.OrgPlanCustom})
	require.NoError(t, err)
	project, err := db.CreateProject(ctx, nil, &model.Project{OrgId: org.Id, OrgUuid: org.Uuid, Id: "project", DisplayName: "Project", CreatedAt: now, UpdatedAt: now, Status: model.ProjectStatusActive})
	require.NoError(t, err)
	envType, err := db.CreateEnvironmentType(ctx, nil, &model.EnvType{OrgId: org.Id, OrgUuid: org.Uuid, Id: "development", DisplayName: "Development"})
	require.NoError(t, err)
	env, err := db.CreateEnvironment(ctx, nil, &model.Environment{OrgId: org.Id, OrgUuid: org.Uuid, ProjectId: project.Id, ProjectUuid: project.Uuid, Id: "development", EnvTypeId: envType.Id, EnvTypeUuid: envType.Uuid, CreatedAt: now, UpdatedAt: now, Status: model.EnvironmentStatusActive})
	require.NoError(t, err)
	shape := map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}}
	_, err = db.CreateResourceType(ctx, nil, &model.ResourceType{OrgId: opt.Of(org.Id), Id: "generic", OutputsSchema: shape})
	require.NoError(t, err)
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	module, err := db.CreateEmptyModule(ctx, tx, org.Id, "module", "Module", "", "generic", map[string]string{})
	require.NoError(t, err)
	publish := func(semver string) *model.CoreModuleVersion {
		version, err := db.PublishCoreModuleVersion(ctx, tx, &model.ModuleDefinitionVersion{OrgId: org.Id, DefinitionId: "module", SemanticVersion: semver, ModuleSource: "inline", ModuleSourceCode: opt.Of(`output "name" { value = "test" }`), OutputSchema: shape, ModuleInputs: map[string]any{}, ModuleParams: map[string]model.ModuleParam{}, ProviderMapping: map[string]string{}, Dependencies: map[string]model.ModuleDefinitionDependency{}, CoProvisioned: []model.ModuleDefinitionCoProvision{}}, actor)
		require.NoError(t, err)
		return version
	}
	base := publish("1.0.0")
	_, err = db.TransitionCoreModuleVersion(ctx, tx, org.Id, "module", base.UUID.String(), moduleversions.LifecycleDefault, base.ResourceVersion, actor, "Baseline", nil)
	require.NoError(t, err)
	version := publish("1.1.0")
	require.NoError(t, tx.Commit())
	command := model.ExternalGrantCommand{OrgID: org.Id, PluginID: "dev.example.workflow", OperationID: uuid.New(), ActorID: actor, SourceOperation: "prepare", SourceDigest: strings.Repeat("a", 64), RequestID: "prepare", Action: "prepare", Reason: "Prepare", Intent: &model.ExternalOperationIntent{Fingerprint: strings.Repeat("b", 64), Modules: []uuid.UUID{module.UUID}, TargetVersions: map[string]uuid.UUID{module.UUID.String(): version.UUID}, Environments: []model.ExternalOperationEnvironment{{ProjectID: project.Uuid, EnvironmentID: env.Uuid}}, Actions: []string{"external-deployment.admit", "external-deployment.rollback", "external-deployment.read", "external-deployment.withdraw"}}}
	command.Intent.Actions = append(command.Intent.Actions, extraActions...)
	grant, err := model.ApplyExternalGrantCommand(ctx, db, command)
	require.NoError(t, err)
	command.Intent, command.Action, command.RequestID, command.ExpectedRevision = nil, "activate", "start", grant.Revision
	grant, err = model.ApplyExternalGrantCommand(ctx, db, command)
	require.NoError(t, err)
	return db, grant
}
