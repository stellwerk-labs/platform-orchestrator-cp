//go:build integration

package api

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/hecho"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	observation "github.com/stellwerk-labs/platform-orchestrator-cp/internal/observationapi"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/plugincontext"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// These fault tests use real PostgreSQL and the real signed CP transport. The
// peer durably records an effect, but is not a DP/Runner or canonical IAM test.
type observationFaultAdmin struct{ denied atomic.Bool }

func (a *observationFaultAdmin) IsOrganizationAdmin(context.Context, string, string) (bool, error) {
	return !a.denied.Load(), nil
}

type observationDispatchFixture struct {
	db       model.Databaser
	server   *Server
	item     plugins.Installation
	admin    *observationFaultAdmin
	body     observation.ObservationRequest
	actor    uuid.UUID
	key      ed25519.PrivateKey
	run      uuid.UUID
	baseline uuid.UUID
}

func newObservationDispatchFixture(t *testing.T) *observationDispatchFixture {
	t.Helper()
	db, target := pluginDeploymentBoundaryDatabase(t)
	f := &observationDispatchFixture{db: db, admin: new(observationFaultAdmin), actor: target.ActorID, run: uuid.New(), baseline: uuid.New()}
	f.item = plugins.Installation{ID: "dev.example.observer", DisplayName: "Observer", Version: "1.0.0", Backend: "https://observer.example.test", Organizations: map[string]plugins.AvailabilityState{"acme": plugins.StateAvailable}, ServiceUserID: userid.NewServiceUserTokenId().String(), AdmissionCheckerID: userid.NewServiceUserTokenId().String(), Operations: []plugins.Operation{{ID: "redeploy", Method: "POST", Path: "/orgs/{orgId}/redeploy", Mutating: true, RequireOrgAdmin: true, ServiceActions: []string{"observation.redeploy"}}}}
	for _, action := range []string{"observation.try-start", "observation.request.get", "observation.get", "observation.redeploy", "observation.redeploy.get", "observation.request.resolve", "observation.redeploy.resolve"} {
		f.item.ServiceGrants = append(f.item.ServiceGrants, plugins.ServiceGrant{Action: action, Permission: "plugin.dev.example.observer.manage", Scope: "organization"})
	}
	registry, item := installedRegistry(t, db, f.item)
	f.item = item
	_, key, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	f.key = key
	f.server = &Server{Database: db, Logger: zap.NewNop(), IamClient: operatorIAM(t).client, PluginGateway: &plugins.Gateway{Registry: registry, SigningKey: key, Issuer: "core", OrganizationAdmins: f.admin}}
	now := time.Now().UTC()
	command := model.ObservationGrantCommand{OrgID: "acme", PluginID: item.ID, GrantID: uuid.New(), ActorID: f.actor, SourceOperation: "configure", SourceDigest: strings.Repeat("a", 64), RequestID: uuid.NewString(), Action: "replace", Reason: "Reviewed scope", Targets: target.Intent.Environments}
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = model.ApplyObservationGrantCommandTx(t.Context(), tx, command, now)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	env, err := db.GetEnvironmentByUuid(t.Context(), nil, "acme", command.Targets[0].EnvironmentID, model.GetModeDefault)
	require.NoError(t, err)
	f.body = observation.ObservationRequest{GrantId: command.GrantID, GrantRevision: 1, RequestId: uuid.New(), ProjectUuid: env.ProjectUuid, EnvironmentUuid: env.Uuid, ProjectId: env.ProjectId, EnvId: env.Id, Occurrence: now, AdmissionDeadline: now.Add(time.Minute)}
	tx, err = db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `CREATE TABLE observation_fault_effects (request_id uuid PRIMARY KEY, receipt jsonb NOT NULL)`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	return f
}
func (f *observationDispatchFixture) ctx(t *testing.T) context.Context {
	t.Helper()
	return context.WithValue(t.Context(), hecho.ContextKeyUserID, f.item.ServiceUserID)
}
func (f *observationDispatchFixture) receipt() observation.ObservationReceipt {
	fingerprint := "sha256:" + strings.Repeat("a", 64)
	return observation.ObservationReceipt{RequestId: f.body.RequestId, Disposition: "accepted", Reason: "accepted", RecordedAt: time.Now().UTC(), RunId: &f.run, BaselineId: &f.baseline, BaselineFingerprint: &fingerprint}
}
func (f *observationDispatchFixture) seedRun(t *testing.T) {
	t.Helper()
	body, err := json.Marshal(f.body)
	require.NoError(t, err)
	receipt, err := json.Marshal(f.receipt())
	require.NoError(t, err)
	tx, err := f.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	require.NoError(t, model.RecordObservationRequestTx(t.Context(), tx, "acme", f.item.ID, model.ObservationRequestRecord{ActorID: f.actor, GrantID: f.body.GrantId, GrantRevision: 1, ProjectUUID: f.body.ProjectUuid, EnvironmentUUID: f.body.EnvironmentUuid, RequestID: f.body.RequestId, RequestDigest: plugincontext.Digest(body), Body: body, Deadline: f.body.AdmissionDeadline}))
	require.NoError(t, model.CompleteObservationRequestTx(t.Context(), tx, "acme", f.item.ID, f.body.RequestId, 200, receipt, &f.run))
	require.NoError(t, tx.Commit())
}
func (f *observationDispatchFixture) redeploy(t *testing.T) observation.ObservationRedeployRequest {
	t.Helper()
	now := time.Now().UTC()
	id := uuid.New()
	source := []byte(`{"intent":"reviewed"}`)
	assertion, err := plugincontext.Sign(f.key, plugincontext.Claims{Version: 1, Issuer: "core", Audience: "plugin:" + f.item.ID, PluginID: f.item.ID, OrgID: "acme", ActorID: f.actor.String(), OperationID: "redeploy", Method: "POST", Path: "/orgs/acme/redeploy", BodySHA256: plugincontext.Digest(source), RequestID: id.String(), IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix()}, now)
	require.NoError(t, err)
	return observation.ObservationRedeployRequest{RequestId: id, ProjectUuid: f.body.ProjectUuid, EnvironmentUuid: f.body.EnvironmentUuid, ProjectId: f.body.ProjectId, EnvId: f.body.EnvId, BaselineId: f.baseline, BaselineFingerprint: "sha256:" + strings.Repeat("a", 64), Proof: &observation.ObservationDelegationProof{OperationId: "redeploy", IdempotencyKey: id.String(), Assertion: assertion, Body: &source}}
}

