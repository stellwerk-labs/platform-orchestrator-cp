//go:build integration

package model

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/moduleversions"
	"github.com/stretchr/testify/require"
)

func admissionFixture(t *testing.T) (Databaser, *ExternalOperationGrant, DeploymentAdmission) {
	t.Helper()
	db := externalOperationDatabase(t)
	command := externalGrantFixture(t, db)
	grant, err := ApplyExternalGrantCommand(t.Context(), db, command)
	require.NoError(t, err)
	command.Intent, command.Action, command.RequestID, command.ExpectedRevision = nil, "activate", "start", grant.Revision
	grant, err = ApplyExternalGrantCommand(t.Context(), db, command)
	require.NoError(t, err)
	env := grant.Intent.Environments[0]
	return db, grant, DeploymentAdmission{
		OrgID: grant.OrgID, AdmissionID: uuid.New(), ProjectUUID: env.ProjectID, EnvironmentUUID: env.EnvironmentID,
		ActorID: grant.ActorID, RequestDigest: strings.Repeat("b", 64), Mode: "deploy", ModuleVersions: grant.Intent.TargetVersions,
		CarryForwardModuleVersions: grant.Intent.TargetVersions,
	}
}

func TestEnvironmentAdmissionPinFirstSerializesThenRejectsStaleGraph(t *testing.T) {
	db, grant, request := admissionFixture(t)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	require.NoError(t, CheckEnvironmentAdmissionEpoch(t.Context(), db, tx, request.OrgID, request.EnvironmentUUID, 0))
	module := grant.Intent.Modules[0]
	_, err = db.CreateEnvironmentModuleVersionPin(t.Context(), tx, grant.OrgID, request.ProjectUUID, "project", request.EnvironmentUUID, "development", module, *grant.BaselineDefaults[module.String()], grant.ActorID, "user", "Protect baseline", nil, false)
	require.NoError(t, err)
	// A separate connection must actually wait on the same lock; without the
	// barrier it could inspect only committed Pins and admit the stale graph.
	blocked, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	_, err = AuthorizeDeploymentAdmission(blocked, db, request)
	require.Error(t, err)
	require.Error(t, blocked.Err())
	require.NoError(t, tx.Commit())
	_, err = AuthorizeDeploymentAdmission(t.Context(), db, request)
	require.ErrorContains(t, err, "does not preserve current Pin")
	epoch, err := ReadEnvironmentAdmissionEpoch(t.Context(), db, nil, request.OrgID, request.EnvironmentUUID)
	require.NoError(t, err)
	require.Zero(t, epoch, "rejected admissions must not advance the epoch")
}

