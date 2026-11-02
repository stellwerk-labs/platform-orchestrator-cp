//go:build integration

package api

import (
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	observation "github.com/stellwerk-labs/platform-orchestrator-cp/internal/observationapi"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/plugincontext"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func expiredObservationFixture(t *testing.T) *observationDispatchFixture {
	t.Helper()
	f := newObservationDispatchFixture(t)
	f.body.Occurrence = time.Now().UTC().Add(-2 * time.Minute)
	f.body.AdmissionDeadline = f.body.Occurrence.Add(time.Minute)
	return f
}
func sealObservation(t *testing.T, f *observationDispatchFixture, server *Server, body observation.ObservationRequest) (observation.ObservationResolution, error) {
	t.Helper()
	r, e := server.ResolveObservationRequest(f.ctx(t), observation.ResolveObservationRequestRequestObject{OrgId: "acme", PluginId: f.item.ID, RequestId: body.RequestId, Body: &body})
	if e != nil {
		return observation.ObservationResolution{}, e
	}
	return observation.ObservationResolution(r.(observation.ResolveObservationRequest200JSONResponse)), nil
}
func seedObservationIntent(t *testing.T, f *observationDispatchFixture) {
	t.Helper()
	encoded, e := json.Marshal(f.body)
	require.NoError(t, e)
	tx, e := f.db.BeginTx(t.Context(), nil)
	require.NoError(t, e)
	require.NoError(t, model.RecordObservationRequestTx(t.Context(), tx, "acme", f.item.ID, model.ObservationRequestRecord{ActorID: f.actor, GrantID: f.body.GrantId, GrantRevision: f.body.GrantRevision, ProjectUUID: f.body.ProjectUuid, EnvironmentUUID: f.body.EnvironmentUuid, RequestID: f.body.RequestId, RequestDigest: plugincontext.Digest(encoded), Body: encoded, Deadline: f.body.AdmissionDeadline}))
	require.NoError(t, tx.Commit())
}
func TestObservationMissingIntentSealsHistoricalScopeWithoutInventingActor(t *testing.T) {
	f := expiredObservationFixture(t)
	f.admin.denied.Store(true)
	tx, e := f.db.BeginTx(t.Context(), nil)
	require.NoError(t, e)
	_, e = tx.ExecContext(t.Context(), `UPDATE environment_observation_grants SET revision=2,state='revoked',targets='[]' WHERE org_id='acme' AND plugin_id=$1`, f.item.ID)
	require.NoError(t, e)
	require.NoError(t, tx.Commit())
	result, e := sealObservation(t, f, f.server, f.body)
	require.NoError(t, e)
	require.EqualValues(t, "not_started", result.Disposition)
	require.Nil(t, result.Receipt)
	replay, e := sealObservation(t, f, f.server, f.body)
	require.NoError(t, e)
	require.Equal(t, result, replay)
	_, e = f.server.TryStartObservation(f.ctx(t), observation.TryStartObservationRequestObject{OrgId: "acme", PluginId: f.item.ID, Body: &f.body})
	require.ErrorContains(t, e, "plugin_operation_conflict")
	tx, e = f.db.BeginTx(t.Context(), nil)
	require.NoError(t, e)
	closure, e := model.ReadObservationClosureTx(t.Context(), tx, "acme", f.item.ID, model.ObservationAdmission, f.body.RequestId)
	require.NoError(t, e)
	require.Nil(t, closure.OriginalActorID)
	require.Equal(t, f.item.ServiceUserID, closure.ServiceActorID.String())
	require.Equal(t, "not_started", closure.State)
	require.NoError(t, tx.Rollback())
	changed := f.body
	changed.EnvId = "changed"
	_, e = sealObservation(t, f, f.server, changed)
	require.ErrorContains(t, e, "plugin_operation_conflict")
	unauthorized := f.body
	unauthorized.RequestId = uuid.New()
	unauthorized.EnvironmentUuid = uuid.New()
	_, e = sealObservation(t, f, f.server, unauthorized)
	require.ErrorContains(t, e, "plugin_service_forbidden")
	unauthorized = f.body
	unauthorized.RequestId = uuid.New()
	unauthorized.GrantRevision = 2
	_, e = sealObservation(t, f, f.server, unauthorized)
	require.ErrorContains(t, e, "plugin_service_forbidden")
	_, e = f.server.ResolveObservationRequest(f.ctx(t), observation.ResolveObservationRequestRequestObject{OrgId: "foreign", PluginId: f.item.ID, RequestId: f.body.RequestId, Body: &f.body})
	require.Error(t, e)
	_, e = f.server.ResolveObservationRequest(f.ctx(t), observation.ResolveObservationRequestRequestObject{OrgId: "acme", PluginId: "dev.example.foreign", RequestId: f.body.RequestId, Body: &f.body})
	require.Error(t, e)
	early := f.body
	early.RequestId = uuid.New()
	early.Occurrence = time.Now().UTC()
	early.AdmissionDeadline = early.Occurrence.Add(time.Minute)
	_, e = sealObservation(t, f, f.server, early)
	require.ErrorContains(t, e, "plugin_request_invalid")
}
func TestObservationMissingRemediationSealsOnlyKnownBaselineWithoutProof(t *testing.T) {
	f := expiredObservationFixture(t)
	f.seedRun(t)
	request := f.redeploy(t)
	original := request
	request.Proof = nil
	f.admin.denied.Store(true)
	call := func(command observation.ObservationRedeployRequest) (observation.ObservationRedeployResolution, error) {
		r, e := f.server.ResolveObservationRedeployment(f.ctx(t), observation.ResolveObservationRedeploymentRequestObject{OrgId: "acme", PluginId: f.item.ID, RequestId: command.RequestId, Body: &observation.ObservationRedeployResolveRequest{RunId: f.run, Request: command}})
		if e != nil {
			return observation.ObservationRedeployResolution{}, e
		}
		return observation.ObservationRedeployResolution(r.(observation.ResolveObservationRedeployment200JSONResponse)), nil
	}
	result, e := call(request)
	require.NoError(t, e)
	require.EqualValues(t, "not_started", result.Disposition)
	require.Nil(t, result.Receipt)
	replay, e := call(request)
	require.NoError(t, e)
	require.Equal(t, result, replay)
	_, e = f.server.RedeployObservationBaseline(f.ctx(t), observation.RedeployObservationBaselineRequestObject{OrgId: "acme", PluginId: f.item.ID, RunId: f.run, Body: &original})
	require.ErrorContains(t, e, "plugin_operation_conflict")
	tx, e := f.db.BeginTx(t.Context(), nil)
	require.NoError(t, e)
	closure, e := model.ReadObservationClosureTx(t.Context(), tx, "acme", f.item.ID, model.ObservationRedeploymentAdmission, request.RequestId)
	require.NoError(t, e)
	require.Nil(t, closure.OriginalActorID)
	require.Equal(t, f.item.ServiceUserID, closure.ServiceActorID.String())
	require.NotContains(t, string(closure.Body), "proof")
	require.NoError(t, tx.Rollback())
	changed := request
	changed.RequestId = uuid.New()
	changed.BaselineId = uuid.New()
	_, e = call(changed)
	require.ErrorContains(t, e, "plugin_service_forbidden")
	_, e = call(original)
	require.ErrorContains(t, e, "plugin_request_invalid")
}

