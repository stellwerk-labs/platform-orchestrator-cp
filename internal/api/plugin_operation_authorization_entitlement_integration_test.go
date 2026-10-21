//go:build integration

package api

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/hecho"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/moduleversions"
	iamapi "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/api/middleware"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v2/plugincontext"
)

type pluginOperationEntitlementHTTP struct {
	f              *pluginEntitlementAPIFixture
	grant          *model.ExternalOperationGrant
	item           plugins.Installation
	denied         atomic.Bool
	checks         atomic.Int64
	rotateForward  atomic.Bool
	secondActor    uuid.UUID
	proof          func(string) PluginDelegationProof
	actorProof     func(string, uuid.UUID) PluginDelegationProof
	call           func(string, uuid.UUID, any) *httptest.ResponseRecorder
	moduleCall     func(string, uuid.UUID, any) *httptest.ResponseRecorder
	deploymentCall func(string, uuid.UUID, string, any) *httptest.ResponseRecorder
}

func TestPluginPrepareRejectsArchivedModuleThroughNativeAPI(t *testing.T) {
	h := newPluginOperationEntitlementHTTP(t)
	moduleID := h.grant.Intent.Modules[0]
	module, err := h.f.db.GetModuleCatalogue(t.Context(), nil, "acme", moduleID.String(), model.GetModeDefault)
	require.NoError(t, err)
	tx, err := h.f.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = h.f.db.SetModuleCatalogueStatus(t.Context(), tx, "acme", moduleID.String(), moduleversions.CatalogueArchived,
		h.grant.ActorID, "Retire proposed target", module.ResourceVersion)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	operationID := uuid.New()
	proofGrant := *h.grant
	proofGrant.OperationID = operationID
	h.grant = &proofGrant
	encoded, err := json.Marshal(h.grant.Intent)
	require.NoError(t, err)
	var intent PluginOperationIntent
	require.NoError(t, json.Unmarshal(encoded, &intent))
	request := PluginOperationAuthorizationRequest{
		Action: "prepare", ExpectedRevision: 0, Intent: &intent, Proof: h.proof("prepare"), Reason: "Prepare proposed target",
	}
	_, err = model.GetExternalOperationGrant(t.Context(), h.f.db, "acme", h.item.ID, operationID)
	require.Error(t, err, "the fresh operation identity must have no prior grant")
	response := h.call(h.item.ServiceUserID, operationID, request)
	require.Equal(t, http.StatusConflict, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), `"error":"plugin_operation_conflict"`)
	require.EqualValues(t, 1, h.checks.Load(), "the real Core prepare path is reached after delegated IAM authorization")
	_, err = model.GetExternalOperationGrant(t.Context(), h.f.db, "acme", h.item.ID, operationID)
	require.Error(t, err, "rejected preparation must not persist the operation grant")
}