func TestEnvironmentAdmissionGuardAndEpochCoverCompleteDuringPinInspection(t *testing.T) {
	db, grant, request := admissionFixture(t)
	before, err := ReadEnvironmentAdmissionEpoch(t.Context(), db, nil, request.OrgID, request.EnvironmentUUID)
	require.NoError(t, err)
	receipt, err := AuthorizeDeploymentAdmission(t.Context(), db, request)
	require.NoError(t, err)
	require.Equal(t, int64(1), receipt.Epoch)
	replayRequest := request
	replayRequest.ConfirmedRestrictedVersionUUIDs = []uuid.UUID{}
	replayed, err := AuthorizeDeploymentAdmission(t.Context(), db, replayRequest)
	require.NoError(t, err)
	require.Equal(t, receipt, replayed, "replay cannot advance the epoch")
	checkPin := func(epoch int64) error {
		tx, err := db.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		return CheckEnvironmentAdmissionEpoch(t.Context(), db, tx, request.OrgID, request.EnvironmentUUID, epoch)
	}
	require.ErrorContains(t, checkPin(receipt.Epoch), "unresolved ordinary")
	settled, err := SettleDeploymentAdmission(t.Context(), db, request.OrgID, request.AdmissionID, request.ProjectUUID, request.EnvironmentUUID, request.RequestDigest, "succeeded")
	require.NoError(t, err)
	require.Equal(t, "settled", settled.State)
	require.ErrorContains(t, checkPin(before), "changed while inspecting", "a completed Deployment must still invalidate prior Pin evidence")
	require.NoError(t, checkPin(receipt.Epoch))
	_, err = AuthorizeDeploymentAdmission(t.Context(), db, request)
	require.ErrorContains(t, err, "settled", "one admission cannot execute twice")
	_, err = SettleDeploymentAdmission(t.Context(), db, request.OrgID, request.AdmissionID, request.ProjectUUID, request.EnvironmentUUID, request.RequestDigest, "absent")
	require.ErrorContains(t, err, "immutable")
	_, err = SettleDeploymentAdmission(t.Context(), db, request.OrgID, request.AdmissionID, uuid.New(), request.EnvironmentUUID, request.RequestDigest, "succeeded")
	require.ErrorContains(t, err, "immutable")

	dispatch := ExternalDispatch{OrgID: grant.OrgID, PluginID: grant.PluginID, OperationID: grant.OperationID, ActorID: grant.ActorID, Fence: grant.Revision, Action: "external-deployment.admit", DispatchID: "external-after-ordinary", ProjectUUID: request.ProjectUUID, EnvironmentUUID: request.EnvironmentUUID, RequestDigest: grant.Intent.Fingerprint, TargetVersions: grant.Intent.TargetVersions}
	require.NoError(t, RecordExternalDispatch(t.Context(), db, dispatch))
	require.NoError(t, RecordExternalDispatch(t.Context(), db, dispatch))
	epoch, err := ReadEnvironmentAdmissionEpoch(t.Context(), db, nil, request.OrgID, request.EnvironmentUUID)
	require.NoError(t, err)
	require.Equal(t, int64(2), epoch, "external admission participates in the same epoch; replay does not increment")
	request.AdmissionID = uuid.New()
	_, err = AuthorizeDeploymentAdmission(t.Context(), db, request)
	require.ErrorContains(t, err, "unresolved external")
	require.NoError(t, CompleteExternalDispatch(t.Context(), db, grant.OrgID, grant.PluginID, grant.OperationID, dispatch.DispatchID, ExternalDispatchCompletion{DeploymentID: uuid.New(), EnvironmentUUID: request.EnvironmentUUID, ProjectUUID: request.ProjectUUID, RequestDigest: dispatch.RequestDigest, Status: "succeeded"}))
	require.ErrorContains(t, checkPin(receipt.Epoch), "changed while inspecting")
	_, err = AuthorizeDeploymentAdmission(t.Context(), db, request)
	require.NoError(t, err)
	dispatch.DispatchID = "blocked-by-ordinary"
	require.ErrorContains(t, RecordExternalDispatch(t.Context(), db, dispatch), "unresolved ordinary")
}