func TestObservationRoutingIntentSurvivesCancelledRemoteAcknowledgement(t *testing.T) {
	for _, remedial := range []bool{false, true} {
		name := "observation"
		if remedial {
			name = "redeployment"
		}
		t.Run(name, func(t *testing.T) {
			f := newObservationDispatchFixture(t)
			id := f.body.RequestId
			var command observation.ObservationRedeployRequest
			if remedial {
				f.seedRun(t)
				command = f.redeploy(t)
				id = command.RequestId
			}
			committed := make(chan struct{})
			var sends atomic.Int32
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if !assert.NoError(t, err) {
					w.WriteHeader(500)
					return
				}
				action := "observation.try-start"
				if remedial {
					action = "observation.redeploy"
				}
				if r.Method == http.MethodGet {
					action = "observation.request.get"
					if strings.HasSuffix(r.URL.Path, "/runs/"+f.run.String()) {
						action = "observation.get"
					}
					if remedial {
						action = "observation.redeploy.get"
					}
				}
				_, err = plugincontext.Verify(f.key.Public().(ed25519.PublicKey), r.Header.Get(plugincontext.Header), plugincontext.Request{Issuer: "core", Audience: "platform-orchestrator-dp", PluginID: f.item.ID, OrgID: "acme", OperationID: action, Method: r.Method, Path: r.URL.EscapedPath(), Body: body, IdempotencyKey: r.Header.Get("Idempotency-Key")}, time.Now().UTC())
				if !assert.NoError(t, err) {
					w.WriteHeader(401)
					return
				}
				tx, err := f.db.BeginTx(r.Context(), nil)
				if !assert.NoError(t, err) {
					w.WriteHeader(500)
					return
				}
				defer func() { _ = tx.Rollback() }()
				if r.Method == http.MethodPost {
					var count int
					query := `SELECT count(*) FROM environment_observation_requests WHERE org_id='acme' AND plugin_id=$1 AND request_id=$2`
					if remedial {
						query = `SELECT count(*) FROM environment_observation_redeployments WHERE org_id='acme' AND plugin_id=$1 AND request_id=$2`
					}
					if !assert.NoError(t, tx.QueryRowContext(r.Context(), query, f.item.ID, id).Scan(&count)) || !assert.Equal(t, 1, count, "routing must already be committed before remote effect") {
						w.WriteHeader(500)
						return
					}
					var receipt any = f.receipt()
					if remedial {
						receipt = observation.ObservationRedeployReceipt{RequestId: id, DeploymentId: uuid.New(), BaselineId: f.baseline, Status: "executing"}
						assert.NotContains(t, string(body), "assertion")
						assert.NotContains(t, string(body), "proof")
					}
					encoded, e := json.Marshal(receipt)
					if !assert.NoError(t, e) {
						w.WriteHeader(500)
						return
					}
					_, err = tx.ExecContext(r.Context(), `INSERT INTO observation_fault_effects VALUES ($1,$2) ON CONFLICT DO NOTHING`, id, encoded)
					if !assert.NoError(t, err) {
						w.WriteHeader(500)
						return
					}
					if !assert.NoError(t, tx.Commit()) {
						w.WriteHeader(500)
						return
					}
					sends.Add(1)
					close(committed)
					<-r.Context().Done()
					return
				}
				var receipt []byte
				if !assert.NoError(t, tx.QueryRowContext(r.Context(), `SELECT receipt FROM observation_fault_effects WHERE request_id=$1`, id).Scan(&receipt)) {
					w.WriteHeader(404)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if action == "observation.get" {
					_ = json.NewEncoder(w).Encode(observation.ObservationRun{Receipt: f.receipt(), Status: "succeeded"})
					return
				}
				_, _ = w.Write(receipt)
			}))
			t.Cleanup(peer.Close)
			port, err := plugins.NewDataPlanePort(peer.URL, f.server.PluginGateway, true)
			require.NoError(t, err)
			f.server.PluginDataPlane = port
			ctx, cancel := context.WithCancel(f.ctx(t))
			defer cancel()
			done := make(chan error, 1)
			go func() {
				if remedial {
					_, err := f.server.RedeployObservationBaseline(ctx, observation.RedeployObservationBaselineRequestObject{OrgId: "acme", PluginId: f.item.ID, RunId: f.run, Body: &command})
					done <- err
				} else {
					_, err := f.server.TryStartObservation(ctx, observation.TryStartObservationRequestObject{OrgId: "acme", PluginId: f.item.ID, Body: &f.body})
					done <- err
				}
			}()
			select {
			case <-committed:
			case err := <-done:
				t.Fatalf("dispatch failed before remote commit: %v", err)
			case <-time.After(10 * time.Second):
				t.Fatal("remote effect did not commit")
			}
			cancel()
			select {
			case err := <-done:
				require.Error(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("cancel did not release dispatch")
			}
			// Same key with changed exact body must conflict before any second send.
			if remedial {
				changed := command
				changed.BaselineFingerprint = "sha256:" + strings.Repeat("b", 64)
				_, err = f.server.RedeployObservationBaseline(f.ctx(t), observation.RedeployObservationBaselineRequestObject{OrgId: "acme", PluginId: f.item.ID, RunId: f.run, Body: &changed})
				require.ErrorContains(t, err, "plugin_operation_conflict")
			} else {
				changed := f.body
				changed.AdmissionDeadline = changed.AdmissionDeadline.Add(time.Second)
				_, err = f.server.TryStartObservation(f.ctx(t), observation.TryStartObservationRequestObject{OrgId: "acme", PluginId: f.item.ID, Body: &changed})
				require.ErrorContains(t, err, "plugin_operation_conflict")
			}
			// Authority can disappear and the old admission window can close. The
			// immutable intent is still enough for scoped GET reconciliation.
			f.admin.denied.Store(true)
			tx, e := f.db.BeginTx(t.Context(), nil)
			require.NoError(t, e)
			_, e = tx.ExecContext(t.Context(), `UPDATE environment_observation_grants SET state='revoked', revision=revision+1 WHERE org_id='acme' AND plugin_id=$1`, f.item.ID)
			require.NoError(t, e)
			_, e = tx.ExecContext(t.Context(), `UPDATE environment_observation_requests SET admission_deadline=$1 WHERE org_id='acme' AND plugin_id=$2`, time.Now().Add(-time.Minute), f.item.ID)
			require.NoError(t, e)
			if remedial {
				_, e = tx.ExecContext(t.Context(), `UPDATE observation_fault_effects SET receipt=jsonb_set(receipt,'{status}','"succeeded"') WHERE request_id=$1`, id)
				require.NoError(t, e)
			}
			require.NoError(t, tx.Commit())
			// Replace the request-serving object to discard every in-memory request state.
			recovered := &Server{Database: f.db, Logger: zap.NewNop(), PluginGateway: f.server.PluginGateway, PluginDataPlane: port}
			if remedial {
				response, err := recovered.GetObservationRedeployment(f.ctx(t), observation.GetObservationRedeploymentRequestObject{OrgId: "acme", PluginId: f.item.ID, RequestId: id})
				require.NoError(t, err)
				receipt := observation.ObservationRedeployReceipt(response.(observation.GetObservationRedeployment200JSONResponse))
				require.Equal(t, id, receipt.RequestId)
				require.EqualValues(t, "succeeded", receipt.Status)
			} else {
				response, err := recovered.GetObservationRequest(f.ctx(t), observation.GetObservationRequestRequestObject{OrgId: "acme", PluginId: f.item.ID, RequestId: id})
				require.NoError(t, err)
				require.Equal(t, id, observation.ObservationReceipt(response.(observation.GetObservationRequest200JSONResponse)).RequestId)
				tx, err := f.db.BeginTx(t.Context(), nil)
				require.NoError(t, err)
				stored, err := model.ReadObservationRequestTx(t.Context(), tx, "acme", f.item.ID, uuid.Nil, f.run)
				require.NoError(t, err)
				require.Equal(t, id, stored.RequestID)
				require.NoError(t, tx.Rollback())
				run, e := recovered.GetObservation(f.ctx(t), observation.GetObservationRequestObject{OrgId: "acme", PluginId: f.item.ID, RunId: f.run})
				require.NoError(t, e)
				require.EqualValues(t, "succeeded", observation.ObservationRun(run.(observation.GetObservation200JSONResponse)).Status)
			}
			require.Equal(t, int32(1), sends.Load())
			_, err = recovered.GetObservationRequest(f.ctx(t), observation.GetObservationRequestRequestObject{OrgId: "foreign", PluginId: f.item.ID, RequestId: f.body.RequestId})
			require.Error(t, err)
			_, err = recovered.GetObservationRedeployment(f.ctx(t), observation.GetObservationRedeploymentRequestObject{OrgId: "acme", PluginId: "dev.example.foreign", RequestId: id})
			require.Error(t, err)
		})
	}
}