func newPluginOperationEntitlementHTTP(t *testing.T) *pluginOperationEntitlementHTTP {
	t.Helper()
	recovery := newPluginGrantRecoveryHTTP(t, false, false)
	h := &pluginOperationEntitlementHTTP{f: recovery.fixture, grant: recovery.grant, item: recovery.item}
	permission := "plugin." + h.item.ID + ".control"
	for _, action := range []string{"operation.prepare", "operation.activate", "operation.suspend", "operation.release"} {
		h.item.ServiceGrants = append(h.item.ServiceGrants, plugins.ServiceGrant{Action: action, RequiredFeature: "feature-a", Permission: permission, Scope: "environment"})
	}
	allActions := []string{"operation.prepare"}
	for _, policy := range h.item.ServiceGrants {
		if policy.Action != "operation.prepare" {
			allActions = append(allActions, policy.Action)
		}
	}
	h.item.Operations = []plugins.Operation{{
		ID: "command", Method: "POST", Path: "/orgs/{orgId}/workflows/{id}/{command}", Mutating: true, RequiredFeature: "feature-a",
		PathValues:       map[string][]string{"command": {"prepare", "resume", "rollback", "pause", "cancel"}},
		SafetyPathValues: map[string][]string{"command": {"rollback", "pause", "cancel"}},
		ServiceActions:   allActions,
		ServiceActionPathValues: map[string]map[string][]string{
			"operation.activate": {"command": {"resume", "rollback"}},
			"operation.suspend":  {"command": {"pause"}},
			"operation.release":  {"command": {"cancel"}},
			// Even an overly broad source declaration cannot turn preparation
			// into a safety effect or create new work after licence expiry.
			"operation.prepare": {"command": {"prepare", "rollback"}},
		},
	}}
	registry, err := plugins.NewInstallationRegistry([]plugins.Installation{h.item}, false)
	require.NoError(t, err)
	registry.Entitlements = h.f.adapter
	_, signingKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	now := time.Now().UTC()
	h.f.server.PluginGateway = &plugins.Gateway{Registry: registry, SigningKey: signingKey, Issuer: "core", Now: func() time.Time { return now }}
	iam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body iamapi.InternalAuthorizeBody
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if h.secondActor == uuid.Nil {
			assert.Equal(t, h.grant.ActorID, body.UserId)
		} else {
			assert.Contains(t, []uuid.UUID{h.grant.ActorID, h.secondActor}, body.UserId)
		}
		if len(body.Checks) == 1 && body.Checks[0].Permission == "plugin."+h.item.ID+".modules" {
			assert.Equal(t, []iamapi.ResourcePermissionCheck{orgCheck("acme", "plugin."+h.item.ID+".modules")}, body.Checks)
		} else {
			assert.Equal(t, []iamapi.ResourcePermissionCheck{environmentCheck(h.grant.Intent.Environments[0].EnvironmentID, permission)}, body.Checks)
		}
		h.checks.Add(1)
		if h.rotateForward.Swap(false) {
			command := model.ExternalGrantCommand{OrgID: "acme", PluginID: h.item.ID, OperationID: h.grant.OperationID, ActorID: h.grant.ActorID, SourceOperation: "command", SourceDigest: strings.Repeat("d", 64), RequestID: "concurrent-pause", ExpectedRevision: h.grant.Revision, Action: "suspend", Reason: "Concurrent command"}
			suspended, err := model.ApplyExternalGrantCommand(r.Context(), h.f.db, command)
			if !assert.NoError(t, err) {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			command.Action, command.RequestID, command.ExpectedRevision = "activate", "concurrent-resume", suspended.Revision
			_, err = model.ApplyExternalGrantCommand(r.Context(), h.f.db, command)
			if !assert.NoError(t, err) {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			h.f.clock.Store(h.f.base + 21)
		}
		if h.denied.Load() {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(iam.Close)
	h.f.server.IamClient, err = iamapi.NewClientWithResponses(iam.URL)
	require.NoError(t, err)
	e, err := hecho.DefaultEchoServerWithValidation(&hecho.ValidatedServerConfig{AppName: "operation-entitlement-test", Logger: zap.NewNop(), OpenAPIRawSchema: MustDecodeOpenApiSpec()})
	require.NoError(t, err)
	e.Use(middleware.EchoCtxMiddleware)
	h.f.server.MapRoutes(e)
	h.actorProof = func(command string, actor uuid.UUID) PluginDelegationProof {
		parameters := map[string]string{"id": h.grant.OperationID.String(), "command": command}
		body := []byte(`{"reason":"Reviewed command"}`)
		requestID := uuid.NewString()
		path, err := h.item.Operations[0].RequestPath("acme", parameters, nil)
		require.NoError(t, err)
		assertion, err := plugincontext.Sign(signingKey, plugincontext.Claims{
			Version: 1, Issuer: "core", Audience: "plugin:" + h.item.ID, PluginID: h.item.ID, OrgID: "acme", ActorID: actor.String(),
			OperationID: "command", Method: "POST", Path: path, BodySHA256: plugincontext.Digest(body), RequestID: requestID,
			IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix(),
		}, now)
		require.NoError(t, err)
		return PluginDelegationProof{OperationId: "command", PathParameters: &parameters, Body: &body, IdempotencyKey: requestID, Assertion: assertion}
	}
	h.proof = func(command string) PluginDelegationProof { return h.actorProof(command, h.grant.ActorID) }
	call := func(actor string, operation uuid.UUID, suffix string, body any) *httptest.ResponseRecorder {
		encoded, err := json.Marshal(body)
		require.NoError(t, err)
		request := httptest.NewRequest(http.MethodPost, "/orgs/acme/plugins/"+h.item.ID+"/operations/"+operation.String()+suffix, bytes.NewReader(encoded))
		request.Header.Set("From", actor)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		e.ServeHTTP(response, request)
		return response
	}
	h.call = func(actor string, operation uuid.UUID, body any) *httptest.ResponseRecorder {
		return call(actor, operation, "/authorize", body)
	}
	h.moduleCall = func(actor string, operation uuid.UUID, body any) *httptest.ResponseRecorder {
		return call(actor, operation, "/modules", body)
	}
	h.deploymentCall = call
	_, descriptor, found := registry.Installation(t.Context(), "acme", h.item.ID)
	require.True(t, found)
	require.True(t, descriptor.Entitlement.Features["feature-a"].Forward)
	h.grant, err = model.ApplyExternalGrantCommand(t.Context(), h.f.db, model.ExternalGrantCommand{
		OrgID: "acme", PluginID: h.item.ID, OperationID: h.grant.OperationID, ActorID: h.grant.ActorID,
		SourceOperation: "command", SourceDigest: strings.Repeat("c", 64), RequestID: "pause-fixture", ExpectedRevision: h.grant.Revision, Action: "suspend", Reason: "Pause existing operation",
	})
	require.NoError(t, err)
	return h
}

func (h *pluginOperationEntitlementHTTP) activation(command string) PluginOperationAuthorizationRequest {
	return PluginOperationAuthorizationRequest{Action: "activate", ExpectedRevision: h.grant.Revision, Proof: h.proof(command), Reason: "Execute reviewed command"}
}

func TestPluginOperationRollbackActivationRetainsSafetyAfterEntitlementLoss(t *testing.T) {
	for _, state := range []string{"expired", "revoked"} {
		t.Run(state, func(t *testing.T) {
			h := newPluginOperationEntitlementHTTP(t)
			if state == "expired" {
				h.f.clock.Store(h.f.base + 21)
			} else {
				_, err := model.ImportPluginEntitlementRevocations(t.Context(), h.f.db, nil, h.f.signRevocation(t, h.f.grant.IssuanceID), "internal_system", sha256.Sum256([]byte("operator-revocation")))
				require.NoError(t, err)
			}
			body := h.activation("resume")
			response := h.call(h.item.ServiceUserID, h.grant.OperationID, body)
			require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
			body.Action, body.Proof = "prepare", h.proof("prepare")
			body.ExpectedRevision = 0
			intent, err := json.Marshal(h.grant.Intent)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(intent, &body.Intent))
			response = h.call(h.item.ServiceUserID, uuid.New(), body)
			require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
			body.Proof = h.proof("rollback")
			response = h.call(h.item.ServiceUserID, uuid.New(), body)
			require.Equal(t, http.StatusConflict, response.Code, "a safety source cannot prepare new intent: %s", response.Body.String())
			require.Contains(t, response.Body.String(), "plugin_not_entitled")
			before, err := model.GetExternalOperationGrant(t.Context(), h.f.db, "acme", h.item.ID, h.grant.OperationID)
			require.NoError(t, err)
			require.Equal(t, h.grant, before)

			body = h.activation("rollback")
			h.denied.Store(true)
			response = h.call(h.item.ServiceUserID, h.grant.OperationID, body)
			require.Equal(t, http.StatusForbidden, response.Code, "safety still requires current actor IAM: %s", response.Body.String())
			h.denied.Store(false)
			response = h.call(h.item.ServiceUserID, h.grant.OperationID, body)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			var activated PluginOperationGrant
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &activated))
			require.Equal(t, PluginOperationGrantState("active"), activated.State)
			require.Equal(t, h.grant.Revision+1, activated.Revision)
			require.Equal(t, int64(3), h.checks.Load(), "safety preparation and activation must retain environment IAM")
			h.denied.Store(true)
			response = h.call(h.item.ServiceUserID, h.grant.OperationID, body)
			require.Equal(t, http.StatusForbidden, response.Code, "receipt replay must recheck IAM")
			_, err = h.f.server.pluginEffectDatabase("acme", h.item, "external-deployment.admit", false).BeginTx(t.Context(), nil)
			requirePlatformEntitlementCode(t, err, "plugin_not_entitled")
		})
	}
}

