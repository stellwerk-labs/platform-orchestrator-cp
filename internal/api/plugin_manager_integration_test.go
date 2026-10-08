//go:build integration

package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	echomiddleware "github.com/labstack/echo/v4/middleware"
	"github.com/stellwerk-labs/golib/hecho"
	"github.com/stellwerk-labs/golib/herrors"
	"github.com/stellwerk-labs/golib/hstandardoutbox"
	iamauthz "github.com/stellwerk-labs/platform-orchestrator-iam/shared/authz"
	iamapi "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/api/middleware"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/pluginmanager"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v2/genevents"
)

// operatorAPI drives one Control Plane replica over HTTP, through the
// generated router and OpenAPI request validation.
type operatorAPI struct {
	t      *testing.T
	server *Server
	e      *echo.Echo
	iam    *iamPeer
}

// bind records the workload binding an operator creates in IAM.
func (p *iamPeer) bind(pluginID string, installation, serviceUser uuid.UUID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bindings = append(p.bindings, iamapi.PluginWorkloadBinding{
		Id: uuid.New(), PluginId: pluginID, InstallationId: installation, ServiceUserId: serviceUser,
		Namespace: "stellwerk-plugins", ServiceAccount: pluginID + "-runtime", Audience: "stellwerk-iam-plugin-workload", CreatedAt: time.Now(),
	})
}

// createServiceUser records a service user an organization administrator
// created in org.
func (p *iamPeer) createServiceUser(org string, id uuid.UUID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.serviceUsers == nil {
		p.serviceUsers = map[string][]uuid.UUID{}
	}
	p.serviceUsers[org] = append(p.serviceUsers[org], id)
}

// deleteServiceUser removes a service user and, as IAM does, its workload
// binding.
func (p *iamPeer) deleteServiceUser(id uuid.UUID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for org, members := range p.serviceUsers {
		p.serviceUsers[org] = slices.DeleteFunc(members, func(member uuid.UUID) bool { return member == id })
	}
	p.bindings = slices.DeleteFunc(p.bindings, func(binding iamapi.PluginWorkloadBinding) bool { return binding.ServiceUserId == id })
}

// newOperatorAPI serves s. pluginOperator holds both Plugin Manager
// permissions in IAM; readers hold plugin_manager_read.
func newOperatorAPI(t *testing.T, s *Server, readers ...uuid.UUID) *operatorAPI {
	t.Helper()
	peer, client := newIAMPeer(t, []uuid.UUID{pluginOperator}, append([]uuid.UUID{pluginOperator}, readers...))
	s.IamClient, s.Logger = client, zap.NewNop()
	e, err := hecho.DefaultEchoServerWithValidation(&hecho.ValidatedServerConfig{AppName: "plugin-manager-test", Logger: zap.NewNop(), OpenAPIRawSchema: MustDecodeOpenApiSpec()})
	require.NoError(t, err)
	e.Use(middleware.EchoCtxMiddleware)
	e.Use(echomiddleware.RequestID())
	s.MapRoutes(e)
	return &operatorAPI{t: t, server: s, e: e, iam: peer}
}

func (a *operatorAPI) call(actor uuid.UUID, method, path, key string, body any) *httptest.ResponseRecorder {
	a.t.Helper()
	var encoded []byte
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		require.NoError(a.t, err)
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(encoded))
	request.Header.Set("From", actor.String())
	request.Header.Set("Content-Type", "application/json")
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	response := httptest.NewRecorder()
	a.e.ServeHTTP(response, request)
	return response
}

// decode requires status and decodes the response body into T.
func decode[T any](t *testing.T, response *httptest.ResponseRecorder, status int) T {
	t.Helper()
	require.Equal(t, status, response.Code, response.Body.String())
	var value T
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &value))
	return value
}

func requireErrorCode(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	require.Equal(t, status, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), `"error":"`+code+`"`)
}

func (a *operatorAPI) revision() int64 {
	a.t.Helper()
	return decode[PluginInstallationPage](a.t, a.call(pluginOperator, http.MethodGet, "/plugin-installations", "", nil), http.StatusOK).Revision
}

func command(revision int64, reason string) PluginManagerCommand {
	return PluginManagerCommand{ExpectedRevision: revision, Reason: reason}
}

// install runs every installation step of item for org over HTTP.
func (a *operatorAPI) install(item plugins.Installation, org string) PluginInstallation {
	a.t.Helper()
	installation := a.activate(item, org)
	timezone := "Europe/Berlin"
	return decode[PluginInstallation](a.t, a.call(pluginOperator, http.MethodPut, "/plugin-installations/"+installation.InstallationId.String()+"/organizations/"+org, uuid.NewString(),
		PluginEnablementPut{ExpectedRevision: *installation.Revision, Reason: "Enable the organization", State: PluginEnablementPutStateEnabled, Timezone: &timezone}), http.StatusOK)
}

