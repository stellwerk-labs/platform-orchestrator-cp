//go:build integration

package model

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func externalGrantFixture(t *testing.T, db Databaser) ExternalGrantCommand {
	t.Helper()
	return externalGrantFixtureWithTarget(t, db, "1.1.0")
}

func externalGrantFixtureWithTarget(t *testing.T, db Databaser, targetVersion string) ExternalGrantCommand {
	t.Helper()
	actor := uuid.New()
	org, modules, _ := externalOperationModulesWithTarget(t, db, actor, targetVersion)
	organization, err := db.GetOrg(t.Context(), nil, org)
	require.NoError(t, err)
	now := time.Now().UTC()
	project, err := db.CreateProject(t.Context(), nil, &Project{OrgId: org, OrgUuid: organization.Uuid, Id: "project", DisplayName: "Grant project", CreatedAt: now, UpdatedAt: now, Status: ProjectStatusActive})
	require.NoError(t, err)
	envType, err := db.CreateEnvironmentType(t.Context(), nil, &EnvType{OrgId: org, OrgUuid: organization.Uuid, Id: "development", DisplayName: "Development"})
	require.NoError(t, err)
	environment, err := db.CreateEnvironment(t.Context(), nil, &Environment{OrgId: org, OrgUuid: organization.Uuid, ProjectId: project.Id, ProjectUuid: project.Uuid, Id: "development", EnvTypeId: envType.Id, EnvTypeUuid: envType.Uuid, CreatedAt: now, UpdatedAt: now, Status: EnvironmentStatusActive})
	require.NoError(t, err)
	return ExternalGrantCommand{OrgID: org, PluginID: "dev.example.workflow", OperationID: uuid.New(), ActorID: actor, SourceOperation: "prepareWorkflow", SourceDigest: strings.Repeat("d", 64), RequestID: "prepare-1", Action: "prepare", Reason: "Freeze reviewed intent", Intent: &ExternalOperationIntent{Fingerprint: strings.Repeat("a", 64), Modules: []uuid.UUID{modules[0].ModuleUUID, modules[1].ModuleUUID}, TargetVersions: map[string]uuid.UUID{modules[0].ModuleUUID.String(): modules[0].VersionUUID, modules[1].ModuleUUID.String(): modules[1].VersionUUID}, Environments: []ExternalOperationEnvironment{{ProjectID: project.Uuid, EnvironmentID: environment.Uuid}}, Actions: []string{"external-deployment.admit", "external-deployment.rollback"}}}
}