// Fault barrier wraps real transactions, changing current authority only after
// the durable intent has committed and before the independent effect fence.
type observationCommitBarrier struct {
	model.Databaser
	once  sync.Once
	after func()
}

func (d *observationCommitBarrier) BeginTx(ctx context.Context, opts *sql.TxOptions) (model.TxWithCommit, error) {
	tx, err := d.Databaser.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &observationBarrierTx{TxWithCommit: tx, db: d}, nil
}

type observationBarrierTx struct {
	model.TxWithCommit
	db *observationCommitBarrier
}

func (tx *observationBarrierTx) Commit() error {
	if err := tx.TxWithCommit.Commit(); err != nil {
		return err
	}
	tx.db.once.Do(tx.db.after)
	return nil
}
func TestObservationIntentCannotAuthorizeAfterAdminLoss(t *testing.T) {
	for _, remedial := range []bool{false, true} {
		name := "observation"
		if remedial {
			name = "redeployment"
		}
		t.Run(name, func(t *testing.T) {
			f := newObservationDispatchFixture(t)
			var command observation.ObservationRedeployRequest
			if remedial {
				f.seedRun(t)
				command = f.redeploy(t)
			}
			f.server.Database = &observationCommitBarrier{Databaser: f.db, after: func() { f.admin.denied.Store(true) }}
			var err error
			if remedial {
				_, err = f.server.RedeployObservationBaseline(f.ctx(t), observation.RedeployObservationBaselineRequestObject{OrgId: "acme", PluginId: f.item.ID, RunId: f.run, Body: &command})
			} else {
				_, err = f.server.TryStartObservation(f.ctx(t), observation.TryStartObservationRequestObject{OrgId: "acme", PluginId: f.item.ID, Body: &f.body})
			}
			require.ErrorContains(t, err, "Admin")
			tx, e := f.db.BeginTx(t.Context(), nil)
			require.NoError(t, e)
			defer func() { _ = tx.Rollback() }()
			if remedial {
				row, e := model.ReadObservationRedeploymentTx(t.Context(), tx, "acme", f.item.ID, command.RequestId)
				require.NoError(t, e)
				require.Equal(t, f.actor, row.ActorID)
				require.NotContains(t, string(row.Body), "proof")
			} else {
				row, e := model.ReadObservationRequestTx(t.Context(), tx, "acme", f.item.ID, f.body.RequestId, uuid.Nil)
				require.NoError(t, e)
				require.Empty(t, row.Response)
			}
		})
	}
}