// activate runs the installation steps of item over HTTP up to its
// activation. The plugin's service user is created in org.
func (a *operatorAPI) activate(item plugins.Installation, org string) PluginInstallation {
	a.t.Helper()
	t := a.t
	manifest, err := json.Marshal(item.Manifest())
	require.NoError(t, err)
	digest := sha256Hex(manifest)
	pkg := decode[PluginPackage](t, a.call(pluginOperator, http.MethodPost, "/plugin-packages", uuid.NewString(),
		PluginPackagePush{ExpectedRevision: a.revision(), Reason: "Register the package", ManifestSha256: digest, Manifest: manifest}), http.StatusCreated)
	require.Equal(t, item.ID, pkg.PluginId)
	installation := decode[PluginInstallation](t, a.call(pluginOperator, http.MethodPost, "/plugin-installations", uuid.NewString(),
		PluginInstallationCreate{ExpectedRevision: *pkg.Revision, Reason: "Install the plugin", PackageSha256: digest, BackendUrl: item.Backend}), http.StatusCreated)
	require.Equal(t, PluginInstallationStateInstalled, installation.State)
	fleet.serve(t, item.Backend, installation.InstallationId, digest)
	a.iam.createServiceUser(org, uuid.MustParse(item.ServiceUserID))
	a.iam.bind(item.ID, installation.InstallationId, uuid.MustParse(item.ServiceUserID))
	path := "/plugin-installations/" + installation.InstallationId.String()
	installation = decode[PluginInstallation](t, a.call(pluginOperator, http.MethodPut, path+"/identity", uuid.NewString(),
		PluginInstallationIdentity{ExpectedRevision: *installation.Revision, Reason: "Bind the service identities", ServiceUserId: uuid.MustParse(item.ServiceUserID), AdmissionCheckerId: uuid.MustParse(item.AdmissionCheckerID)}), http.StatusOK)
	installation = decode[PluginInstallation](t, a.call(pluginOperator, http.MethodPost, path+"/actions/activate", uuid.NewString(), command(*installation.Revision, "Activate the plugin")), http.StatusOK)
	require.Equal(t, PluginInstallationStateActive, installation.State)
	return installation
}

func sha256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// pluginManagerServer is a plugin host replica over db.
func pluginManagerServer(t *testing.T, db model.Databaser) *Server {
	t.Helper()
	registry := &plugins.InstallationRegistry{}
	manager := testPluginManager(t, db, registry, nil)
	require.NoError(t, manager.Refresh(t.Context()))
	return &Server{Database: db, Plugins: registry, PluginGateway: &plugins.Gateway{Registry: registry}, PluginManager: manager}
}

func workflowPlugin(grant *model.ExternalOperationGrant, actions ...string) plugins.Installation {
	item := plugins.Installation{ID: grant.PluginID, DisplayName: "Workflow", Version: "1.0.0", Backend: "https://workflow.test",
		ServiceUserID: userid.NewServiceUserTokenId().String(), AdmissionCheckerID: userid.NewServiceUserTokenId().String(),
		Operations: []plugins.Operation{{ID: "list", Method: "GET", Path: "/orgs/{orgId}/items"}}}
	for _, action := range actions {
		item.ServiceGrants = append(item.ServiceGrants, plugins.ServiceGrant{Action: action, Permission: "plugin." + grant.PluginID + ".read", Scope: "environment"})
	}
	return item
}

type operatorRoute struct {
	method, path string
	write        bool
	body         any
}

// operatorRoutes is every Plugin Manager route with a valid request.
func operatorRoutes(installation uuid.UUID, org string) []operatorRoute {
	path := "/plugin-installations/" + installation.String()
	bundle := []byte(`{"schema_version":1,"grants":[],"revocations":[]}`)
	return []operatorRoute{
		{http.MethodGet, "/plugin-packages", false, nil},
		{http.MethodPost, "/plugin-packages", true, PluginPackagePush{Reason: "r", ManifestSha256: sha256Hex([]byte("m")), Manifest: json.RawMessage(`{"id":"dev.example.other","display_name":"Other","version":"1.0.0"}`)}},
		{http.MethodGet, "/plugin-installations", false, nil},
		{http.MethodPost, "/plugin-installations", true, PluginInstallationCreate{Reason: "r", PackageSha256: sha256Hex([]byte("m")), BackendUrl: "https://other.test"}},
		{http.MethodGet, path, false, nil},
		{http.MethodPut, path + "/identity", true, PluginInstallationIdentity{Reason: "r", ServiceUserId: userid.NewServiceUserTokenId(), AdmissionCheckerId: userid.NewServiceUserTokenId()}},
		{http.MethodPost, path + "/actions/activate", true, command(0, "r")},
		{http.MethodPost, path + "/actions/disable", true, command(0, "r")},
		{http.MethodPost, path + "/actions/enable", true, command(0, "r")},
		{http.MethodPost, path + "/actions/uninstall", true, command(0, "r")},
		{http.MethodPost, path + "/actions/reinstall", true, command(0, "r")},
		{http.MethodPost, path + "/upgrade", true, PluginInstallationUpgrade{Reason: "r", PackageSha256: sha256Hex([]byte("m"))}},
		{http.MethodDelete, path, true, command(0, "r")},
		{http.MethodPut, path + "/organizations/" + org, true, PluginEnablementPut{Reason: "r", State: PluginEnablementPutStateDisabled}},
		{http.MethodDelete, path + "/organizations/" + org, true, command(0, "r")},
		{http.MethodPost, "/plugin-entitlement-bundles", true, PluginEntitlementBundleImportRequest{Reason: "r", Bundle: bundle}},
		{http.MethodGet, "/plugin-audit-events", false, nil},
	}
}