func TestExternalGrantPersistenceFencingAndDispatch(t *testing.T) {
	db := externalOperationDatabase(t)
	command := externalGrantFixture(t, db)
	ctx := t.Context()
	grant, err := ApplyExternalGrantCommand(ctx, db, command)
	require.NoError(t, err)
	require.Equal(t, int64(1), grant.Revision)
	require.Equal(t, "prepared", grant.State)
	replayed, err := ApplyExternalGrantCommand(ctx, db, command)
	require.NoError(t, err)
	require.Equal(t, grant, replayed)
	otherOperation := command
	otherOperation.OperationID = uuid.New()
	_, err = ApplyExternalGrantCommand(ctx, db, otherOperation)
	require.Error(t, err, "one original source request cannot authorize a second operation")
	_, err = GetExternalOperationGrant(ctx, db, otherOperation.OrgID, otherOperation.PluginID, otherOperation.OperationID)
	require.Error(t, err)
	wrongActor := command
	wrongActor.ActorID = uuid.New()
	_, err = ApplyExternalGrantCommand(ctx, db, wrongActor)
	require.ErrorContains(t, err, "another command or actor")
	control := command
	control.Intent = nil
	control.SourceOperation = "controlWorkflow"
	control.Action, control.RequestID, control.ExpectedRevision = "activate", "start-1", 1
	grant, err = ApplyExternalGrantCommand(ctx, db, control)
	require.NoError(t, err)
	dispatch := ExternalDispatch{OrgID: command.OrgID, PluginID: command.PluginID, OperationID: command.OperationID, ActorID: command.ActorID, Fence: grant.Revision, Action: "external-deployment.admit", DispatchID: "environment-1", ProjectUUID: command.Intent.Environments[0].ProjectID, EnvironmentUUID: command.Intent.Environments[0].EnvironmentID, RequestDigest: strings.Repeat("b", 64)}
	dispatch.TargetVersions = grant.Intent.TargetVersions
	require.ErrorContains(t, CheckExternalDispatch(ctx, db, dispatch), "was not authorized")
	require.NoError(t, RecordExternalDispatch(ctx, db, dispatch))
	require.NoError(t, RecordExternalDispatch(ctx, db, dispatch), "lost acknowledgement is idempotent")
	check := dispatch
	check.TargetVersions = nil
	require.NoError(t, CheckExternalDispatch(ctx, db, check))
	changed := dispatch
	changed.RequestDigest = strings.Repeat("c", 64)
	require.ErrorContains(t, RecordExternalDispatch(ctx, db, changed), "different exact request")
	changed = dispatch
	changed.EnvironmentUUID = uuid.New()
	require.ErrorContains(t, RecordExternalDispatch(ctx, db, changed), "outside the frozen")
	changed = dispatch
	changed.Action = "module.version.unpin"
	require.ErrorContains(t, RecordExternalDispatch(ctx, db, changed), "outside the frozen")
	control.Action, control.RequestID, control.ExpectedRevision = "suspend", "pause-1", grant.Revision
	grant, err = ApplyExternalGrantCommand(ctx, db, control)
	require.NoError(t, err)
	require.ErrorContains(t, CheckExternalDispatch(ctx, db, dispatch), "not active")
	control.Action, control.RequestID, control.ExpectedRevision = "activate", "resume-1", grant.Revision
	grant, err = ApplyExternalGrantCommand(ctx, db, control)
	require.NoError(t, err)
	require.ErrorContains(t, CheckExternalDispatch(ctx, db, dispatch), "not active", "resuming never revives old worker fences")
	dispatch.Fence = grant.Revision
	require.ErrorContains(t, RecordExternalDispatch(ctx, db, dispatch), "different exact request", "dispatch IDs remain reserved across resumes")
	dispatch.DispatchID = "environment-1-reconciled"
	require.ErrorContains(t, RecordExternalDispatch(ctx, db, dispatch), "unresolved", "resume cannot free an uncertain dispatch")
	require.NoError(t, CompleteExternalDispatch(ctx, db, command.OrgID, command.PluginID, command.OperationID, "environment-1", ExternalDispatchCompletion{DeploymentID: uuid.New(), Status: "succeeded", EnvironmentUUID: dispatch.EnvironmentUUID, ProjectUUID: dispatch.ProjectUUID, RequestDigest: strings.Repeat("b", 64)}))
	require.NoError(t, RecordExternalDispatch(ctx, db, dispatch))
	// Reconnect through the actual application database constructor. Recovery
	// cannot depend on the original connection pool or an in-memory session.
	var schema string
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema))
	require.NoError(t, tx.Rollback())
	reconnected, err := NewDatabaser(ctx, zap.NewNop(), os.Getenv("PLUGIN_TEST_DATABASE_URL")+" search_path="+schema, WithExtensionResourceJournal(journalEveryOrganization))
	require.NoError(t, err)
	defer func() { require.NoError(t, reconnected.Close()) }()
	loaded, err := GetExternalOperationGrant(ctx, reconnected, command.OrgID, command.PluginID, command.OperationID)
	require.NoError(t, err)
	require.Equal(t, grant, loaded)
	check = dispatch
	check.TargetVersions = nil
	require.NoError(t, CheckExternalDispatch(ctx, reconnected, check))
	_, err = GetExternalOperationGrant(ctx, db, command.OrgID, "dev.example.other", command.OperationID)
	require.Error(t, err)
	control.Action, control.RequestID, control.ExpectedRevision = "release", "finish-1", grant.Revision
	_, err = ApplyExternalGrantCommand(ctx, db, control)
	require.NoError(t, err)
	require.Error(t, CheckExternalDispatch(ctx, db, dispatch))
}