func TestPluginOperationRollbackSafetyCannotBeForged(t *testing.T) {
	h := newPluginOperationEntitlementHTTP(t)
	h.f.clock.Store(h.f.base + 21)
	body := h.activation("resume")
	(*body.Proof.PathParameters)["command"] = "rollback"
	response := h.call(h.item.ServiceUserID, h.grant.OperationID, body)
	require.Equal(t, http.StatusForbidden, response.Code, "a resume signature cannot assert a rollback path: %s", response.Body.String())
	body = h.activation("cancel")
	response = h.call(h.item.ServiceUserID, h.grant.OperationID, body)
	require.Equal(t, http.StatusForbidden, response.Code, "a signed safety path cannot exchange an undelegated activation action: %s", response.Body.String())
	body = h.activation("rollback")
	encoded, err := json.Marshal(body)
	require.NoError(t, err)
	var forged map[string]any
	require.NoError(t, json.Unmarshal(encoded, &forged))
	forged["safety_activation"] = true
	response = h.call(h.item.ServiceUserID, h.grant.OperationID, forged)
	require.Equal(t, http.StatusBadRequest, response.Code, "the server-only classification is not an accepted request field: %s", response.Body.String())
	body = h.activation("rollback")
	body.Proof.OperationId = "unknown"
	response = h.call(h.item.ServiceUserID, h.grant.OperationID, body)
	require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
	body = h.activation("rollback")
	response = h.call(h.item.AdmissionCheckerID, h.grant.OperationID, body)
	require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
	response = h.call(uuid.NewString(), h.grant.OperationID, body)
	require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
	require.Zero(t, h.checks.Load(), "forged or wrong-principal requests cannot reach IAM or mutate grants")
	current, err := model.GetExternalOperationGrant(t.Context(), h.f.db, "acme", h.item.ID, h.grant.OperationID)
	require.NoError(t, err)
	require.Equal(t, h.grant, current)
}