func TestEnvironmentAdmissionRechecksCurrentModuleLifecycle(t *testing.T) {
	t.Run("fresh Proposed requires trusted current authorization", func(t *testing.T) {
		db, _, request := admissionFixture(t)
		request.CarryForwardModuleVersions = nil
		_, err := AuthorizeDeploymentAdmission(t.Context(), db, request)
		require.ErrorContains(t, err, "current scoped authority")
		_, err = AuthorizeDeploymentAdmission(t.Context(), db, request, DeploymentAdmissionPolicy{AllowFreshProposed: true})
		require.NoError(t, err)
	})

	t.Run("active Pin removal before locked replay cannot inherit prior decision", func(t *testing.T) {
		db, grant, request := admissionFixture(t)
		request.CarryForwardModuleVersions = nil
		var firstPin *EnvironmentModuleVersionPin
		for _, moduleUUID := range grant.Intent.Modules {
			tx, err := db.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			pin, err := db.CreateEnvironmentModuleVersionPin(t.Context(), tx, request.OrgID, request.ProjectUUID, "project", request.EnvironmentUUID, "development", moduleUUID, request.ModuleVersions[moduleUUID.String()], request.ActorID, "user", "Protect candidate", nil, false)
			require.NoError(t, err)
			require.NoError(t, tx.Commit())
			if firstPin == nil {
				firstPin = pin
			}
		}
		_, err := AuthorizeDeploymentAdmission(t.Context(), db, request)
		require.NoError(t, err, "exact active Pins permit their Proposed versions without fresh-adoption authority")
		tx, err := db.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		_, err = db.TransitionEnvironmentModuleVersionPin(t.Context(), tx, request.OrgID, firstPin.ID, firstPin.ResourceVersion, moduleversions.PinRemoved, "removed", request.ActorID, "user", "Remove protection", nil, nil, nil)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
		_, err = AuthorizeDeploymentAdmission(t.Context(), db, request)
		require.ErrorContains(t, err, "current scoped authority", "an active guard replay must recheck current Pin and actor policy")
		_, err = AuthorizeDeploymentAdmission(t.Context(), db, request, DeploymentAdmissionPolicy{AllowFreshProposed: true})
		require.NoError(t, err)
	})

	t.Run("archived exact carry-forward permits unrelated target changes", func(t *testing.T) {
		db, grant, request := admissionFixture(t)
		moduleUUID := grant.Intent.Modules[0]
		request.CarryForwardModuleVersions = nil
		module, err := db.GetModuleCatalogue(t.Context(), nil, request.OrgID, moduleUUID.String(), GetModeDefault)
		require.NoError(t, err)
		tx, err := db.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		_, err = db.SetModuleCatalogueStatus(t.Context(), tx, request.OrgID, moduleUUID.String(), moduleversions.CatalogueArchived, request.ActorID, "Retire catalogue entry", module.ResourceVersion)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())

		_, err = AuthorizeDeploymentAdmission(t.Context(), db, request, DeploymentAdmissionPolicy{AllowFreshProposed: true})
		require.ErrorContains(t, err, "exact carry-forward")
		request.CarryForwardModuleVersions = map[string]uuid.UUID{moduleUUID.String(): request.ModuleVersions[moduleUUID.String()]}
		request.AdmissionID = uuid.New()
		_, err = AuthorizeDeploymentAdmission(t.Context(), db, request, DeploymentAdmissionPolicy{AllowFreshProposed: true})
		require.NoError(t, err)
		require.Len(t, request.ModuleVersions, 2)
		require.Len(t, request.CarryForwardModuleVersions, 1, "a changed target Module must not be mislabeled as carry-forward")
	})

	t.Run("deprecated requires active Pin outside rollback", func(t *testing.T) {
		db, grant, request := admissionFixture(t)
		moduleUUID := grant.Intent.Modules[0]
		version, err := db.GetCoreModuleVersion(t.Context(), nil, request.OrgID, moduleUUID.String(), request.ModuleVersions[moduleUUID.String()].String(), GetModeDefault)
		require.NoError(t, err)
		tx, err := db.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		_, err = db.TransitionCoreModuleVersion(t.Context(), tx, request.OrgID, moduleUUID.String(), version.UUID.String(), moduleversions.LifecycleDeprecated, version.ResourceVersion, request.ActorID, "Withdraw candidate", nil)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())

		_, err = AuthorizeDeploymentAdmission(t.Context(), db, request)
		require.ErrorContains(t, err, "active Pin or rollback")
		tx, err = db.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		_, err = db.CreateEnvironmentModuleVersionPin(t.Context(), tx, request.OrgID, request.ProjectUUID, "project", request.EnvironmentUUID, "development", moduleUUID, version.UUID, request.ActorID, "user", "Retain deprecated version", nil, false)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
		request.AdmissionID = uuid.New()
		_, err = AuthorizeDeploymentAdmission(t.Context(), db, request)
		require.NoError(t, err)
	})

	t.Run("defective requires exact used confirmation and Pin", func(t *testing.T) {
		db, grant, request := admissionFixture(t)
		moduleUUID := grant.Intent.Modules[0]
		versionUUID := request.ModuleVersions[moduleUUID.String()]
		tx, err := db.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		_, err = db.CreateEnvironmentModuleVersionPin(t.Context(), tx, request.OrgID, request.ProjectUUID, "project", request.EnvironmentUUID, "development", moduleUUID, versionUUID, request.ActorID, "user", "Protect candidate", nil, false)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
		version, err := db.GetCoreModuleVersion(t.Context(), nil, request.OrgID, moduleUUID.String(), versionUUID.String(), GetModeDefault)
		require.NoError(t, err)
		tx, err = db.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		_, err = db.TransitionCoreModuleVersion(t.Context(), tx, request.OrgID, moduleUUID.String(), versionUUID.String(), moduleversions.LifecycleDefective, version.ResourceVersion, request.ActorID, "Unsafe candidate", nil)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())

		_, err = AuthorizeDeploymentAdmission(t.Context(), db, request)
		require.ErrorContains(t, err, "exact confirmation")
		otherModule := grant.Intent.Modules[1]
		request.ConfirmedRestrictedVersionUUIDs = []uuid.UUID{request.ModuleVersions[otherModule.String()]}
		_, err = AuthorizeDeploymentAdmission(t.Context(), db, request)
		require.ErrorContains(t, err, "exact confirmation")
		request.ConfirmedRestrictedVersionUUIDs = []uuid.UUID{versionUUID}
		request.AdmissionID = uuid.New()
		_, err = AuthorizeDeploymentAdmission(t.Context(), db, request)
		require.NoError(t, err)
	})

	t.Run("rollback permits confirmed defective without Pin", func(t *testing.T) {
		db, grant, request := admissionFixture(t)
		moduleUUID := grant.Intent.Modules[0]
		versionUUID := request.ModuleVersions[moduleUUID.String()]
		version, err := db.GetCoreModuleVersion(t.Context(), nil, request.OrgID, moduleUUID.String(), versionUUID.String(), GetModeDefault)
		require.NoError(t, err)
		tx, err := db.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		_, err = db.TransitionCoreModuleVersion(t.Context(), tx, request.OrgID, moduleUUID.String(), versionUUID.String(), moduleversions.LifecycleDefective, version.ResourceVersion, request.ActorID, "Unsafe candidate", nil)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
		request.Mode = "rollback"
		request.ConfirmedRestrictedVersionUUIDs = []uuid.UUID{versionUUID}
		_, err = AuthorizeDeploymentAdmission(t.Context(), db, request)
		require.NoError(t, err)
	})
}

