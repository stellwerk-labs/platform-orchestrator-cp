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
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
)

func TestPluginOperationInventoryIsUnattendedButConfinedToTheActiveGrant(t *testing.T) {
	db, grant := pluginDeploymentBoundaryDatabase(t, plugins.InventoryReadAction)
	ctx, now := t.Context(), time.Now().UTC()
	org, err := db.GetOrg(ctx, nil, grant.OrgID)
	require.NoError(t, err)
	inScope, err := db.GetEnvironment(ctx, nil, grant.OrgID, "project", "development", model.GetModeDefault)
	require.NoError(t, err)
	// Neighbours outside the frozen scope that a selector could reach.
	outside, err := db.CreateEnvironment(ctx, nil, &model.Environment{OrgId: org.Id, OrgUuid: org.Uuid, ProjectId: inScope.ProjectId, ProjectUuid: inScope.ProjectUuid,
		Id: "staging", EnvTypeId: inScope.EnvTypeId, EnvTypeUuid: inScope.EnvTypeUuid, CreatedAt: now, UpdatedAt: now, Status: model.EnvironmentStatusActive})
	require.NoError(t, err)
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	otherModule, err := db.CreateEmptyModule(ctx, tx, org.Id, "other", "Other", "", "generic", map[string]string{})
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	service, checker := userid.NewServiceUserTokenId().String(), userid.NewServiceUserTokenId().String()
	permission := "plugin." + grant.PluginID + ".read"
	item := plugins.Installation{ID: grant.PluginID, DisplayName: "Workflow", Version: "1.0.0", Backend: "https://workflow.test", ServiceUserID: service, AdmissionCheckerID: checker,
		Organizations: map[string]plugins.AvailabilityState{grant.OrgID: plugins.StateAvailable},
		Operations:    []plugins.Operation{{ID: "list", Method: "GET", Path: "/orgs/{orgId}/items"}},
		ServiceGrants: []plugins.ServiceGrant{{Action: plugins.InventoryReadAction, Permission: permission, Scope: "environment"}}}
	var denied atomic.Bool
	var iamCalls atomic.Int32
	iam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		iamCalls.Add(1)
		var body iamapi.InternalAuthorizeBody
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		assert.Equal(t, grant.ActorID, body.UserId, "the operation's controller is rechecked, not the service")
		assert.Equal(t, []iamapi.ResourcePermissionCheck{environmentCheck(inScope.Uuid, permission)}, body.Checks, "only the frozen Environment is checked")
		if denied.Load() {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(iam.Close)
	iamClient, err := iamapi.NewClientWithResponses(iam.URL)
	require.NoError(t, err)
	s := &Server{Logger: zap.NewNop(), Database: db, IamClient: iamClient, PluginGateway: &plugins.Gateway{}, PluginJournalRetention: 30 * 24 * time.Hour}
	install := func(installation plugins.Installation) {
		encoded, err := json.Marshal([]plugins.Installation{installation})
		require.NoError(t, err)
		s.PluginGateway.Registry, err = plugins.LoadInstallations(bytes.NewReader(encoded), false)
		require.NoError(t, err)
	}
	install(item)
	e, err := hecho.DefaultEchoServerWithValidation(&hecho.ValidatedServerConfig{AppName: "plugin-operation-inventory-test", Logger: zap.NewNop(), OpenAPIRawSchema: MustDecodeOpenApiSpec()})
	require.NoError(t, err)
	e.Use(middleware.EchoCtxMiddleware)
	s.MapRoutes(e)
	read := func(actor string, operation uuid.UUID) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "/orgs/"+grant.OrgID+"/plugins/"+grant.PluginID+"/operations/"+operation.String()+"/inventory", nil)
		request.Header.Set("From", actor)
		response := httptest.NewRecorder()
		e.ServeHTTP(response, request)
		return response
	}
	requireCode := func(response *httptest.ResponseRecorder, status int, code string) {
		t.Helper()
		require.Equal(t, status, response.Code, response.Body.String())
		require.Contains(t, response.Body.String(), `"error":"`+code+`"`)
	}

	response := read(service, grant.OperationID)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var inventory PluginInventory
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &inventory))
	require.Len(t, inventory.Environments, 1)
	require.Equal(t, inScope.Uuid, inventory.Environments[0].Uuid, "the neighbouring Environment %s is not visible", outside.Id)
	for _, module := range inventory.Modules {
		require.NotEqual(t, otherModule.UUID, module.Uuid, "a Module outside the frozen intent is not visible")
		require.Contains(t, grant.Intent.Modules, module.Uuid)
	}
	require.NotEmpty(t, inventory.Modules)

	calls := iamCalls.Load()
	for _, caller := range []string{checker, userid.NewHumanUserId().String(), userid.NewServiceUserTokenId().String()} {
		require.Equal(t, http.StatusForbidden, read(caller, grant.OperationID).Code, "only the installation's own service identity may read")
	}
	requireCode(read(service, uuid.New()), http.StatusNotFound, "plugin_operation_not_found")
	require.Equal(t, calls, iamCalls.Load(), "rejected callers and unknown operations never reach IAM")

	denied.Store(true)
	require.Equal(t, http.StatusForbidden, read(service, grant.OperationID).Code, "the controller's current IAM decides")
	denied.Store(false)

	withoutGrant := item
	withoutGrant.ServiceGrants = []plugins.ServiceGrant{{Action: plugins.ResourceEventsReadAction, Permission: permission, Scope: "environment"}}
	install(withoutGrant)
	requireCode(read(service, grant.OperationID), http.StatusForbidden, "plugin_service_forbidden")
	install(item)

	// Suspended and recently released operations keep safety reconciliation.
	for index, action := range []string{"suspend", "release"} {
		current, err := model.GetExternalOperationGrant(ctx, db, grant.OrgID, grant.PluginID, grant.OperationID)
		require.NoError(t, err)
		_, err = model.ApplyExternalGrantCommand(ctx, db, model.ExternalGrantCommand{OrgID: grant.OrgID, PluginID: grant.PluginID, OperationID: grant.OperationID,
			ActorID: grant.ActorID, SourceOperation: "command", SourceDigest: strings.Repeat([]string{"c", "d"}[index], 64), RequestID: action, Action: action,
			Reason: "Lifecycle " + action, ExpectedRevision: current.Revision})
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, read(service, grant.OperationID).Code, "a %s operation can still resynchronise", action)
	}
	// Only a database fixture can backdate the release.
	tx, err = db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `UPDATE external_operation_grants SET updated_at=now()-interval '31 days' WHERE org_id=$1 AND plugin_id=$2 AND operation_id=$3`,
		grant.OrgID, grant.PluginID, grant.OperationID)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	requireCode(read(service, grant.OperationID), http.StatusConflict, "plugin_operation_conflict")

	intent := grant.Intent
	prepared, err := model.ApplyExternalGrantCommand(ctx, db, model.ExternalGrantCommand{OrgID: grant.OrgID, PluginID: grant.PluginID, OperationID: uuid.New(),
		ActorID: grant.ActorID, SourceOperation: "prepare", SourceDigest: strings.Repeat("e", 64), RequestID: "prepare-unstarted", Action: "prepare",
		Reason: "Prepare", Intent: &intent})
	require.NoError(t, err)
	requireCode(read(service, prepared.OperationID), http.StatusConflict, "plugin_operation_conflict")
}