func TestExternalModuleOperationsAreBoundToCurrentGrantAndCapturedDefaults(t *testing.T) {
	db := externalOperationDatabase(t)
	ctx := t.Context()
	prepare := externalGrantFixture(t, db)
	prepare.Intent.Actions = []string{"module.reserve", "module.promote", "module.restore", "module.release"}
	grant, err := ApplyExternalGrantCommand(ctx, db, prepare)
	require.NoError(t, err)
	command := ExternalModuleCommand{OrgID: grant.OrgID, PluginID: grant.PluginID, OperationID: grant.OperationID, ActorID: grant.ActorID, RequestID: "reserve", Action: "reserve", GrantFence: grant.Revision, Reason: "Freeze exact Modules"}
	for _, moduleID := range grant.Intent.Modules {
		version, err := db.GetCoreModuleVersion(ctx, nil, grant.OrgID, moduleID.String(), grant.Intent.TargetVersions[moduleID.String()].String(), GetModeDefault)
		require.NoError(t, err)
		command.Targets = append(command.Targets, ExternalModuleTarget{ModuleUUID: moduleID, VersionUUID: version.UUID, ExpectedResourceVersion: version.ResourceVersion, ExpectedDefaultUUID: grant.BaselineDefaults[moduleID.String()]})
	}
	receipt, err := ApplyExternalModuleOperation(ctx, db, command)
	require.NoError(t, err)
	command.Action, command.RequestID, command.ExpectedFence = "promote", "promote", receipt.Fence
	_, err = ApplyExternalModuleOperation(ctx, db, command)
	require.ErrorContains(t, err, "this state", "prepared is not authority to promote")
	control := prepare
	control.Intent = nil
	control.Action, control.RequestID, control.ExpectedRevision = "activate", "activate", grant.Revision
	grant, err = ApplyExternalGrantCommand(ctx, db, control)
	require.NoError(t, err)
	_, err = ApplyExternalModuleOperation(ctx, db, command)
	require.ErrorContains(t, err, "current operation grant", "old fences never become usable")
	command.GrantFence = grant.Revision
	changed := command
	changed.Targets = append([]ExternalModuleTarget{}, command.Targets...)
	changed.Targets[0].VersionUUID = uuid.New()
	_, err = ApplyExternalModuleOperation(ctx, db, changed)
	require.ErrorContains(t, err, "frozen operation grant")
	changed.Targets[0] = command.Targets[0]
	changed.Targets[0].ExpectedDefaultUUID = nil
	_, err = ApplyExternalModuleOperation(ctx, db, changed)
	require.ErrorContains(t, err, "captured Default")
	_, err = ApplyExternalModuleOperation(ctx, db, command)
	require.NoError(t, err)
	_, err = ApplyExternalModuleOperation(ctx, db, command)
	require.NoError(t, err, "authorized lost-ack replay")
	restore := command
	restore.Action, restore.RequestID = "restore", "restore"
	restore.Targets = append([]ExternalModuleTarget{}, command.Targets...)
	for i, target := range command.Targets {
		base := grant.BaselineDefaults[target.ModuleUUID.String()]
		require.NotNil(t, base)
		version, err := db.GetCoreModuleVersion(ctx, nil, grant.OrgID, target.ModuleUUID.String(), base.String(), GetModeDefault)
		require.NoError(t, err)
		restore.Targets[i].VersionUUID = *base
		restore.Targets[i].ExpectedResourceVersion = 0
		require.Positive(t, version.ResourceVersion, "Core derives the locked post-promotion baseline revision")
		promoted := target.VersionUUID
		restore.Targets[i].ExpectedDefaultUUID = &promoted
	}
	changed = restore
	changed.Targets = append([]ExternalModuleTarget{}, restore.Targets...)
	changed.Targets[0].VersionUUID = uuid.New()
	_, err = ApplyExternalModuleOperation(ctx, db, changed)
	require.ErrorContains(t, err, "exact Default promotion")
	_, err = ApplyExternalModuleOperation(ctx, db, restore)
	require.NoError(t, err)
	control.Action, control.RequestID, control.ExpectedRevision = "suspend", "suspend", grant.Revision
	grant, err = ApplyExternalGrantCommand(ctx, db, control)
	require.NoError(t, err)
	_, err = ApplyExternalModuleOperation(ctx, db, command)
	require.Error(t, err, "receipt replay cannot bypass a revoked grant")
	command.Action, command.RequestID, command.GrantFence = "release", "release", grant.Revision
	_, err = ApplyExternalModuleOperation(ctx, db, command)
	require.NoError(t, err, "suspension permits releasing reservations but not promotion")
}