func TestEnvironmentAdmissionDestroyRequiresDeletingAndNoPendingOverride(t *testing.T) {
	db := externalOperationDatabase(t)
	_, grant, begin := externalPinFixture(t, db)
	env := grant.Intent.Environments[0]
	request := DeploymentAdmission{OrgID: grant.OrgID, AdmissionID: uuid.New(), ProjectUUID: env.ProjectID, EnvironmentUUID: env.EnvironmentID, ActorID: uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff"), RequestDigest: strings.Repeat("b", 64), Mode: "destroy", ModuleVersions: map[string]uuid.UUID{}}
	withModuleEvidence := request
	withModuleEvidence.ModuleVersions = grant.Intent.TargetVersions
	_, err := AuthorizeDeploymentAdmission(t.Context(), db, withModuleEvidence)
	require.ErrorContains(t, err, "do not accept Module lifecycle evidence")
	_, err = AuthorizeDeploymentAdmission(t.Context(), db, request)
	require.ErrorContains(t, err, "confirmed deletion")
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `UPDATE envs SET status='deleting' WHERE org_id=$1 AND uuid=$2`, request.OrgID, request.EnvironmentUUID)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	pins, err := ApplyExternalPinCommand(t.Context(), db, begin, nil)
	require.NoError(t, err)
	_, err = AuthorizeDeploymentAdmission(t.Context(), db, request)
	require.ErrorContains(t, err, "Pin protection")
	cancel := begin
	cancel.Action, cancel.RequestID, cancel.Targets = "cancel", "cancel-before-destroy", nil
	for _, pin := range pins {
		cancel.Targets = append(cancel.Targets, ExternalPinTarget{PinID: pin.ID, ExpectedResourceVersion: pin.ResourceVersion})
	}
	_, err = ApplyExternalPinCommand(t.Context(), db, cancel, nil)
	require.NoError(t, err)
	_, err = AuthorizeDeploymentAdmission(t.Context(), db, request)
	require.NoError(t, err, "active Pins do not block confirmed Environment deletion")
}