// The peer is a durable failure injector, not an actual DP or IAM authority.
// It verifies the real signed exact transport and reads committed CP closure
// state from another PostgreSQL connection before recording its own decision.
func TestObservationResolutionRetainsClosureAcrossLostPeerAcknowledgement(t *testing.T) {
	for _, remedial := range []bool{false, true} {
		for _, accepted := range []bool{false, true} {
			name := "observation"
			if remedial {
				name = "redeployment"
			}
			if accepted {
				name += "/accepted"
			} else {
				name += "/not-started"
			}
			t.Run(name, func(t *testing.T) {
				f := expiredObservationFixture(t)
				var command observation.ObservationRedeployRequest
				id := f.body.RequestId
				kind := model.ObservationAdmission
				if remedial {
					f.seedRun(t)
					command = f.redeploy(t)
					command.Proof = nil
					id = command.RequestId
					kind = model.ObservationRedeploymentAdmission
					encoded, e := json.Marshal(command)
					require.NoError(t, e)
					tx, e := f.db.BeginTx(t.Context(), nil)
					require.NoError(t, e)
					require.NoError(t, model.RecordObservationRedeploymentTx(t.Context(), tx, "acme", f.item.ID, model.ObservationRedeployment{RequestID: id, ObservationRequestID: f.body.RequestId, RunID: f.run, ActorID: f.actor, RequestDigest: plugincontext.Digest(encoded), Body: encoded}))
					require.NoError(t, tx.Commit())
				} else {
					seedObservationIntent(t, f)
				}
				var output any = observation.ObservationResolution{RequestId: id, Disposition: "not_started"}
				if remedial {
					output = observation.ObservationRedeployResolution{RequestId: id, Disposition: "not_started"}
				}
				if accepted {
					if remedial {
						receipt := observation.ObservationRedeployReceipt{RequestId: id, DeploymentId: uuid.New(), BaselineId: f.baseline, Status: "executing"}
						output = observation.ObservationRedeployResolution{RequestId: id, Disposition: "accepted", Receipt: &receipt}
					} else {
						receipt := f.receipt()
						output = observation.ObservationResolution{RequestId: id, Disposition: "accepted", Receipt: &receipt}
					}
				}
				response, e := json.Marshal(output)
				require.NoError(t, e)
				var calls atomic.Int32
				peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					action := "observation.request.resolve"
					if remedial {
						action = "observation.redeploy.resolve"
					}
					body, e := io.ReadAll(r.Body)
					if !assert.NoError(t, e) {
						w.WriteHeader(500)
						return
					}
					claims, e := plugincontext.Verify(f.key.Public().(ed25519.PublicKey), r.Header.Get(plugincontext.Header), plugincontext.Request{Issuer: "core", Audience: "platform-orchestrator-dp", PluginID: f.item.ID, OrgID: "acme", OperationID: action, Method: r.Method, Path: r.URL.EscapedPath(), Body: body, IdempotencyKey: r.Header.Get("Idempotency-Key")}, time.Now().UTC())
					if !assert.NoError(t, e) {
						w.WriteHeader(401)
						return
					}
					assert.Equal(t, f.actor.String(), claims.ActorID)
					assert.Equal(t, id.String(), claims.RequestID)
					if remedial {
						var input observation.ObservationRedeployResolveRequest
						assert.NoError(t, json.Unmarshal(body, &input))
						assert.Equal(t, f.run, input.RunId)
						assert.Nil(t, input.Request.Proof)
					} else {
						var input observation.ObservationRequest
						assert.NoError(t, json.Unmarshal(body, &input))
						assert.Equal(t, f.body, input)
					}
					tx, e := f.db.BeginTx(r.Context(), nil)
					if !assert.NoError(t, e) {
						w.WriteHeader(500)
						return
					}
					defer func() { _ = tx.Rollback() }()
					var state string
					var original uuid.UUID
					if !assert.NoError(t, tx.QueryRowContext(r.Context(), `SELECT state,original_actor_id FROM environment_observation_closures WHERE org_id='acme' AND plugin_id=$1 AND kind=$2 AND request_id=$3`, f.item.ID, kind, id).Scan(&state, &original)) {
						w.WriteHeader(500)
						return
					}
					assert.Equal(t, "close_requested", state)
					assert.Equal(t, f.actor, original)
					_, e = tx.ExecContext(r.Context(), `INSERT INTO observation_fault_effects VALUES($1,$2) ON CONFLICT DO NOTHING`, id, response)
					if !assert.NoError(t, e) {
						w.WriteHeader(500)
						return
					}
					if !assert.NoError(t, tx.Commit()) {
						w.WriteHeader(500)
						return
					}
					if calls.Add(1) == 1 {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(503)
						_, _ = w.Write([]byte(`{"code":"observation_unavailable"}`))
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(response)
				}))
				t.Cleanup(peer.Close)
				port, e := plugins.NewDataPlanePort(peer.URL, f.server.PluginGateway, true)
				require.NoError(t, e)
				f.server.PluginDataPlane = port
				f.admin.denied.Store(true)
				call := func(server *Server) (any, error) {
					if remedial {
						return server.ResolveObservationRedeployment(f.ctx(t), observation.ResolveObservationRedeploymentRequestObject{OrgId: "acme", PluginId: f.item.ID, RequestId: id, Body: &observation.ObservationRedeployResolveRequest{RunId: f.run, Request: command}})
					}
					return server.ResolveObservationRequest(f.ctx(t), observation.ResolveObservationRequestRequestObject{OrgId: "acme", PluginId: f.item.ID, RequestId: id, Body: &f.body})
				}
				_, e = call(f.server)
				require.Error(t, e)
				tx, e := f.db.BeginTx(t.Context(), nil)
				require.NoError(t, e)
				require.Error(t, model.CheckObservationAdmissionOpenTx(t.Context(), tx, "acme", f.item.ID, kind, id))
				require.NoError(t, tx.Rollback())
				recovered := &Server{Database: f.db, Logger: f.server.Logger, PluginGateway: f.server.PluginGateway, PluginDataPlane: port}
				result, e := call(recovered)
				require.NoError(t, e)
				encoded, e := json.Marshal(result)
				require.NoError(t, e)
				require.JSONEq(t, string(response), string(encoded))
				replay, e := call(recovered)
				require.NoError(t, e)
				encoded, e = json.Marshal(replay)
				require.NoError(t, e)
				require.JSONEq(t, string(response), string(encoded))
				require.Equal(t, int32(2), calls.Load(), "committed resolution replays without another remote call")
				tx, e = f.db.BeginTx(t.Context(), nil)
				require.NoError(t, e)
				closure, e := model.ReadObservationClosureTx(t.Context(), tx, "acme", f.item.ID, kind, id)
				require.NoError(t, e)
				require.Equal(t, strings.Split(name, "/")[1], strings.ReplaceAll(closure.State, "_", "-"))
				require.NoError(t, tx.Rollback())
			})
		}
	}
}

func TestObservationSealBetweenIntentAndEffectPreventsForward(t *testing.T) {
	f := expiredObservationFixture(t)
	var forwards atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/resolve") {
			forwards.Add(1)
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(observation.ObservationResolution{RequestId: f.body.RequestId, Disposition: "not_started"})
	}))
	t.Cleanup(peer.Close)
	port, e := plugins.NewDataPlanePort(peer.URL, f.server.PluginGateway, true)
	require.NoError(t, e)
	f.server.PluginDataPlane = port
	resolver := &Server{Database: f.db, Logger: f.server.Logger, PluginGateway: f.server.PluginGateway, PluginDataPlane: port}
	f.server.Database = &observationCommitBarrier{Databaser: f.db, after: func() { _, e := sealObservation(t, f, resolver, f.body); require.NoError(t, e) }}
	_, e = f.server.TryStartObservation(f.ctx(t), observation.TryStartObservationRequestObject{OrgId: "acme", PluginId: f.item.ID, Body: &f.body})
	require.ErrorContains(t, e, "plugin_operation_conflict")
	require.Zero(t, forwards.Load())
}