func (h *pluginOperationEntitlementHTTP) moduleRecovery(t *testing.T, retainReservation bool) int64 {
	t.Helper()
	intent := h.grant.Intent
	for _, action := range []string{"module.reserve", "module.release", "module.promote", "module.restore"} {
		intent.Actions = append(intent.Actions, action)
		h.item.ServiceGrants = append(h.item.ServiceGrants, plugins.ServiceGrant{Action: action, RequiredFeature: "feature-a", Permission: "plugin." + h.item.ID + ".modules", Scope: "organization"})
		h.item.Operations[0].ServiceActions = append(h.item.Operations[0].ServiceActions, action)
	}
	registry, err := plugins.NewInstallationRegistry([]plugins.Installation{h.item}, false)
	require.NoError(t, err)
	registry.Entitlements = h.f.adapter
	h.f.server.PluginGateway.Registry = registry
	command := model.ExternalGrantCommand{OrgID: "acme", PluginID: h.item.ID, OperationID: uuid.New(), ActorID: h.grant.ActorID, SourceOperation: "command", SourceDigest: strings.Repeat("e", 64), RequestID: "module-prepare", Action: "prepare", Reason: "Prepare module workflow", Intent: &intent}
	h.grant, err = model.ApplyExternalGrantCommand(t.Context(), h.f.db, command)
	require.NoError(t, err)
	command.Intent, command.Action, command.RequestID, command.ExpectedRevision = nil, "activate", "module-start", h.grant.Revision
	h.grant, err = model.ApplyExternalGrantCommand(t.Context(), h.f.db, command)
	require.NoError(t, err)
	var moduleFence int64
	if retainReservation {
		module := h.grant.Intent.Modules[0]
		reserve := model.ExternalModuleCommand{OrgID: "acme", PluginID: h.item.ID, OperationID: h.grant.OperationID, ActorID: h.grant.ActorID, RequestID: "initial-reserve", Action: "reserve", GrantFence: h.grant.Revision, Reason: "Retained forward reservation", Targets: []model.ExternalModuleTarget{{ModuleUUID: module, VersionUUID: h.grant.Intent.TargetVersions[module.String()]}}}
		receipt, err := model.ApplyExternalModuleOperation(t.Context(), h.f.db, reserve)
		require.NoError(t, err)
		reserve.Action, reserve.RequestID, reserve.ExpectedFence = "release", "completed-release", receipt.Fence
		receipt, err = model.ApplyExternalModuleOperation(t.Context(), h.f.db, reserve)
		require.NoError(t, err)
		moduleFence = receipt.Fence
	}
	command.Action, command.RequestID, command.ExpectedRevision = "release", "completed-grant-release", h.grant.Revision
	h.grant, err = model.ApplyExternalGrantCommand(t.Context(), h.f.db, command)
	require.NoError(t, err)
	return moduleFence
}

func (h *pluginOperationEntitlementHTTP) moduleRequest(action string, moduleFence int64) map[string]any {
	module := h.grant.Intent.Modules[0]
	return map[string]any{"action": action, "grant_fence": h.grant.Revision, "expected_fence": moduleFence, "request_id": uuid.NewString(), "reason": "Recover completed operation", "targets": []map[string]any{{"module_uuid": module, "version_uuid": h.grant.Intent.TargetVersions[module.String()], "expected_resource_version": 0}}}
}

func (h *pluginOperationEntitlementHTTP) activateRollback(t *testing.T) PluginOperationAuthorizationRequest {
	t.Helper()
	body := h.activation("rollback")
	response := h.call(h.item.ServiceUserID, h.grant.OperationID, body)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.NotContains(t, response.Body.String(), "safety_activation", "the retained classification is not an API field")
	var err error
	h.grant, err = model.GetExternalOperationGrant(t.Context(), h.f.db, "acme", h.item.ID, h.grant.OperationID)
	require.NoError(t, err)
	require.True(t, h.grant.SafetyActivation)
	return body
}

func TestPluginCompletedRollbackReacquiresOnlyRetainedModulesAfterEntitlementLoss(t *testing.T) {
	for _, state := range []string{"expired", "revoked"} {
		t.Run(state, func(t *testing.T) {
			h := newPluginOperationEntitlementHTTP(t)
			fence := h.moduleRecovery(t, true)
			if state == "expired" {
				h.f.clock.Store(h.f.base + 21)
			} else {
				_, err := model.ImportPluginEntitlementRevocations(t.Context(), h.f.db, nil, h.f.signRevocation(t, h.f.grant.IssuanceID), "internal_system", sha256.Sum256([]byte("module-revocation")))
				require.NoError(t, err)
			}
			response := h.moduleCall(h.item.ServiceUserID, h.grant.OperationID, h.moduleRequest("reserve", fence))
			require.Equal(t, http.StatusConflict, response.Code, "ordinary retained reservation remains forward: %s", response.Body.String())
			authorization := h.activateRollback(t)
			outside := h.moduleRequest("reserve", fence)
			outside["targets"].([]map[string]any)[0]["module_uuid"] = uuid.New()
			response = h.moduleCall(h.item.ServiceUserID, h.grant.OperationID, outside)
			require.Equal(t, http.StatusConflict, response.Code, "recovery cannot change frozen module membership: %s", response.Body.String())
			request := h.moduleRequest("reserve", fence)
			h.denied.Store(true)
			response = h.moduleCall(h.item.ServiceUserID, h.grant.OperationID, request)
			require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
			h.denied.Store(false)
			response = h.moduleCall(h.item.ServiceUserID, h.grant.OperationID, request)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			var receipt PluginModuleOperationReceipt
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &receipt))
			require.Equal(t, fence+1, receipt.Fence)
			response = h.moduleCall(h.item.ServiceUserID, h.grant.OperationID, request)
			require.Equal(t, http.StatusOK, response.Code, "lost reservation response must retain idempotency: %s", response.Body.String())
			response = h.call(h.item.ServiceUserID, h.grant.OperationID, authorization)
			require.Equal(t, http.StatusOK, response.Code, "activation replay retains immutable class: %s", response.Body.String())
			current, err := model.GetExternalOperationGrant(t.Context(), h.f.db, "acme", h.item.ID, h.grant.OperationID)
			require.NoError(t, err)
			require.True(t, current.SafetyActivation)
			response = h.moduleCall(h.item.ServiceUserID, h.grant.OperationID, h.moduleRequest("promote", receipt.Fence))
			require.Equal(t, http.StatusConflict, response.Code, "rollback reacquisition cannot promote new defaults: %s", response.Body.String())
		})
	}
}