func TestPluginManagerOperatorAPIInstallsWithoutRestart(t *testing.T) {
	db, grant := pluginDeploymentBoundaryDatabase(t)
	writer, other := pluginManagerServer(t, db), pluginManagerServer(t, db)
	api := newOperatorAPI(t, writer)
	go other.PluginManager.Run(t.Context(), 50*time.Millisecond)
	installation := api.install(workflowPlugin(grant, "external-deployment.admit"), grant.OrgID)
	require.Len(t, installation.Organizations, 1)
	require.Equal(t, PluginEnablementStateEnabled, installation.Organizations[0].State)
	require.Equal(t, "Europe/Berlin", installation.Organizations[0].Timezone)
	descriptor, found := writer.PluginGateway.Registry.Get(t.Context(), grant.OrgID, grant.PluginID)
	require.True(t, found, "the writing replica serves the installation at once")
	require.Equal(t, plugins.StateAvailable, descriptor.State)
	require.Eventually(t, func() bool {
		descriptor, found := other.PluginGateway.Registry.Get(t.Context(), grant.OrgID, grant.PluginID)
		return found && descriptor.State == plugins.StateAvailable
	}, 2*time.Second, 10*time.Millisecond, "another replica serves it within its refresh interval")

	// An HTTP retry with the same key replays the exact response.
	path := "/plugin-installations/" + installation.InstallationId.String()
	key := uuid.NewString()
	disable := command(*installation.Revision, "Pause the plugin")
	first := api.call(pluginOperator, http.MethodPost, path+"/actions/disable", key, disable)
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	replay := api.call(pluginOperator, http.MethodPost, path+"/actions/disable", key, disable)
	require.Equal(t, http.StatusOK, replay.Code)
	require.JSONEq(t, first.Body.String(), replay.Body.String())
	requireErrorCode(t, api.call(pluginOperator, http.MethodPost, path+"/actions/enable", key, disable), http.StatusConflict, "plugin_manager_idempotency_conflict")
	requireErrorCode(t, api.call(pluginOperator, http.MethodPost, path+"/actions/enable", uuid.NewString(), disable), http.StatusConflict, "plugin_manager_revision_conflict")
	requireErrorCode(t, api.call(pluginOperator, http.MethodPost, path+"/actions/enable", uuid.NewString(), command(api.revision(), " ")), http.StatusBadRequest, "plugin_manager_request_invalid")
	require.Equal(t, http.StatusBadRequest, api.call(pluginOperator, http.MethodPost, path+"/actions/enable", "", command(api.revision(), "r")).Code, "the idempotency key is required")
	require.Equal(t, http.StatusBadRequest, api.call(pluginOperator, http.MethodPost, path+"/actions/enable", uuid.NewString(), map[string]any{"expected_revision": api.revision()}).Code, "a reason is required")
	requireErrorCode(t, api.call(pluginOperator, http.MethodGet, "/plugin-installations/"+uuid.NewString(), "", nil), http.StatusNotFound, "plugin_installation_not_found")

	audit := decode[PluginAuditEventPage](t, api.call(pluginOperator, http.MethodGet, "/plugin-audit-events?limit=200", "", nil), http.StatusOK)
	require.Len(t, audit.Items, 6, "push, create, identity, activate, enable organization, disable")
	last := audit.Items[len(audit.Items)-1]
	require.Equal(t, "plugin_installation.disable", last.Action)
	require.Equal(t, pluginOperator, last.Actor)
	require.Equal(t, "Pause the plugin", last.Reason)
	require.NotEmpty(t, ref.DerefOr(last.RequestId, ""), "the request ID of the operator call is audited")
	require.Contains(t, string(last.Before), `"state":"active"`)
	require.Contains(t, string(last.After), `"state":"disabled"`)
}