func TestEnvironmentAdmissionMigrationDownRequiresSettledOutcomes(t *testing.T) {
	for _, path := range []string{"ordinary", "external"} {
		t.Run(path, func(t *testing.T) {
			db, grant, request := admissionFixture(t)
			// Exercise this guard directly, independently of later retained-history guards.
			resetExtensionEventFixtures(t, db)
			provider := extensionMigrationProvider(t, db)
			initialVersion, err := provider.GetDBVersion(t.Context())
			require.NoError(t, err)
			dispatch := ExternalDispatch{OrgID: grant.OrgID, PluginID: grant.PluginID, OperationID: grant.OperationID, ActorID: grant.ActorID, Fence: grant.Revision, Action: "external-deployment.admit", DispatchID: "migration-pending", ProjectUUID: request.ProjectUUID, EnvironmentUUID: request.EnvironmentUUID, RequestDigest: grant.Intent.Fingerprint, TargetVersions: grant.Intent.TargetVersions}
			if path == "ordinary" {
				_, err = AuthorizeDeploymentAdmission(t.Context(), db, request)
			} else {
				err = RecordExternalDispatch(t.Context(), db, dispatch)
			}
			require.NoError(t, err)
			err = exerciseSQLMigrationDown(t, db, 36, false)
			require.ErrorContains(t, err, "cannot remove unresolved Environment admission protection")
			version, err := provider.GetDBVersion(t.Context())
			require.NoError(t, err)
			require.Equal(t, initialVersion, version, "a refused downgrade must preserve the migration journal")
			epoch, err := ReadEnvironmentAdmissionEpoch(t.Context(), db, nil, request.OrgID, request.EnvironmentUUID)
			require.NoError(t, err)
			require.Equal(t, int64(1), epoch, "refused migration must preserve protection evidence")
			if path == "ordinary" {
				_, err = SettleDeploymentAdmission(t.Context(), db, request.OrgID, request.AdmissionID, request.ProjectUUID, request.EnvironmentUUID, request.RequestDigest, "absent")
			} else {
				err = CompleteExternalDispatch(t.Context(), db, grant.OrgID, grant.PluginID, grant.OperationID, dispatch.DispatchID, ExternalDispatchCompletion{DeploymentID: uuid.New(), EnvironmentUUID: request.EnvironmentUUID, ProjectUUID: request.ProjectUUID, RequestDigest: dispatch.RequestDigest, Status: "succeeded"})
			}
			require.NoError(t, err)
			err = exerciseSQLMigrationDown(t, db, 36, true)
			require.NoError(t, err)
			request.AdmissionID = uuid.New()
			_, err = AuthorizeDeploymentAdmission(t.Context(), db, request)
			require.NoError(t, err, "the reinstalled schema must admit a fresh exact request")
		})
	}
}

func TestEnvironmentAdmissionSealedAbsencePreventsDelayedAuthorize(t *testing.T) {
	db, _, request := admissionFixture(t)
	for _, admitted := range []bool{false, true} {
		request.AdmissionID = uuid.New()
		if admitted {
			_, err := AuthorizeDeploymentAdmission(t.Context(), db, request)
			require.NoError(t, err)
		}
		sealed, err := SealDeploymentAdmissionAbsence(t.Context(), db, request)
		require.NoError(t, err)
		require.Equal(t, "settled", sealed.State)
		require.Equal(t, "absent", sealed.Outcome)
		replayed, err := SealDeploymentAdmissionAbsence(t.Context(), db, request)
		require.NoError(t, err)
		require.Equal(t, sealed, replayed)
		_, err = AuthorizeDeploymentAdmission(t.Context(), db, request)
		require.ErrorContains(t, err, "settled", "a delayed authorize cannot reopen the tombstone")
		for _, change := range []func(*DeploymentAdmission){
			func(r *DeploymentAdmission) { r.ActorID = uuid.New() },
			func(r *DeploymentAdmission) { r.ProjectUUID = uuid.New() },
			func(r *DeploymentAdmission) { r.EnvironmentUUID = uuid.New() },
			func(r *DeploymentAdmission) { r.RequestDigest = strings.Repeat("f", 64) },
			func(r *DeploymentAdmission) { r.Mode = "rollback" },
			func(r *DeploymentAdmission) {
				r.ModuleVersions = map[string]uuid.UUID{}
				r.CarryForwardModuleVersions = map[string]uuid.UUID{}
			},
		} {
			altered := request
			change(&altered)
			_, err = SealDeploymentAdmissionAbsence(t.Context(), db, altered)
			require.ErrorContains(t, err, "immutable")
		}
	}
	request.AdmissionID = uuid.New()
	_, err := AuthorizeDeploymentAdmission(t.Context(), db, request)
	require.NoError(t, err)
	_, err = SettleDeploymentAdmission(t.Context(), db, request.OrgID, request.AdmissionID, request.ProjectUUID, request.EnvironmentUUID, request.RequestDigest, "succeeded")
	require.NoError(t, err)
	_, err = SealDeploymentAdmissionAbsence(t.Context(), db, request)
	require.ErrorContains(t, err, "immutable", "completed infrastructure cannot become absent")
}