func TestPluginRollbackReservationCannotCreateScopeOrReuseAnOldSafetyFence(t *testing.T) {
	t.Run("no retained reservation", func(t *testing.T) {
		h := newPluginOperationEntitlementHTTP(t)
		h.moduleRecovery(t, false)
		h.f.clock.Store(h.f.base + 21)
		h.activateRollback(t)
		for _, fence := range []int64{0, 1} {
			response := h.moduleCall(h.item.ServiceUserID, h.grant.OperationID, h.moduleRequest("reserve", fence))
			require.Equal(t, http.StatusConflict, response.Code, "safety cannot acquire initial module scope: %s", response.Body.String())
		}
	})
	t.Run("normal activation replaces safety class", func(t *testing.T) {
		h := newPluginOperationEntitlementHTTP(t)
		fence := h.moduleRecovery(t, true)
		authorization := h.activateRollback(t)
		request := h.moduleRequest("reserve", fence)
		h.rotateForward.Store(true)
		response := h.moduleCall(h.item.ServiceUserID, h.grant.OperationID, request)
		require.Equal(t, http.StatusConflict, response.Code, "transaction must reject a class/fence rotated after API preflight: %s", response.Body.String())
		current, err := model.GetExternalOperationGrant(t.Context(), h.f.db, "acme", h.item.ID, h.grant.OperationID)
		require.NoError(t, err)
		require.False(t, current.SafetyActivation)
		response = h.call(h.item.ServiceUserID, h.grant.OperationID, authorization)
		require.Equal(t, http.StatusOK, response.Code, "historical activation receipt replay is read-only: %s", response.Body.String())
		afterReplay, err := model.GetExternalOperationGrant(t.Context(), h.f.db, "acme", h.item.ID, h.grant.OperationID)
		require.NoError(t, err)
		require.Equal(t, current, afterReplay, "old safety receipt cannot reclassify a newer normal activation")
		request["grant_fence"] = current.Revision
		response = h.moduleCall(h.item.ServiceUserID, h.grant.OperationID, request)
		require.Equal(t, http.StatusConflict, response.Code, "current normal activation cannot reserve after expiry: %s", response.Body.String())
		response = h.call(h.item.ServiceUserID, h.grant.OperationID, h.activation("resume"))
		require.Equal(t, http.StatusForbidden, response.Code, "resume remains forward after expiry: %s", response.Body.String())
	})
	for _, absent := range []string{"legacy class", "activation receipt", "reservation evidence"} {
		t.Run(absent, func(t *testing.T) {
			h := newPluginOperationEntitlementHTTP(t)
			fence := h.moduleRecovery(t, true)
			h.f.clock.Store(h.f.base + 21)
			h.activateRollback(t)
			tx, err := h.f.db.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			switch absent {
			case "legacy class":
				_, err = tx.ExecContext(t.Context(), `UPDATE external_operation_grant_commands SET response=response-'safety_activation' WHERE operation_id=$1 AND response->>'revision'=$2`, h.grant.OperationID, strconv.FormatInt(h.grant.Revision, 10))
			case "activation receipt":
				_, err = tx.ExecContext(t.Context(), `DELETE FROM external_operation_grant_commands WHERE operation_id=$1 AND response->>'revision'=$2`, h.grant.OperationID, strconv.FormatInt(h.grant.Revision, 10))
			case "reservation evidence":
				_, err = tx.ExecContext(t.Context(), `DELETE FROM external_module_operation_receipts WHERE operation_id=$1 AND action='reserve'`, h.grant.OperationID)
			}
			require.NoError(t, err)
			require.NoError(t, tx.Commit())
			response := h.moduleCall(h.item.ServiceUserID, h.grant.OperationID, h.moduleRequest("reserve", fence))
			require.Equal(t, http.StatusConflict, response.Code, "missing retained authority must fail closed: %s", response.Body.String())
		})
	}
}

