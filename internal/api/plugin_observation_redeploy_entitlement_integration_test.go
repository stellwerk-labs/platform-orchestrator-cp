//go:build integration

package api

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
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
	"github.com/stellwerk-labs/golib/herrors"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	observation "github.com/stellwerk-labs/platform-orchestrator-cp/internal/observationapi"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/plugincontext"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// This fixture uses real installed package metadata, signed development
// entitlements, PostgreSQL guards and signed delegation. IAM and the DP HTTP
// peer are controlled component peers, not the full native Core/Runner gate.
func newLicensedObservationRedeploy(t *testing.T) (*pluginEntitlementAPIFixture, *observationDispatchFixture) {
	t.Helper()
	f := newUnimportedPluginEntitlementAPIFixtureWithManifest(t, func(item *plugins.Installation) {
		item.Entitlement.Features = []string{"feature-a", "feature-b"}
		item.ServiceGrants = append(item.ServiceGrants, plugins.ServiceGrant{Action: "observation.redeploy", RequiredFeature: "feature-a", Permission: "plugin." + item.ID + ".control", Scope: "organization"})
		item.Operations = append(item.Operations, plugins.Operation{ID: "redeploy", Method: "POST", Path: "/orgs/{orgId}/redeploy", Mutating: true, RequireOrgAdmin: true, RequiredFeature: "feature-b", ServiceActions: []string{"observation.redeploy"}})
	})
	importObservationFeatures(t, f, 1, []string{"feature-a", "feature-b"})
	registry, err := plugins.NewInstallationRegistry([]plugins.Installation{f.item}, false)
	require.NoError(t, err)
	registry.Entitlements = f.adapter
	_, key, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	d := &observationDispatchFixture{db: f.db, item: f.item, admin: new(observationFaultAdmin), actor: uuid.New(), key: key, run: uuid.New(), baseline: uuid.New()}
	d.server = &Server{Database: f.db, Logger: zap.NewNop(), IamClient: operatorIAM(t).client, PluginEntitlements: f.adapter, PluginGateway: &plugins.Gateway{Registry: registry, SigningKey: key, Issuer: "core", OrganizationAdmins: d.admin}}
	env, err := f.db.GetEnvironment(t.Context(), nil, "acme", "project", "development", model.GetModeDefault)
	require.NoError(t, err)
	now := time.Now().UTC()
	grant := model.ObservationGrantCommand{OrgID: "acme", PluginID: f.item.ID, GrantID: uuid.New(), ActorID: d.actor, SourceOperation: "configure", SourceDigest: strings.Repeat("a", 64), RequestID: uuid.NewString(), Action: "replace", Reason: "Reviewed scope", Targets: []model.ExternalOperationEnvironment{{ProjectID: env.ProjectUuid, EnvironmentID: env.Uuid}}}
	tx, err := f.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = model.ApplyObservationGrantCommandTx(t.Context(), tx, grant, now)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	d.body = observation.ObservationRequest{GrantId: grant.GrantID, GrantRevision: 1, RequestId: uuid.New(), ProjectUuid: env.ProjectUuid, EnvironmentUuid: env.Uuid, ProjectId: env.ProjectId, EnvId: env.Id, Occurrence: now, AdmissionDeadline: now.Add(time.Minute)}
	d.seedRun(t)
	return f, d
}

func importObservationFeatures(t *testing.T, f *pluginEntitlementAPIFixture, sequence uint64, features []string) {
	t.Helper()
	if f.base < 10 {
		t.Fatal("signed feature fixture requires a positive timestamp")
		return
	}
	base := uint64(f.base)
	issuance := f.grant.IssuanceID
	if sequence != 1 {
		issuance = uuid.New()
	}
	raw := f.sign(t, map[uint64]any{1: uint64(1), 2: "grant", 3: "development.example", 4: issuance[:], 5: sequence, 6: f.item.ID, 7: f.binding.InstallationID[:], 8: f.binding.OrganizationID[:], 9: features, 10: map[string]uint64{}, 11: base - 10, 12: base - 5, 13: base + 10, 14: base + 20, 15: "development"})
	result := importBundle(t, f.db, f.adapter, f.bundle(t, raw))
	require.Empty(t, result.Diagnostics)
	require.Equal(t, 1, result.ImportedGrants)
}