func TestOnlyPlatformOperatorsReachThePluginManager(t *testing.T) {
	db, grant := pluginDeploymentBoundaryDatabase(t)
	reader := userid.NewHumanUserId()
	api := newOperatorAPI(t, pluginManagerServer(t, db), reader)
	item := workflowPlugin(grant, "external-deployment.admit")
	installation := api.install(item, grant.OrgID)
	audited := len(decode[PluginAuditEventPage](t, api.call(pluginOperator, http.MethodGet, "/plugin-audit-events", "", nil), http.StatusOK).Items)
	api.iam.orgChecks.Store(0)

	denied := map[string]uuid.UUID{
		"organization administrator": userid.NewHumanUserId(),
		"the plugin's workload":      uuid.MustParse(item.ServiceUserID),
		"the admission checker":      uuid.MustParse(item.AdmissionCheckerID),
		"another service user":       userid.NewServiceUserTokenId(),
		"the internal system":        userid.InternalSystemUuid,
	}
	for _, route := range operatorRoutes(installation.InstallationId, grant.OrgID) {
		for name, actor := range denied {
			response := api.call(actor, route.method, route.path, uuid.NewString(), route.body)
			require.Equal(t, http.StatusForbidden, response.Code, "%s on %s %s: %s", name, route.method, route.path, response.Body.String())
			require.Contains(t, response.Body.String(), `"error":"plugin_manager_forbidden"`)
		}
		response := api.call(reader, route.method, route.path, uuid.NewString(), route.body)
		required := iamauthz.PermissionPluginManagerRead
		if route.write {
			required = iamauthz.PermissionPluginManagerWrite
		}
		checks := api.iam.platformChecks()
		require.Equal(t, iamauthz.PlatformCheck(required), checks[len(checks)-1], "%s %s asks IAM for exactly %s", route.method, route.path, required)
		if route.write {
			requireErrorCode(t, response, http.StatusForbidden, "plugin_manager_forbidden")
		} else {
			require.Equal(t, http.StatusOK, response.Code, "%s %s: %s", route.method, route.path, response.Body.String())
		}
	}
	require.Zero(t, api.iam.orgChecks.Load(), "organization roles are never consulted")
	require.Len(t, decode[PluginAuditEventPage](t, api.call(pluginOperator, http.MethodGet, "/plugin-audit-events", "", nil), http.StatusOK).Items, audited, "denied calls change nothing")
}

func TestIdentityRequiresTheExactWorkloadBindingInIAM(t *testing.T) {
	db, grant := pluginDeploymentBoundaryDatabase(t)
	api := newOperatorAPI(t, pluginManagerServer(t, db))
	item := workflowPlugin(grant, "external-deployment.admit")
	manifest, err := json.Marshal(item.Manifest())
	require.NoError(t, err)
	digest := sha256Hex(manifest)
	pkg := decode[PluginPackage](t, api.call(pluginOperator, http.MethodPost, "/plugin-packages", uuid.NewString(),
		PluginPackagePush{ExpectedRevision: api.revision(), Reason: "Register the package", ManifestSha256: digest, Manifest: manifest}), http.StatusCreated)
	installation := decode[PluginInstallation](t, api.call(pluginOperator, http.MethodPost, "/plugin-installations", uuid.NewString(),
		PluginInstallationCreate{ExpectedRevision: *pkg.Revision, Reason: "Install the plugin", PackageSha256: digest, BackendUrl: item.Backend}), http.StatusCreated)
	path := "/plugin-installations/" + installation.InstallationId.String()
	serviceUser, checker := uuid.MustParse(item.ServiceUserID), uuid.MustParse(item.AdmissionCheckerID)
	recordIdentity := func() *httptest.ResponseRecorder {
		return api.call(pluginOperator, http.MethodPut, path+"/identity", uuid.NewString(), PluginInstallationIdentity{
			ExpectedRevision: *installation.Revision, Reason: "Bind the service identities", ServiceUserId: serviceUser, AdmissionCheckerId: checker,
		})
	}
	audited := len(decode[PluginAuditEventPage](t, api.call(pluginOperator, http.MethodGet, "/plugin-audit-events", "", nil), http.StatusOK).Items)

	requireErrorCode(t, recordIdentity(), http.StatusConflict, "plugin_workload_binding_missing")
	for name, bind := range map[string]func(){
		"another installation of the plugin":   func() { api.iam.bind(item.ID, uuid.New(), serviceUser) },
		"another plugin with the same UUID":    func() { api.iam.bind("dev.example.other", installation.InstallationId, serviceUser) },
		"another service user of the plugin":   func() { api.iam.bind(item.ID, installation.InstallationId, userid.NewServiceUserTokenId()) },
		"the admission checker's own identity": func() { api.iam.bind(item.ID, installation.InstallationId, checker) },
	} {
		bind()
		response := recordIdentity()
		requireErrorCode(t, response, http.StatusConflict, "plugin_workload_binding_missing")
		require.Contains(t, response.Body.String(), serviceUser.String(), name)
	}
	api.iam.bind(item.ID, installation.InstallationId, serviceUser)
	api.iam.lookupDown.Store(true)
	require.Equal(t, http.StatusInternalServerError, recordIdentity().Code, "an unanswered lookup never records an identity")
	api.iam.lookupDown.Store(false)

	unchanged := decode[PluginInstallation](t, api.call(pluginOperator, http.MethodGet, path, "", nil), http.StatusOK)
	require.Nil(t, unchanged.ServiceUserId)
	require.Equal(t, *installation.Revision, *unchanged.Revision, "refused identities change nothing")
	require.Len(t, decode[PluginAuditEventPage](t, api.call(pluginOperator, http.MethodGet, "/plugin-audit-events", "", nil), http.StatusOK).Items, audited)

	recorded := decode[PluginInstallation](t, recordIdentity(), http.StatusOK)
	require.Equal(t, serviceUser, *recorded.ServiceUserId)
	require.Equal(t, checker, *recorded.AdmissionCheckerId)
}