func TestPluginSafetyMetadataPreservesLegacyRequestReceiptReplay(t *testing.T) {
	t.Run("old activation receipt never gains safety authority", func(t *testing.T) {
		h := newPluginOperationEntitlementHTTP(t)
		fence := h.moduleRecovery(t, true)
		body := h.activation("rollback")
		// The previous Core version hashed exactly these wire command fields,
		// without the newly introduced internal classification.
		legacy := model.ExternalGrantCommand{OrgID: "acme", PluginID: h.item.ID, OperationID: h.grant.OperationID, ActorID: h.grant.ActorID, SourceOperation: body.Proof.OperationId, SourceDigest: plugincontext.Digest(*body.Proof.Body), RequestID: body.Proof.IdempotencyKey, ExpectedRevision: body.ExpectedRevision, Action: string(body.Action), Reason: body.Reason}
		old, err := model.ApplyExternalGrantCommand(t.Context(), h.f.db, legacy)
		require.NoError(t, err)
		require.False(t, old.SafetyActivation)
		h.f.clock.Store(h.f.base + 21)
		response := h.call(h.item.ServiceUserID, h.grant.OperationID, body)
		require.Equal(t, http.StatusOK, response.Code, "adding internal class must not change old request hashes: %s", response.Body.String())
		current, err := model.GetExternalOperationGrant(t.Context(), h.f.db, "acme", h.item.ID, h.grant.OperationID)
		require.NoError(t, err)
		require.Equal(t, old, current)
		require.False(t, current.SafetyActivation, "receipt replay cannot upgrade legacy authority")
		h.grant = current
		response = h.moduleCall(h.item.ServiceUserID, h.grant.OperationID, h.moduleRequest("reserve", fence))
		require.Equal(t, http.StatusConflict, response.Code, response.Body.String())
	})
	t.Run("old module receipt keeps its exact request hash", func(t *testing.T) {
		h := newPluginOperationEntitlementHTTP(t)
		fence := h.moduleRecovery(t, true)
		h.activateRollback(t)
		body := h.moduleRequest("reserve", fence)
		module := h.grant.Intent.Modules[0]
		legacy := model.ExternalModuleCommand{OrgID: "acme", PluginID: h.item.ID, OperationID: h.grant.OperationID, ActorID: h.grant.ActorID, RequestID: body["request_id"].(string), Action: "reserve", ExpectedFence: fence, GrantFence: h.grant.Revision, Reason: body["reason"].(string), Targets: []model.ExternalModuleTarget{{ModuleUUID: module, VersionUUID: h.grant.Intent.TargetVersions[module.String()]}}}
		old, err := model.ApplyExternalModuleOperation(t.Context(), h.f.db, legacy)
		require.NoError(t, err)
		h.f.clock.Store(h.f.base + 21)
		response := h.moduleCall(h.item.ServiceUserID, h.grant.OperationID, body)
		require.Equal(t, http.StatusOK, response.Code, "classification must not change old module request hashes: %s", response.Body.String())
		var replay PluginModuleOperationReceipt
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &replay))
		require.Equal(t, old.Fence, replay.Fence)
		h.denied.Store(true)
		response = h.moduleCall(h.item.ServiceUserID, h.grant.OperationID, body)
		require.Equal(t, http.StatusForbidden, response.Code, "legacy replay still requires current IAM: %s", response.Body.String())
	})
}

func (h *pluginOperationEntitlementHTTP) heldModules(t *testing.T) int64 {
	t.Helper()
	fence := h.moduleRecovery(t, true)
	response := h.call(h.item.ServiceUserID, h.grant.OperationID, h.activation("resume"))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var err error
	h.grant, err = model.GetExternalOperationGrant(t.Context(), h.f.db, "acme", h.item.ID, h.grant.OperationID)
	require.NoError(t, err)
	response = h.moduleCall(h.item.ServiceUserID, h.grant.OperationID, h.moduleRequest("reserve", fence))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var receipt PluginModuleOperationReceipt
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &receipt))
	return receipt.Fence
}

