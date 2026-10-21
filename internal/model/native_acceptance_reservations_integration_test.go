//go:build integration

package model

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/moduleversions"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
	"github.com/stretchr/testify/require"
)

func TestDisjointModuleOperationsShareEnvironmentWithoutSharingReservations(t *testing.T) {
	db := externalOperationDatabase(t)
	ctx := t.Context()
	fixture := externalGrantFixture(t, db)
	reserve := func(index int, requestID string) (ExternalGrantCommand, *ExternalModuleReceipt) {
		t.Helper()
		command := fixture
		command.OperationID, command.RequestID, command.SourceDigest = uuid.New(), requestID, strings.Repeat([]string{"a", "b"}[index], 64)
		moduleID := fixture.Intent.Modules[index]
		command.Intent = &ExternalOperationIntent{Fingerprint: fixture.Intent.Fingerprint,
			Modules: []uuid.UUID{moduleID}, TargetVersions: map[string]uuid.UUID{moduleID.String(): fixture.Intent.TargetVersions[moduleID.String()]},
			Environments: fixture.Intent.Environments, Actions: []string{"module.reserve", "module.release"}}
		grant, err := ApplyExternalGrantCommand(ctx, db, command)
		require.NoError(t, err)
		version, err := db.GetCoreModuleVersion(ctx, nil, command.OrgID, moduleID.String(), command.Intent.TargetVersions[moduleID.String()].String(), GetModeDefault)
		require.NoError(t, err)
		receipt, err := ApplyExternalModuleOperation(ctx, db, ExternalModuleCommand{OrgID: command.OrgID, PluginID: command.PluginID,
			OperationID: command.OperationID, ActorID: command.ActorID, RequestID: "reserve", Action: "reserve", GrantFence: grant.Revision,
			Reason: "Disjoint Module on the same Environment", Targets: []ExternalModuleTarget{{ModuleUUID: moduleID, VersionUUID: version.UUID,
				ExpectedResourceVersion: version.ResourceVersion, ExpectedDefaultUUID: grant.BaselineDefaults[moduleID.String()]}}})
		require.NoError(t, err)
		return command, receipt
	}
	first, firstReceipt := reserve(0, "prepare-alpha")
	second, secondReceipt := reserve(1, "prepare-bravo")
	require.Equal(t, first.Intent.Environments, second.Intent.Environments)
	require.NotEqual(t, first.OperationID, second.OperationID)
	require.Equal(t, "active", firstReceipt.State)
	require.Equal(t, "active", secondReceipt.State)
	for index, command := range []ExternalGrantCommand{first, second} {
		reservations, err := db.ListModuleOperationReservations(ctx, nil, fixture.OrgID, fixture.Intent.Modules[index], false)
		require.NoError(t, err)
		require.Len(t, reservations, 1)
		require.Equal(t, command.OperationID.String(), reservations[0].RelatedResourceID)
		require.Equal(t, command.PluginID, reservations[0].Namespace)
		require.Equal(t, uuid.NewSHA1(command.OperationID, []byte(fmt.Sprintf("%s/%s/%d", command.PluginID, fixture.Intent.Modules[index], 1))), reservations[0].OperationID)
	}
	// Overlap is still excluded, even though sharing the Environment is allowed.
	competitor := first
	competitor.OperationID, competitor.RequestID, competitor.SourceDigest = uuid.New(), "prepare-overlap", strings.Repeat("c", 64)
	grant, err := ApplyExternalGrantCommand(ctx, db, competitor)
	require.NoError(t, err)
	version, err := db.GetCoreModuleVersion(ctx, nil, first.OrgID, first.Intent.Modules[0].String(), first.Intent.TargetVersions[first.Intent.Modules[0].String()].String(), GetModeDefault)
	require.NoError(t, err)
	_, err = ApplyExternalModuleOperation(ctx, db, ExternalModuleCommand{OrgID: first.OrgID, PluginID: first.PluginID,
		OperationID: competitor.OperationID, ActorID: first.ActorID, RequestID: "reserve", Action: "reserve", GrantFence: grant.Revision,
		Reason: "Reject overlapping Module", Targets: []ExternalModuleTarget{{ModuleUUID: first.Intent.Modules[0], VersionUUID: version.UUID,
			ExpectedResourceVersion: version.ResourceVersion, ExpectedDefaultUUID: grant.BaselineDefaults[first.Intent.Modules[0].String()]}}})
	conflict, isConflict := IsErrConflict(err)
	require.True(t, isConflict, "overlap must fail as a reservation conflict: %v", err)
	require.Equal(t, "Module is reserved by "+first.PluginID+"/"+first.OperationID.String(), conflict.Message)
	for index, command := range []ExternalGrantCommand{first, second} {
		reservations, err := db.ListModuleOperationReservations(ctx, nil, fixture.OrgID, fixture.Intent.Modules[index], false)
		require.NoError(t, err)
		require.Len(t, reservations, 1)
		require.Equal(t, command.OperationID.String(), reservations[0].RelatedResourceID)
		require.Equal(t, command.PluginID, reservations[0].Namespace)
		require.Equal(t, uuid.NewSHA1(command.OperationID, []byte(fmt.Sprintf("%s/%s/%d", command.PluginID, fixture.Intent.Modules[index], 1))), reservations[0].OperationID)
	}
}