func TestObservationEffectRechecksGrantRevisionAfterIntentCommit(t *testing.T) {
	f := newObservationDispatchFixture(t)
	f.server.Database = &observationCommitBarrier{Databaser: f.db, after: func() {
		tx, err := f.db.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		_, err = tx.ExecContext(t.Context(), `UPDATE environment_observation_grants SET revision=revision+1, state='revoked' WHERE org_id='acme' AND plugin_id=$1`, f.item.ID)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
	}}
	_, err := f.server.TryStartObservation(f.ctx(t), observation.TryStartObservationRequestObject{OrgId: "acme", PluginId: f.item.ID, Body: &f.body})
	require.ErrorContains(t, err, "plugin_operation_conflict")
	tx, err := f.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	row, err := model.ReadObservationRequestTx(t.Context(), tx, "acme", f.item.ID, f.body.RequestId, uuid.Nil)
	require.NoError(t, err)
	require.Empty(t, row.Response)
}

func TestObservationRedeployEffectRechecksProofExpiryAfterIntentCommit(t *testing.T) {
	f := newObservationDispatchFixture(t)
	f.seedRun(t)
	command := f.redeploy(t)
	f.server.Database = &observationCommitBarrier{Databaser: f.db, after: func() { f.server.PluginGateway.Now = func() time.Time { return time.Now().UTC().Add(2 * time.Minute) } }}
	_, err := f.server.RedeployObservationBaseline(f.ctx(t), observation.RedeployObservationBaselineRequestObject{OrgId: "acme", PluginId: f.item.ID, RunId: f.run, Body: &command})
	require.ErrorContains(t, err, "plugin_delegation_forbidden")
	tx, err := f.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	row, err := model.ReadObservationRedeploymentTx(t.Context(), tx, "acme", f.item.ID, command.RequestId)
	require.NoError(t, err)
	require.NotContains(t, string(row.Body), command.Proof.Assertion)
}