func TestPluginOperationInventoryRequiresTheFrozenDelegation(t *testing.T) {
	db, grant := pluginDeploymentBoundaryDatabase(t, plugins.ResourceEventsReadAction)
	service, checker := userid.NewServiceUserTokenId().String(), userid.NewServiceUserTokenId().String()
	permission := "plugin." + grant.PluginID + ".read"
	encoded, err := json.Marshal([]plugins.Installation{{ID: grant.PluginID, DisplayName: "Workflow", Version: "1.0.0", Backend: "https://workflow.test", ServiceUserID: service, AdmissionCheckerID: checker,
		Organizations: map[string]plugins.AvailabilityState{grant.OrgID: plugins.StateAvailable},
		Operations:    []plugins.Operation{{ID: "list", Method: "GET", Path: "/orgs/{orgId}/items"}},
		ServiceGrants: []plugins.ServiceGrant{{Action: plugins.InventoryReadAction, Permission: permission, Scope: "environment"}}}})
	require.NoError(t, err)
	registry, err := plugins.LoadInstallations(bytes.NewReader(encoded), false)
	require.NoError(t, err)
	s := &Server{Logger: zap.NewNop(), Database: db, PluginGateway: &plugins.Gateway{Registry: registry}}
	e, err := hecho.DefaultEchoServerWithValidation(&hecho.ValidatedServerConfig{AppName: "plugin-operation-inventory-delegation-test", Logger: zap.NewNop(), OpenAPIRawSchema: MustDecodeOpenApiSpec()})
	require.NoError(t, err)
	e.Use(middleware.EchoCtxMiddleware)
	s.MapRoutes(e)
	request := httptest.NewRequest(http.MethodGet, "/orgs/"+grant.OrgID+"/plugins/"+grant.PluginID+"/operations/"+grant.OperationID.String()+"/inventory", nil)
	request.Header.Set("From", service)
	response := httptest.NewRecorder()
	e.ServeHTTP(response, request)
	require.Equal(t, http.StatusForbidden, response.Code, "an operation that did not delegate inventory.read cannot read unattended")
	require.Contains(t, response.Body.String(), `"error":"plugin_service_forbidden"`)
}