func TestPluginRetainedRollbackBindsFreshActorAndSettlesOriginalForwardDispatch(t *testing.T) {
	for _, state := range []string{"running", "entitlement-paused", "explicitly-paused", "awaiting-pinned"} {
		t.Run(state, func(t *testing.T) {
			h := newPluginOperationEntitlementHTTP(t)
			fence := h.heldModules(t)
			before := *h.grant
			environment := h.grant.Intent.Environments[0]
			forward := model.ExternalDispatch{OrgID: "acme", PluginID: h.item.ID, OperationID: h.grant.OperationID, ActorID: h.grant.ActorID, Fence: h.grant.Revision, Action: "external-deployment.admit", DispatchID: "admitted-before-rollback", ProjectUUID: environment.ProjectID, EnvironmentUUID: environment.EnvironmentID, RequestDigest: strings.Repeat("f", 64), TargetVersions: h.grant.Intent.TargetVersions}
			require.NoError(t, model.RecordExternalDispatch(t.Context(), h.f.server.pluginEffectDatabase("acme", h.item, forward.Action, false), forward))
			if state == "awaiting-pinned" {
				response := h.moduleCall(h.item.ServiceUserID, h.grant.OperationID, h.moduleRequest("release", fence))
				require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			}
			if state == "explicitly-paused" {
				body := h.activation("pause")
				body.Action = "suspend"
				response := h.call(h.item.ServiceUserID, h.grant.OperationID, body)
				require.Equal(t, http.StatusOK, response.Code, response.Body.String())
				var err error
				h.grant, err = model.GetExternalOperationGrant(t.Context(), h.f.db, "acme", h.item.ID, h.grant.OperationID)
				require.NoError(t, err)
			}
			if state != "running" {
				h.f.clock.Store(h.f.base + 21)
			}
			h.secondActor = uuid.New()
			body := h.activation("rollback")
			body.Proof = h.actorProof("rollback", h.secondActor)
			response := h.call(h.item.ServiceUserID, h.grant.OperationID, body)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			var err error
			h.grant, err = model.GetExternalOperationGrant(t.Context(), h.f.db, "acme", h.item.ID, h.grant.OperationID)
			require.NoError(t, err)
			require.True(t, h.grant.SafetyActivation)
			require.Equal(t, h.secondActor, h.grant.ActorID, "fresh operator becomes current Core controller")
			require.Equal(t, body.ExpectedRevision+1, h.grant.Revision)
			reserve := h.moduleRequest("reserve", fence)
			response = h.moduleCall(h.item.ServiceUserID, h.grant.OperationID, reserve)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			var retained PluginModuleOperationReceipt
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &retained))
			if state == "awaiting-pinned" {
				require.Equal(t, fence+1, retained.Fence, "released lease is reacquired with a new fence")
			} else {
				require.Equal(t, fence, retained.Fence, "held lease retains its original fence")
			}
			response = h.moduleCall(h.item.ServiceUserID, h.grant.OperationID, reserve)
			require.Equal(t, http.StatusOK, response.Code, "ensure-held is restart-safe: %s", response.Body.String())
			response = h.call(h.item.ServiceUserID, h.grant.OperationID, body)
			require.Equal(t, http.StatusOK, response.Code, "activation response loss recovers the same rotated fence: %s", response.Body.String())
			check := forward
			check.TargetVersions = nil
			require.Error(t, model.CheckExternalDispatch(t.Context(), h.f.db, check), "old forward callback cannot use previous actor/fence")
			completion := model.ExternalDispatchCompletion{DeploymentID: uuid.New(), EnvironmentUUID: environment.EnvironmentID, ProjectUUID: environment.ProjectID, RequestDigest: forward.RequestDigest, Status: "succeeded"}
			require.NoError(t, model.CompleteExternalDispatch(t.Context(), h.f.server.pluginEffectDatabase("acme", h.item, "external-deployment.read", true), "acme", h.item.ID, h.grant.OperationID, forward.DispatchID, completion), "admitted forward receipt still settles after actor/fence rotation")
			require.NoError(t, model.CompleteExternalDispatch(t.Context(), h.f.server.pluginEffectDatabase("acme", h.item, "external-deployment.read", true), "acme", h.item.ID, h.grant.OperationID, forward.DispatchID, completion))
			changed := completion
			changed.RequestDigest = strings.Repeat("a", 64)
			require.Error(t, model.CompleteExternalDispatch(t.Context(), h.f.db, "acme", h.item.ID, h.grant.OperationID, forward.DispatchID, changed), "original admitted receipt remains immutable")
			reservations, err := h.f.db.ListModuleOperationReservations(t.Context(), nil, "acme", before.Intent.Modules[0], false)
			require.NoError(t, err)
			require.Len(t, reservations, 1)
			if state == "awaiting-pinned" {
				require.Equal(t, h.secondActor, reservations[0].AcquiredBy, "reacquisition records the new controller")
			} else {
				require.Equal(t, before.ActorID, reservations[0].AcquiredBy, "retention cannot fabricate a new acquisition")
			}
			if state == "running" {
				response = h.call(h.item.ServiceUserID, h.grant.OperationID, h.activation("resume"))
				require.Equal(t, http.StatusOK, response.Code, "current forward entitlement permits fresh reauthorization: %s", response.Body.String())
				current, err := model.GetExternalOperationGrant(t.Context(), h.f.db, "acme", h.item.ID, h.grant.OperationID)
				require.NoError(t, err)
				require.Equal(t, h.grant.Revision+1, current.Revision)
				require.False(t, current.SafetyActivation, "forward activation cannot retain old recovery authority")
				h.grant = current
				response = h.moduleCall(h.item.ServiceUserID, h.grant.OperationID, h.moduleRequest("reserve", fence))
				require.Equal(t, http.StatusConflict, response.Code, "normal activation cannot ensure-held a safety lease: %s", response.Body.String())
			}
		})
	}
}

func TestPluginActiveReauthorizationRequiresCurrentForwardEntitlementAndIAM(t *testing.T) {
	for _, state := range []string{"full", "expired", "revoked"} {
		t.Run(state, func(t *testing.T) {
			h := newPluginOperationEntitlementHTTP(t)
			h.heldModules(t)
			before := *h.grant
			environment := h.grant.Intent.Environments[0]
			forward := model.ExternalDispatch{OrgID: "acme", PluginID: h.item.ID, OperationID: h.grant.OperationID, ActorID: h.grant.ActorID, Fence: h.grant.Revision, Action: "external-deployment.admit", DispatchID: "admitted-before-reauthorization", ProjectUUID: environment.ProjectID, EnvironmentUUID: environment.EnvironmentID, RequestDigest: strings.Repeat("b", 64), TargetVersions: h.grant.Intent.TargetVersions}
			require.NoError(t, model.RecordExternalDispatch(t.Context(), h.f.server.pluginEffectDatabase("acme", h.item, forward.Action, false), forward))
			h.secondActor = uuid.New()
			body := h.activation("resume")
			body.Proof = h.actorProof("resume", h.secondActor)
			switch state {
			case "expired":
				h.f.clock.Store(h.f.base + 21)
			case "revoked":
				_, err := model.ImportPluginEntitlementRevocations(t.Context(), h.f.db, nil, h.f.signRevocation(t, h.f.grant.IssuanceID), "internal_system", sha256.Sum256([]byte("active-reauthorization-revocation")))
				require.NoError(t, err)
			}
			h.denied.Store(true)
			response := h.call(h.item.ServiceUserID, h.grant.OperationID, body)
			require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
			h.denied.Store(false)
			response = h.call(h.item.ServiceUserID, h.grant.OperationID, body)
			current, err := model.GetExternalOperationGrant(t.Context(), h.f.db, "acme", h.item.ID, h.grant.OperationID)
			require.NoError(t, err)
			if state != "full" {
				require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
				require.Equal(t, before, *current, "denied reauthorization cannot mutate actor, fence, intent or class")
				return
			}
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			require.Equal(t, before.Revision+1, current.Revision)
			require.Equal(t, h.secondActor, current.ActorID)
			require.Equal(t, before.Intent, current.Intent)
			require.Equal(t, before.BaselineDefaults, current.BaselineDefaults)
			require.False(t, current.SafetyActivation)
			check := forward
			check.TargetVersions = nil
			require.Error(t, model.CheckExternalDispatch(t.Context(), h.f.db, check), "previous actor and fence cannot authorize forward callbacks")
			completion := model.ExternalDispatchCompletion{DeploymentID: uuid.New(), EnvironmentUUID: environment.EnvironmentID, ProjectUUID: environment.ProjectID, RequestDigest: forward.RequestDigest, Status: "succeeded"}
			require.NoError(t, model.CompleteExternalDispatch(t.Context(), h.f.server.pluginEffectDatabase("acme", h.item, "external-deployment.read", true), "acme", h.item.ID, current.OperationID, forward.DispatchID, completion), "fresh normal reauthorization still permits exact already-admitted completion")
			response = h.call(h.item.ServiceUserID, h.grant.OperationID, body)
			require.Equal(t, http.StatusOK, response.Code, "reauthorization replay preserves the original new fence: %s", response.Body.String())
			stale := body
			stale.Proof = h.actorProof("resume", h.secondActor)
			response = h.call(h.item.ServiceUserID, h.grant.OperationID, stale)
			require.Equal(t, http.StatusConflict, response.Code, "a new request cannot reuse the previous fence: %s", response.Body.String())
		})
	}
}