func TestAnInstallationServesOnlyTheOrganizationOfItsServiceUser(t *testing.T) {
	db, grant := pluginDeploymentBoundaryDatabase(t)
	s := pluginManagerServer(t, db)
	api := newOperatorAPI(t, s)
	ctx := t.Context()
	const other = "beta"
	_, err := db.CreateOrg(ctx, nil, &model.Org{Id: other, CreatedBy: pluginOperator, CreatedAt: time.Now().UTC(), Source: model.OrgSourceInternal, Plan: model.OrgPlanCustom})
	require.NoError(t, err)
	// Both organizations have more service users than IAM returns on one
	// page, so Core has to follow IAM's page tokens.
	for _, org := range []string{grant.OrgID, other} {
		for range serviceUserPage + 1 {
			api.iam.createServiceUser(org, userid.NewServiceUserTokenId())
		}
	}
	item := workflowPlugin(grant, "external-deployment.admit")
	serviceUser, checker := uuid.MustParse(item.ServiceUserID), uuid.MustParse(item.AdmissionCheckerID)
	api.iam.createServiceUser(grant.OrgID, serviceUser)
	manifest, err := json.Marshal(item.Manifest())
	require.NoError(t, err)
	digest := sha256Hex(manifest)
	decode[PluginPackage](t, api.call(pluginOperator, http.MethodPost, "/plugin-packages", uuid.NewString(),
		PluginPackagePush{ExpectedRevision: api.revision(), Reason: "Register the package", ManifestSha256: digest, Manifest: manifest}), http.StatusCreated)
	installation := decode[PluginInstallation](t, api.call(pluginOperator, http.MethodPost, "/plugin-installations", uuid.NewString(),
		PluginInstallationCreate{ExpectedRevision: api.revision(), Reason: "Install the plugin", PackageSha256: digest, BackendUrl: item.Backend}), http.StatusCreated)
	fleet.serve(t, item.Backend, installation.InstallationId, digest)
	path := "/plugin-installations/" + installation.InstallationId.String()
	timezone := "UTC"
	enablement := func(org string, state PluginEnablementPutState) func() *httptest.ResponseRecorder {
		return func() *httptest.ResponseRecorder {
			return api.call(pluginOperator, http.MethodPut, path+"/organizations/"+org, uuid.NewString(),
				PluginEnablementPut{ExpectedRevision: api.revision(), Reason: "Enable " + org, State: state, Timezone: &timezone})
		}
	}
	removal := func(org string) func() *httptest.ResponseRecorder {
		return func() *httptest.ResponseRecorder {
			return api.call(pluginOperator, http.MethodDelete, path+"/organizations/"+org, uuid.NewString(), command(api.revision(), "Remove "+org))
		}
	}
	identity := func() *httptest.ResponseRecorder {
		return api.call(pluginOperator, http.MethodPut, path+"/identity", uuid.NewString(), PluginInstallationIdentity{
			ExpectedRevision: api.revision(), Reason: "Record the service identities", ServiceUserId: serviceUser, AdmissionCheckerId: checker})
	}
	audited := func() int {
		return len(decode[PluginAuditEventPage](t, api.call(pluginOperator, http.MethodGet, "/plugin-audit-events?limit=200", "", nil), http.StatusOK).Items)
	}
	refused := func(why string, status int, code string, call func() *httptest.ResponseRecorder) string {
		t.Helper()
		revision, events := api.revision(), audited()
		response := call()
		require.Equal(t, status, response.Code, "%s: %s", why, response.Body.String())
		require.Contains(t, response.Body.String(), code, why)
		require.Equal(t, revision, api.revision(), "%s changes nothing", why)
		require.Equal(t, events, audited(), "%s writes no audit event", why)
		return response.Body.String()
	}
	succeeds := func(call func() *httptest.ResponseRecorder) PluginInstallation {
		t.Helper()
		return decode[PluginInstallation](t, call(), http.StatusOK)
	}
	const mismatch = `"error":"plugin_service_user_organization_mismatch"`

	refused("enabling before the identities", http.StatusConflict, `"error":"plugin_installation_state"`, enablement(grant.OrgID, PluginEnablementPutStateEnabled))
	api.iam.bind(item.ID, installation.InstallationId, serviceUser)
	succeeds(identity)
	message := refused("enabling another organization", http.StatusConflict, mismatch, enablement(other, PluginEnablementPutStateEnabled))
	require.Contains(t, message, serviceUser.String())
	require.Contains(t, message, other)
	refused("enabling the service user's organization before activation", http.StatusConflict, `"error":"plugin_installation_state"`, enablement(grant.OrgID, PluginEnablementPutStateEnabled))
	succeeds(func() *httptest.ResponseRecorder {
		return api.call(pluginOperator, http.MethodPost, path+"/actions/activate", uuid.NewString(), command(api.revision(), "Activate the plugin"))
	})
	refused("enabling another organization of an active installation", http.StatusConflict, mismatch, enablement(other, PluginEnablementPutStateDisabled))
	api.iam.lookupDown.Store(true)
	refused("an unanswered IAM lookup", http.StatusInternalServerError, "", enablement(grant.OrgID, PluginEnablementPutStateEnabled))
	api.iam.lookupDown.Store(false)

	enabled := succeeds(enablement(grant.OrgID, PluginEnablementPutStateEnabled))
	require.Len(t, enabled.Organizations, 1)
	require.Equal(t, grant.OrgID, enabled.Organizations[0].OrgId)
	refused("a second organization", http.StatusConflict, mismatch, enablement(other, PluginEnablementPutStateEnabled))
	replayed := succeeds(identity)
	require.Equal(t, *enabled.Revision, *replayed.Revision, "recording the same identities of an enabled installation changes nothing")

	// An older Core that did not ask IAM enabled another organization: the
	// identities of that installation are refused until the enablement is
	// removed, which never asks IAM.
	succeeds(removal(grant.OrgID))
	_, err = s.PluginManager.PutEnablement(ctx, pluginmanager.Command{Actor: pluginOperator, IdempotencyKey: uuid.NewString(), ExpectedRevision: api.revision(), Reason: "Enable without asking IAM"},
		installation.InstallationId, other, model.PluginEnablementEnabled, "UTC", nil, item.ServiceUserID)
	require.NoError(t, err)
	message = refused("identities of an installation enabled for another organization", http.StatusConflict, mismatch, identity)
	require.Contains(t, message, other)
	require.Empty(t, succeeds(removal(other)).Organizations)

	// Changing an existing enablement keeps its organization, so the safety
	// lever works even after IAM deleted the service user; a new enablement
	// needs IAM again.
	succeeds(enablement(grant.OrgID, PluginEnablementPutStateEnabled))
	api.iam.deleteServiceUser(serviceUser)
	disabled := succeeds(enablement(grant.OrgID, PluginEnablementPutStateDisabled))
	require.Equal(t, PluginEnablementStateDisabled, disabled.Organizations[0].State)
	require.Empty(t, succeeds(removal(grant.OrgID)).Organizations)
	refused("enabling an organization IAM no longer lists the service user in", http.StatusConflict, mismatch, enablement(grant.OrgID, PluginEnablementPutStateEnabled))
}