func TestReleasedGrantCanFinishOwnedModuleReservationRelease(t *testing.T) {
	db := externalOperationDatabase(t)
	ctx := t.Context()
	prepare := externalGrantFixture(t, db)
	prepare.Intent.Actions = []string{"module.reserve", "module.release", "module.promote", "module.restore"}
	grant, err := ApplyExternalGrantCommand(ctx, db, prepare)
	require.NoError(t, err)
	require.Equal(t, grantStatePrepared, grant.State)

	command := ExternalModuleCommand{
		OrgID: grant.OrgID, PluginID: grant.PluginID, OperationID: grant.OperationID,
		ActorID: grant.ActorID, RequestID: "reserve-modules", Action: externalActionReserve,
		GrantFence: grant.Revision, Reason: "Reserve the exact reviewed module set",
	}
	for _, moduleID := range grant.Intent.Modules {
		version, err := db.GetCoreModuleVersion(ctx, nil, grant.OrgID, moduleID.String(), grant.Intent.TargetVersions[moduleID.String()].String(), GetModeDefault)
		require.NoError(t, err)
		command.Targets = append(command.Targets, ExternalModuleTarget{
			ModuleUUID: moduleID, VersionUUID: version.UUID,
			ExpectedResourceVersion: version.ResourceVersion,
			ExpectedDefaultUUID:     grant.BaselineDefaults[moduleID.String()],
		})
	}
	reserved, err := ApplyExternalModuleOperation(ctx, db, command)
	require.NoError(t, err)
	require.Equal(t, int64(1), reserved.Fence)
	require.Equal(t, externalStateActive, reserved.State)

	grantCommand := prepare
	grantCommand.Intent = nil
	grantCommand.Action, grantCommand.RequestID = "release", "return-to-draft"
	grantCommand.ExpectedRevision = grant.Revision
	releasedGrant, err := ApplyExternalGrantCommand(ctx, db, grantCommand)
	require.NoError(t, err)
	require.Equal(t, int64(2), releasedGrant.Revision)
	require.Equal(t, grantStateReleased, releasedGrant.State)

	release := command
	release.RequestID, release.Action = "release-modules", externalActionRelease
	release.GrantFence, release.ExpectedFence = releasedGrant.Revision, reserved.Fence

	wrongActor := release
	wrongActor.ActorID = uuid.New()
	wrongActor.RequestID = "release-wrong-actor"
	_, err = ApplyExternalModuleOperation(ctx, db, wrongActor)
	require.ErrorContains(t, err, "current operation grant")

	staleGrant := release
	staleGrant.GrantFence--
	staleGrant.RequestID = "release-stale-grant"
	_, err = ApplyExternalModuleOperation(ctx, db, staleGrant)
	require.ErrorContains(t, err, "current operation grant")

	staleOperation := release
	staleOperation.ExpectedFence--
	staleOperation.RequestID = "release-stale-operation"
	_, err = ApplyExternalModuleOperation(ctx, db, staleOperation)
	require.ErrorContains(t, err, "operation fence changed")

	wrongModule := release
	wrongModule.Targets = append([]ExternalModuleTarget{}, release.Targets...)
	wrongModule.Targets[0].ModuleUUID = uuid.New()
	wrongModule.RequestID = "release-wrong-module"
	_, err = ApplyExternalModuleOperation(ctx, db, wrongModule)
	require.ErrorContains(t, err, "module or target version differs from the frozen operation grant")

	for _, action := range []string{externalActionReserve, externalActionPromote, externalActionRestore} {
		forbidden := release
		forbidden.Action = action
		forbidden.RequestID = "released-grant-" + action
		_, err = ApplyExternalModuleOperation(ctx, db, forbidden)
		require.ErrorContains(t, err, "operation grant cannot mutate module reservations or defaults in this state")
	}
	for _, target := range release.Targets {
		active, err := db.ListModuleOperationReservations(ctx, nil, grant.OrgID, target.ModuleUUID, false)
		require.NoError(t, err)
		require.Len(t, active, 1, "failed recovery attempts must leave the exact reservation active")
		module, err := db.GetModuleCatalogue(ctx, nil, grant.OrgID, target.ModuleUUID.String(), GetModeDefault)
		require.NoError(t, err)
		require.Equal(t, grant.BaselineDefaults[target.ModuleUUID.String()], module.CurrentDefaultVersionUUID, "cleanup-only recovery must not change Module Defaults")
	}

	receipt, err := ApplyExternalModuleOperation(ctx, db, release)
	require.NoError(t, err)
	require.Equal(t, externalActionRelease, receipt.Action)
	require.Equal(t, externalStateReleased, receipt.State)
	require.Equal(t, reserved.Fence, receipt.Fence)
	replayed, err := ApplyExternalModuleOperation(ctx, db, release)
	require.NoError(t, err, "lost acknowledgement must replay the same release receipt")
	require.Equal(t, receipt, replayed)

	for _, target := range release.Targets {
		active, err := db.ListModuleOperationReservations(ctx, nil, grant.OrgID, target.ModuleUUID, false)
		require.NoError(t, err)
		require.Empty(t, active)
		history, err := db.ListModuleOperationReservations(ctx, nil, grant.OrgID, target.ModuleUUID, true)
		require.NoError(t, err)
		require.Len(t, history, 1)
		require.NotNil(t, history[0].ReleasedAt)
	}
}

