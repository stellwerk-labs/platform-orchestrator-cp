//go:build integration

package model

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func draftTestAdmissionFixture(t *testing.T) (Databaser, DeploymentAdmission) {
	t.Helper()
	db := externalOperationDatabaseWith(t)
	fixture := externalGrantFixture(t, db)
	ctx := t.Context()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	draft, err := db.CreateModuleDraft(ctx, tx, fixture.OrgID, "alpha", draftTestContent(t, `output "name" { value = "test" }`), fixture.ActorID, "Author test")
	require.NoError(t, err)
	snapshot, err := db.CaptureModuleDraftTestSnapshot(ctx, tx, fixture.OrgID, "alpha", ModuleDraftTestCapture{DraftID: draft.ID, ExpectedDraftRevision: 1, EnvironmentUUID: fixture.Intent.Environments[0].EnvironmentID, Actor: fixture.ActorID, Reason: "Review test material"})
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	module := snapshot.ModuleUUID.String()
	return db, DeploymentAdmission{OrgID: fixture.OrgID, AdmissionID: uuid.New(), ProjectUUID: snapshot.ProjectUUID, EnvironmentUUID: snapshot.EnvironmentUUID, ActorID: fixture.ActorID, RequestDigest: strings.Repeat("a", 64), Mode: "deploy", ModuleVersions: map[string]uuid.UUID{module: snapshot.ID}, DraftTestSnapshots: map[string]uuid.UUID{module: snapshot.ID}}
}

func TestDraftTestAdmissionRequiresExplicitSelectionAndCurrentAuthority(t *testing.T) {
	db, request := draftTestAdmissionFixture(t)
	ctx := t.Context()
	implicit := request
	implicit.DraftTestSnapshots = nil
	_, err := AuthorizeDeploymentAdmission(ctx, db, implicit, DeploymentAdmissionPolicy{AllowFreshDraftTests: true})
	require.Error(t, err, "unknown published Version UUIDs must not be inferred as tests")
	_, err = AuthorizeDeploymentAdmission(ctx, db, request)
	require.ErrorContains(t, err, "current Module authoring authority")
	rollback := request
	rollback.Mode = "rollback"
	_, err = AuthorizeDeploymentAdmission(ctx, db, rollback)
	require.ErrorContains(t, err, "successful prior admission")
	receipt, err := AuthorizeDeploymentAdmission(ctx, db, request, DeploymentAdmissionPolicy{AllowFreshDraftTests: true})
	require.NoError(t, err)
	_, err = AuthorizeDeploymentAdmission(ctx, db, request)
	require.ErrorContains(t, err, "current Module authoring authority", "replay must not retain a revoked permission decision")
	_, err = SettleDeploymentAdmission(ctx, db, request.OrgID, request.AdmissionID, request.ProjectUUID, request.EnvironmentUUID, request.RequestDigest, "succeeded")
	require.NoError(t, err)
	retained := request
	retained.AdmissionID = uuid.New()
	retained.CarryForwardModuleVersions = request.ModuleVersions
	next, err := AuthorizeDeploymentAdmission(ctx, db, retained)
	require.NoError(t, err)
	require.Greater(t, next.Epoch, receipt.Epoch)
	_, err = SettleDeploymentAdmission(ctx, db, retained.OrgID, retained.AdmissionID, retained.ProjectUUID, retained.EnvironmentUUID, retained.RequestDigest, "succeeded")
	require.NoError(t, err)
	rollback.AdmissionID = uuid.New()
	_, err = AuthorizeDeploymentAdmission(ctx, db, rollback)
	require.NoError(t, err)
}

func TestDraftTestAdmissionRejectsSelectionForgery(t *testing.T) {
	db, request := draftTestAdmissionFixture(t)
	for _, tt := range []struct {
		name   string
		change func(*DeploymentAdmission)
	}{
		{"wrong snapshot", func(r *DeploymentAdmission) {
			r.DraftTestSnapshots = map[string]uuid.UUID{}
			for module := range r.ModuleVersions {
				r.DraftTestSnapshots[module] = uuid.New()
			}
		}},
		{"unresolved Module", func(r *DeploymentAdmission) {
			r.DraftTestSnapshots = map[string]uuid.UUID{uuid.NewString(): uuid.New()}
		}},
		{"wrong Project", func(r *DeploymentAdmission) { r.ProjectUUID = uuid.New() }},
		{"wrong Environment", func(r *DeploymentAdmission) { r.EnvironmentUUID = uuid.New() }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			changed := request
			tt.change(&changed)
			_, err := AuthorizeDeploymentAdmission(t.Context(), db, changed, DeploymentAdmissionPolicy{AllowFreshDraftTests: true})
			require.Error(t, err)
		})
	}
}

func TestDraftTestAdmissionStillPreservesModulePins(t *testing.T) {
	db, request := draftTestAdmissionFixture(t)
	ctx := t.Context()
	module, err := db.GetModuleCatalogue(ctx, nil, request.OrgID, "alpha", GetModeDefault)
	require.NoError(t, err)
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = db.CreateEnvironmentModuleVersionPin(ctx, tx, request.OrgID, request.ProjectUUID, "project", request.EnvironmentUUID, "development", module.UUID, *module.CurrentDefaultVersionUUID, request.ActorID, "user", "Protect current version", nil, false)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	_, err = AuthorizeDeploymentAdmission(ctx, db, request, DeploymentAdmissionPolicy{AllowFreshDraftTests: true})
	var conflict ErrConflict
	require.ErrorAs(t, err, &conflict)
	require.Equal(t, PinProtection, conflict.Code)
}