func TestNoPromotionReleasePreservesDefaultAndOccupiedProposedSlot(t *testing.T) {
	for _, targetVersion := range []string{"1.1.0", "1.1.0-rc.1"} {
		t.Run(targetVersion, func(t *testing.T) {
			assertNoPromotionReleasePreservesDefaultAndOccupiedProposedSlot(t, targetVersion)
		})
	}
}

func assertNoPromotionReleasePreservesDefaultAndOccupiedProposedSlot(t *testing.T, targetVersion string) {
	t.Helper()
	db := externalOperationDatabase(t)
	ctx := t.Context()
	prepare := externalGrantFixtureWithTarget(t, db, targetVersion)
	prepare.Intent.Actions = []string{"module.reserve", "module.release"}
	grant, err := ApplyExternalGrantCommand(ctx, db, prepare)
	require.NoError(t, err)
	command := ExternalModuleCommand{OrgID: grant.OrgID, PluginID: grant.PluginID, OperationID: grant.OperationID,
		ActorID: grant.ActorID, RequestID: "reserve", Action: "reserve", GrantFence: grant.Revision, Reason: "No-promotion lifecycle"}
	before := make([]CoreModuleVersion, 0, len(grant.Intent.Modules))
	for _, moduleID := range grant.Intent.Modules {
		version, err := db.GetCoreModuleVersion(ctx, nil, grant.OrgID, moduleID.String(), grant.Intent.TargetVersions[moduleID.String()].String(), GetModeDefault)
		require.NoError(t, err)
		before = append(before, *version)
		command.Targets = append(command.Targets, ExternalModuleTarget{ModuleUUID: moduleID, VersionUUID: version.UUID,
			ExpectedResourceVersion: version.ResourceVersion, ExpectedDefaultUUID: grant.BaselineDefaults[moduleID.String()]})
	}
	reserved, err := ApplyExternalModuleOperation(ctx, db, command)
	require.NoError(t, err)
	control := prepare
	control.Intent = nil
	control.Action, control.RequestID, control.ExpectedRevision = "activate", "activate", grant.Revision
	grant, err = ApplyExternalGrantCommand(ctx, db, control)
	require.NoError(t, err)
	command.Action, command.RequestID, command.ExpectedFence, command.GrantFence = "release", "release", reserved.Fence, grant.Revision
	_, err = ApplyExternalModuleOperation(ctx, db, command)
	require.NoError(t, err)
	for index, moduleID := range grant.Intent.Modules {
		target, err := db.GetCoreModuleVersion(ctx, nil, grant.OrgID, moduleID.String(), before[index].UUID.String(), GetModeDefault)
		require.NoError(t, err)
		require.Equal(t, before[index], *target, "release cannot change the Proposed Version")
		base, err := db.GetCoreModuleVersion(ctx, nil, grant.OrgID, moduleID.String(), grant.BaselineDefaults[moduleID.String()].String(), GetModeDefault)
		require.NoError(t, err)
		require.Equal(t, moduleversions.LifecycleDefault, base.LifecycleStatus)
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = db.PublishCoreModuleVersion(ctx, tx, &ModuleDefinitionVersion{OrgId: grant.OrgID, DefinitionId: target.ModuleSlug,
			SemanticVersion: "1.2.0", ModuleSource: "inline", ModuleSourceCode: opt.Of(`output "name" { value = "next" }`),
			OutputSchema: map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}},
			ModuleInputs: map[string]any{}, ModuleParams: map[string]ModuleParam{}, ProviderMapping: map[string]string{},
			Dependencies: map[string]ModuleDefinitionDependency{}, CoProvisioned: []ModuleDefinitionCoProvision{}}, grant.ActorID)
		require.ErrorContains(t, err, "already has a Proposed version")
		require.NoError(t, tx.Rollback())
	}
}