func TestPluginHostDisabledHidesOnlyThePluginManager(t *testing.T) {
	db, grant := pluginDeploymentBoundaryDatabase(t)
	api := newOperatorAPI(t, &Server{Database: db})
	require.NoError(t, model.ReconcileExtensionResourceJournal(t.Context(), db, false))
	for _, route := range operatorRoutes(uuid.New(), grant.OrgID) {
		for _, actor := range []uuid.UUID{pluginOperator, userid.NewHumanUserId()} {
			requireErrorCode(t, api.call(actor, route.method, route.path, uuid.NewString(), route.body), http.StatusNotFound, "plugin_host_disabled")
		}
	}
	user := userid.NewHumanUserId()
	contributions := decode[PluginContributionPage](t, api.call(user, http.MethodGet, "/orgs/"+grant.OrgID+"/plugins", "", nil), http.StatusOK)
	require.Empty(t, contributions.Items)
	require.Equal(t, PluginContractLevelV1alpha, contributions.ContractLevel)
	response := api.call(user, http.MethodGet, "/orgs/"+grant.OrgID+"/env-types", "", nil)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), `"development"`, "Core-only operation is unaffected")
}

func TestDisablingAnOrganizationStopsEffectsOnEveryReplicaAtOnce(t *testing.T) {
	db, grant := pluginDeploymentBoundaryDatabase(t)
	writer, other := pluginManagerServer(t, db), pluginManagerServer(t, db)
	api := newOperatorAPI(t, writer)
	installation := api.install(workflowPlugin(grant, "external-deployment.admit"), grant.OrgID)
	require.NoError(t, other.PluginManager.Refresh(t.Context()))
	item, descriptor, found := other.PluginGateway.Registry.Installation(t.Context(), grant.OrgID, grant.PluginID)
	require.True(t, found)
	require.Equal(t, plugins.StateAvailable, descriptor.State)
	effect := func(safety bool) error {
		tx, err := other.pluginEffectDatabase(grant.OrgID, item, "external-deployment.admit", safety).BeginTx(t.Context(), nil)
		if err == nil {
			require.NoError(t, tx.Rollback())
		}
		return err
	}
	require.NoError(t, effect(false))

	path := "/plugin-installations/" + installation.InstallationId.String() + "/organizations/" + grant.OrgID
	decode[PluginInstallation](t, api.call(pluginOperator, http.MethodPut, path, uuid.NewString(),
		PluginEnablementPut{ExpectedRevision: api.revision(), Reason: "Stop new work", State: PluginEnablementPutStateDisabled}), http.StatusOK)
	_, stale, _ := other.PluginGateway.Registry.Installation(t.Context(), grant.OrgID, grant.PluginID)
	require.Equal(t, plugins.StateAvailable, stale.State, "the other replica has not refreshed its snapshot yet")
	requireEffectCode(t, effect(false), "plugin_not_enabled")
	require.NoError(t, effect(true), "safety work continues")

	go other.PluginManager.Run(t.Context(), 50*time.Millisecond)
	require.Eventually(t, func() bool {
		descriptor, _ := other.PluginGateway.Registry.Get(t.Context(), grant.OrgID, grant.PluginID)
		return descriptor.State == plugins.StateNotEnabled
	}, 2*time.Second, 10*time.Millisecond, "reads follow within the refresh bound")

	decode[PluginInstallation](t, api.call(pluginOperator, http.MethodDelete, path, uuid.NewString(), command(api.revision(), "Remove the organization")), http.StatusOK)
	requireEffectCode(t, effect(false), "plugin_not_installed")
	requireEffectCode(t, effect(true), "plugin_not_installed")
}