func TestPluginSafetyEnsureHeldRejectsMissingOrForeignReservation(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(strconv.FormatBool(foreign), func(t *testing.T) {
			h := newPluginOperationEntitlementHTTP(t)
			fence := h.heldModules(t)
			h.f.clock.Store(h.f.base + 21)
			h.activateRollback(t)
			module := h.grant.Intent.Modules[0]
			reservations, err := h.f.db.ListModuleOperationReservations(t.Context(), nil, "acme", module, false)
			require.NoError(t, err)
			require.Len(t, reservations, 1)
			tx, err := h.f.db.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			_, err = h.f.db.ReleaseModuleOperationReservation(t.Context(), tx, "acme", reservations[0].ID, reservations[0].ResourceVersion, h.grant.ActorID, "External operator cleanup")
			require.NoError(t, err)
			if foreign {
				_, err = h.f.db.AcquireModuleOperationReservation(t.Context(), tx, "acme", module.String(), "dev.example.other", uuid.New(), "foreign-operation", "External operation", uuid.New())
				require.NoError(t, err)
			}
			require.NoError(t, tx.Commit())
			response := h.moduleCall(h.item.ServiceUserID, h.grant.OperationID, h.moduleRequest("reserve", fence))
			require.Equal(t, http.StatusConflict, response.Code, "stale active metadata cannot mint a replacement lease: %s", response.Body.String())
			current, err := h.f.db.ListModuleOperationReservations(t.Context(), nil, "acme", module, false)
			require.NoError(t, err)
			if foreign {
				require.Len(t, current, 1)
				require.Equal(t, "dev.example.other", current[0].Namespace)
			} else {
				require.Empty(t, current)
			}
		})
	}
}

func TestPluginConcurrentSafetyAndNormalActivationPreserveSingleCurrentActorAndFence(t *testing.T) {
	h := newPluginOperationEntitlementHTTP(t)
	fence := h.heldModules(t)
	h.secondActor = uuid.New()
	normal, safety := h.activation("resume"), h.activation("rollback")
	safety.Proof = h.actorProof("rollback", h.secondActor)
	responses := make([]*httptest.ResponseRecorder, 2)
	var group sync.WaitGroup
	for index, body := range []PluginOperationAuthorizationRequest{normal, safety} {
		group.Go(func() { responses[index] = h.call(h.item.ServiceUserID, h.grant.OperationID, body) })
	}
	group.Wait()
	statuses := []int{responses[0].Code, responses[1].Code}
	require.ElementsMatch(t, []int{http.StatusOK, http.StatusConflict}, statuses, "%s; %s", responses[0].Body.String(), responses[1].Body.String())
	current, err := model.GetExternalOperationGrant(t.Context(), h.f.db, "acme", h.item.ID, h.grant.OperationID)
	require.NoError(t, err)
	require.Equal(t, h.grant.Revision+1, current.Revision)
	if responses[1].Code == http.StatusOK {
		require.True(t, current.SafetyActivation)
		require.Equal(t, h.secondActor, current.ActorID)
	} else {
		require.False(t, current.SafetyActivation)
		require.Equal(t, h.grant.ActorID, current.ActorID)
	}
	h.grant = current
	h.f.clock.Store(h.f.base + 21)
	response := h.moduleCall(h.item.ServiceUserID, h.grant.OperationID, h.moduleRequest("reserve", fence))
	if current.SafetyActivation {
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	} else {
		require.Equal(t, http.StatusConflict, response.Code, response.Body.String())
	}
}