func TestLicensedObservationRedeployAndNestedAdmissionAvoidEntitlementReentry(t *testing.T) {
	_, d := newLicensedObservationRedeploy(t)
	iam := operatorIAM(t)
	checker := uuid.MustParse(d.item.AdmissionCheckerID)
	iam.createStaticPrincipal(checker)
	d.server.IamClient = iam.client
	d.server.DeploymentAdmissionIdentity = checker
	var callbacks atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			w.WriteHeader(500)
			return
		}
		_, err = plugincontext.Verify(d.key.Public().(ed25519.PublicKey), r.Header.Get(plugincontext.Header), plugincontext.Request{Issuer: "core", Audience: "platform-orchestrator-dp", PluginID: d.item.ID, OrgID: "acme", OperationID: "observation.redeploy", Method: r.Method, Path: r.URL.EscapedPath(), Body: body, IdempotencyKey: r.Header.Get("Idempotency-Key")}, time.Now().UTC())
		if !assert.NoError(t, err) {
			w.WriteHeader(401)
			return
		}
		// The real CP caller boundary must complete while its parent request
		// still holds the actual licensed plugin binding fence across this call.
		ctx := context.WithValue(r.Context(), hecho.ContextKeyUserID, checker.String())
		if !assert.NoError(t, d.server.deploymentAdmissionCaller(ctx, "acme")) {
			w.WriteHeader(503)
			return
		}
		callbacks.Add(1)
		var request observation.ObservationRedeployRequest
		if !assert.NoError(t, json.Unmarshal(body, &request)) {
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(observation.ObservationRedeployReceipt{RequestId: request.RequestId, DeploymentId: uuid.New(), BaselineId: d.baseline, Status: "executing"})
	}))
	t.Cleanup(peer.Close)
	var err error
	d.server.PluginDataPlane, err = plugins.NewDataPlanePort(peer.URL, d.server.PluginGateway, true)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(d.ctx(t), 3*time.Second)
	defer cancel()
	command := d.redeploy(t)
	response, err := d.server.RedeployObservationBaseline(ctx, observation.RedeployObservationBaselineRequestObject{OrgId: "acme", PluginId: d.item.ID, RunId: d.run, Body: &command})
	require.NoError(t, err)
	require.IsType(t, observation.RedeployObservationBaseline200JSONResponse{}, response)
	require.EqualValues(t, 1, callbacks.Load())
}

func TestDeploymentAdmissionClassifiesPersistedPluginServicesWithoutDiscovery(t *testing.T) {
	f := newPluginEntitlementAPIFixture(t)
	iam := operatorIAM(t)
	service := uuid.MustParse(f.item.ServiceUserID)
	iam.createStaticPrincipal(service) // Even misconfiguration cannot promote it.
	registry, err := plugins.NewInstallationRegistry([]plugins.Installation{f.item}, false)
	require.NoError(t, err)
	registry.Entitlements = f.adapter
	f.server.PluginGateway = &plugins.Gateway{Registry: registry}
	f.server.DeploymentAdmissionIdentity, f.server.IamClient = service, iam.client
	ctx, cancel := context.WithTimeout(context.WithValue(t.Context(), hecho.ContextKeyUserID, service.String()), 3*time.Second)
	defer cancel()
	tx, err := f.server.pluginEffectDatabase("acme", f.item, "effect.commit", false).BeginTx(ctx, nil)
	require.NoError(t, err)
	require.ErrorContains(t, f.server.deploymentAdmissionCaller(ctx, "acme"), "plugin service cannot act")
	require.NoError(t, tx.Rollback())
	for _, state := range []string{model.PluginInstallationInstalled, model.PluginInstallationActive, model.PluginInstallationDisabled, model.PluginInstallationUninstalled, model.PluginInstallationPurged} {
		tx, err := f.db.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		_, err = tx.ExecContext(t.Context(), `UPDATE plugin_installations SET state=$1 WHERE installation_uuid=$2`, state, f.item.InstallationID)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
		require.ErrorContains(t, f.server.deploymentAdmissionCaller(ctx, "another-organization"), "plugin service cannot act", state)
	}
	invalid := f.item
	invalid.Backend = ""
	require.NotEmpty(t, registry.Replace(2, []plugins.Installation{invalid}, false))
	require.ErrorContains(t, f.server.deploymentAdmissionCaller(ctx, "acme"), "plugin service cannot act", "invalid snapshots must not erase persisted identity")
	require.Zero(t, iam.lookups.Load(), "plugin services are denied before any IAM principal lookup")
	cancelled, stop := context.WithCancel(ctx)
	stop()
	var boundary *herrors.PlatformOrchestratorError
	require.ErrorAs(t, f.server.deploymentAdmissionCaller(cancelled, "acme"), &boundary)
	require.Equal(t, "authorization_unavailable", boundary.Code)
	require.Equal(t, 503, boundary.StatusCode)
}