func TestExternalGrantConcurrencyAndFrozenBounds(t *testing.T) {
	db := externalOperationDatabase(t)
	command := externalGrantFixture(t, db)
	var group sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := ApplyExternalGrantCommand(t.Context(), db, command)
			results <- err
		}()
	}
	group.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	control := command
	control.Intent = nil
	control.ExpectedRevision = 1
	control.Action = "activate"
	results = make(chan error, 2)
	for _, requestID := range []string{"start-a", "start-b"} {
		group.Add(1)
		go func(requestID string) {
			defer group.Done()
			c := control
			c.RequestID = requestID
			_, err := ApplyExternalGrantCommand(t.Context(), db, c)
			results <- err
		}(requestID)
	}
	group.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	require.Equal(t, 1, successes)
	altered := command
	altered.RequestID = "rewrite"
	_, err := ApplyExternalGrantCommand(t.Context(), db, altered)
	require.ErrorContains(t, err, "already frozen")
	altered.OperationID = uuid.New()
	copyIntent := *command.Intent
	copyIntent.Environments = []ExternalOperationEnvironment{{ProjectID: uuid.New(), EnvironmentID: command.Intent.Environments[0].EnvironmentID}}
	altered.Intent = &copyIntent
	_, err = ApplyExternalGrantCommand(t.Context(), db, altered)
	require.ErrorContains(t, err, "does not belong")
	_, err = GetExternalOperationGrant(t.Context(), db, altered.OrgID, altered.PluginID, altered.OperationID)
	require.Error(t, err, "failed validation must create neither grant nor command receipt")
}