func TestEnvironmentAdmissionSealedAbsenceSerializesWithAuthorize(t *testing.T) {
	db, _, request := admissionFixture(t)
	for range 5 {
		request.AdmissionID = uuid.New()
		var group sync.WaitGroup
		var authorizeErr, sealErr error
		start := make(chan struct{})
		group.Go(func() {
			<-start
			_, authorizeErr = AuthorizeDeploymentAdmission(t.Context(), db, request)
		})
		group.Go(func() {
			<-start
			_, sealErr = SealDeploymentAdmissionAbsence(t.Context(), db, request)
		})
		close(start)
		group.Wait()
		require.NoError(t, sealErr)
		if authorizeErr != nil {
			require.ErrorContains(t, authorizeErr, "settled")
		}
		_, err := AuthorizeDeploymentAdmission(t.Context(), db, request)
		require.ErrorContains(t, err, "settled")
		// Even if authorize won the first lock, absence settles that exact
		// guard, allowing a new identity without reopening the closed attempt.
	}
}

// The Data Plane retries an admission refused as environment_busy and rejects
// other refusals for good, so each refusal must carry its stable code.
func TestEnvironmentAdmissionConflictsCarryStableCodes(t *testing.T) {
	requireCode := func(t *testing.T, err error, code string) {
		t.Helper()
		conflict, ok := IsErrConflict(err)
		require.True(t, ok, "expected a conflict, got %v", err)
		require.Equal(t, code, conflict.Code)
	}
	db, grant, request := admissionFixture(t)
	_, err := AuthorizeDeploymentAdmission(t.Context(), db, request)
	require.NoError(t, err)
	second := request
	second.AdmissionID = uuid.New()
	_, err = AuthorizeDeploymentAdmission(t.Context(), db, second)
	requireCode(t, err, EnvironmentBusy)
	dispatch := ExternalDispatch{OrgID: grant.OrgID, PluginID: grant.PluginID, OperationID: grant.OperationID, ActorID: grant.ActorID, Fence: grant.Revision, Action: "external-deployment.admit", DispatchID: "blocked-by-ordinary", ProjectUUID: request.ProjectUUID, EnvironmentUUID: request.EnvironmentUUID, RequestDigest: grant.Intent.Fingerprint, TargetVersions: grant.Intent.TargetVersions}
	requireCode(t, RecordExternalDispatch(t.Context(), db, dispatch), EnvironmentBusy)
	_, err = SettleDeploymentAdmission(t.Context(), db, request.OrgID, request.AdmissionID, request.ProjectUUID, request.EnvironmentUUID, request.RequestDigest, "succeeded")
	require.NoError(t, err)

	dispatch.DispatchID = "external-first"
	require.NoError(t, RecordExternalDispatch(t.Context(), db, dispatch))
	_, err = AuthorizeDeploymentAdmission(t.Context(), db, second)
	requireCode(t, err, EnvironmentBusy)
	require.NoError(t, CompleteExternalDispatch(t.Context(), db, grant.OrgID, grant.PluginID, grant.OperationID, dispatch.DispatchID, ExternalDispatchCompletion{DeploymentID: uuid.New(), EnvironmentUUID: request.EnvironmentUUID, ProjectUUID: request.ProjectUUID, RequestDigest: dispatch.RequestDigest, Status: "succeeded"}))

	fresh := second
	fresh.AdmissionID = uuid.New()
	fresh.CarryForwardModuleVersions = nil
	_, err = AuthorizeDeploymentAdmission(t.Context(), db, fresh)
	requireCode(t, err, ModuleVersionLifecycle)

	module := grant.Intent.Modules[0]
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = db.CreateEnvironmentModuleVersionPin(t.Context(), tx, grant.OrgID, request.ProjectUUID, "project", request.EnvironmentUUID, "development", module, *grant.BaselineDefaults[module.String()], grant.ActorID, "user", "Protect baseline", nil, false)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	_, err = AuthorizeDeploymentAdmission(t.Context(), db, second)
	requireCode(t, err, PinProtection)
}