func requireEffectCode(t *testing.T, err error, code string) {
	t.Helper()
	var denied *herrors.PlatformOrchestratorError
	require.ErrorAs(t, err, &denied, "expected %s", code)
	require.Equal(t, 409, denied.StatusCode)
	require.Equal(t, code, denied.Code)
}

func journalEnvironmentEvent(t *testing.T, db model.Databaser, org string, environment uuid.UUID) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"specversion": "1.0", "type": genevents.IoPlatformOrchestratorEnvironmentDeleted, "time": time.Now().UTC(),
		"data": map[string]any{"org_id": org, "environment_uuid": environment}})
	require.NoError(t, err)
	_, err = db.InsertPendingEventMessages(t.Context(), nil, []*hstandardoutbox.PendingEventMessage{{Subject: string(genevents.IoPlatformOrchestratorEnvironmentDeleted), Payload: payload}})
	require.NoError(t, err)
}

func TestEnablementStartsTheResourceJournalAndRemovalLeavesAGap(t *testing.T) {
	db, grant := pluginDeploymentBoundaryDatabase(t, "resource-events.read")
	s := pluginManagerServer(t, db)
	api := newOperatorAPI(t, s)
	environment := grant.Intent.Environments[0].EnvironmentID
	item := workflowPlugin(grant, "resource-events.read")
	journalEnvironmentEvent(t, db, grant.OrgID, environment)
	installation := api.install(item, grant.OrgID)

	service := uuid.MustParse(item.ServiceUserID)
	read := func(after string) *httptest.ResponseRecorder {
		path := "/orgs/" + grant.OrgID + "/plugins/" + grant.PluginID + "/operations/" + grant.OperationID.String() + "/resource-events?limit=10"
		if after != "" {
			path += "&after=" + after
		}
		return api.call(service, http.MethodGet, path, "", nil)
	}
	page := decode[ReadPluginResourceEvents200JSONResponse](t, read(""), http.StatusOK)
	require.Empty(t, page.Events, "history starts when the subscription is enabled")
	journalEnvironmentEvent(t, db, grant.OrgID, environment)
	page = decode[ReadPluginResourceEvents200JSONResponse](t, read(page.NextCursor), http.StatusOK)
	require.Len(t, page.Events, 1)
	cursor := page.NextCursor

	enablement := "/plugin-installations/" + installation.InstallationId.String() + "/organizations/" + grant.OrgID
	decode[PluginInstallation](t, api.call(pluginOperator, http.MethodDelete, enablement, uuid.NewString(), command(api.revision(), "Remove the subscription")), http.StatusOK)
	var journaling bool
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT journaling FROM extension_resource_event_journal_state WHERE org_id=$1`, grant.OrgID).Scan(&journaling))
	require.NoError(t, tx.Rollback())
	require.False(t, journaling, "removing the only subscription pauses the journal")
	requireErrorCode(t, read(cursor), http.StatusForbidden, "plugin_service_forbidden")
	journalEnvironmentEvent(t, db, grant.OrgID, environment)

	timezone := "UTC"
	decode[PluginInstallation](t, api.call(pluginOperator, http.MethodPut, enablement, uuid.NewString(),
		PluginEnablementPut{ExpectedRevision: api.revision(), Reason: "Subscribe again", State: PluginEnablementPutStateEnabled, Timezone: &timezone}), http.StatusOK)
	requireErrorCode(t, read(cursor), http.StatusConflict, "plugin_resource_cursor_expired")
	page = decode[ReadPluginResourceEvents200JSONResponse](t, read(""), http.StatusOK)
	require.Empty(t, page.Events, "the change made while paused is never served as if nothing was missed")
	journalEnvironmentEvent(t, db, grant.OrgID, environment)
	page = decode[ReadPluginResourceEvents200JSONResponse](t, read(page.NextCursor), http.StatusOK)
	require.Len(t, page.Events, 1, "a cursor issued after the gap stays valid")
}

func TestEntitlementBundleImportTakesEffectWithoutRestart(t *testing.T) {
	f := newUnimportedPluginEntitlementAPIFixture(t)
	registry := &plugins.InstallationRegistry{Entitlements: f.adapter}
	manager := testPluginManager(t, f.db, registry, f.adapter)
	require.NoError(t, manager.Refresh(t.Context()))
	f.server.PluginManager, f.server.PluginEntitlements, f.server.PluginGateway = manager, f.adapter, &plugins.Gateway{Registry: registry}
	api := newOperatorAPI(t, f.server)
	descriptor, found := registry.Get(t.Context(), "acme", f.item.ID)
	require.True(t, found)
	require.Equal(t, plugins.StateNotEntitled, descriptor.State)
	_, err := f.server.pluginEffectDatabase("acme", f.item, "effect.commit", false).BeginTx(t.Context(), nil)
	requirePlatformEntitlementCode(t, err, "plugin_not_entitled")

	foreign := f.signGrantFor(t, uuid.New(), uuid.New(), 1, f.base+10, f.base+20)
	revision := api.revision()
	rejected := decode[PluginEntitlementBundleImport](t, api.call(pluginOperator, http.MethodPost, "/plugin-entitlement-bundles", uuid.NewString(),
		PluginEntitlementBundleImportRequest{ExpectedRevision: revision, Reason: "Import a licence for another installation", Bundle: f.bundle(t, foreign)}), http.StatusOK)
	require.Zero(t, rejected.ImportedGrants)
	require.Len(t, rejected.Diagnostics, 1)
	require.Equal(t, "plugin_entitlement_grant_rejected", rejected.Diagnostics[0].Code, "a grant for another installation UUID never matches")
	require.Equal(t, revision, rejected.Revision, "a rejected import changes nothing")

	key := uuid.NewString()
	request := PluginEntitlementBundleImportRequest{ExpectedRevision: revision, Reason: "Import the signed licence", Bundle: f.bundle(t, f.grant.Envelope)}
	imported := decode[PluginEntitlementBundleImport](t, api.call(pluginOperator, http.MethodPost, "/plugin-entitlement-bundles", key, request), http.StatusOK)
	require.Equal(t, 1, imported.ImportedGrants)
	require.Empty(t, imported.Diagnostics)
	require.Equal(t, revision+1, imported.Revision)
	replayed := decode[PluginEntitlementBundleImport](t, api.call(pluginOperator, http.MethodPost, "/plugin-entitlement-bundles", key, request), http.StatusOK)
	require.Equal(t, imported, replayed)

	descriptor, _ = registry.Get(t.Context(), "acme", f.item.ID)
	require.Equal(t, plugins.StateAvailable, descriptor.State, "the licence applies without a restart")
	require.Equal(t, "valid", descriptor.Entitlement.Features["feature-a"].State)
	tx, err := f.server.pluginEffectDatabase("acme", f.item, "effect.commit", false).BeginTx(t.Context(), nil)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
	audit := decode[PluginAuditEventPage](t, api.call(pluginOperator, http.MethodGet, "/plugin-audit-events?after=0&limit=200", "", nil), http.StatusOK)
	last := audit.Items[len(audit.Items)-1]
	require.Equal(t, "plugin_entitlement_bundle.import", last.Action)
	require.Contains(t, string(last.After), `"imported_grants":1`)
	require.Equal(t, revision+1, last.Revision)
}