// A real entitlement evaluation is paused only after it has finished and
// released its discovery transaction. Import then changes authoritative scope
// before the request acquires its effect fence, exercising a stale descriptor.
type observationEntitlementPreflightBarrier struct {
	plugins.EntitlementResolver
	once     sync.Once
	captured chan struct{}
	resume   chan struct{}
}

func (b *observationEntitlementPreflightBarrier) ResolveEntitlement(ctx context.Context, org string, item plugins.Installation) (plugins.EntitlementStatus, error) {
	status, err := b.EntitlementResolver.ResolveEntitlement(ctx, org, item)
	b.once.Do(func() {
		close(b.captured)
		select {
		case <-b.resume:
		case <-ctx.Done():
		}
	})
	return status, err
}

func TestObservationRedeployRechecksDistinctSourceFeatureAfterPreflight(t *testing.T) {
	for _, change := range []string{"remove-source-feature", "revoke-grant"} {
		t.Run(change, func(t *testing.T) {
			f, d := newLicensedObservationRedeploy(t)
			barrier := &observationEntitlementPreflightBarrier{EntitlementResolver: f.adapter, captured: make(chan struct{}), resume: make(chan struct{})}
			d.server.PluginGateway.Registry.Entitlements = barrier
			command := d.redeploy(t)
			ctx, cancel := context.WithTimeout(d.ctx(t), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				tx, _, _, err := d.server.observationRedeployAuthority(ctx, "acme", d.item.ID, d.run, command, observationProof(*command.Proof))
				if tx != nil {
					_ = tx.Rollback()
				}
				done <- err
			}()
			select {
			case <-barrier.captured:
			case <-ctx.Done():
				t.Fatal("discovery did not finish before its deadline")
			}
			if change == "remove-source-feature" {
				importObservationFeatures(t, f, 2, []string{"feature-a"})
			} else {
				_, err := model.ImportPluginEntitlementRevocations(t.Context(), f.db, nil, f.signRevocation(t, f.grant.IssuanceID), "internal_system", sha256.Sum256([]byte("revoke-during-preflight")))
				require.NoError(t, err)
			}
			close(barrier.resume)
			select {
			case err := <-done:
				requirePlatformEntitlementCode(t, err, "plugin_not_entitled")
			case <-ctx.Done():
				t.Fatal("authority recursively waited on its own entitlement fence")
			}
		})
	}
}

func TestLicensedObservationRedeployRetainsFreshProofAndAdminChecks(t *testing.T) {
	_, d := newLicensedObservationRedeploy(t)
	for _, failure := range []string{"body", "expired", "admin", "action"} {
		t.Run(failure, func(t *testing.T) {
			command := d.redeploy(t)
			proof := observationProof(*command.Proof)
			switch failure {
			case "body":
				proof.Body = []byte(`{"intent":"changed"}`)
			case "expired":
				d.server.PluginGateway.Now = func() time.Time { return time.Now().Add(2 * time.Minute) }
				t.Cleanup(func() { d.server.PluginGateway.Now = nil })
			case "admin":
				d.admin.denied.Store(true)
				t.Cleanup(func() { d.admin.denied.Store(false) })
			case "action":
				proof.OperationID = "effect"
			}
			ctx, cancel := context.WithTimeout(d.ctx(t), 3*time.Second)
			defer cancel()
			tx, _, _, err := d.server.observationRedeployAuthority(ctx, "acme", d.item.ID, d.run, command, proof)
			if tx != nil {
				_ = tx.Rollback()
			}
			require.Error(t, err)
			require.NotErrorIs(t, err, context.DeadlineExceeded)
		})
	}
}
