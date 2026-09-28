//go:build integration

package model

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/moduleversions"
	"github.com/stretchr/testify/require"
)

func externalPinFixture(t *testing.T, db Databaser) (ExternalGrantCommand, *ExternalOperationGrant, ExternalPinCommand) {
	t.Helper()
	prepare := externalGrantFixture(t, db)
	prepare.Intent.Actions = []string{"pin.override.begin", "pin.override.reconcile", "pin.override.restore", "pin.override.cancel", "external-deployment.admit"}
	grant, err := ApplyExternalGrantCommand(t.Context(), db, prepare)
	require.NoError(t, err)
	control := prepare
	control.Intent = nil
	control.Action, control.RequestID, control.ExpectedRevision = "activate", "start", grant.Revision
	grant, err = ApplyExternalGrantCommand(t.Context(), db, control)
	require.NoError(t, err)
	command := ExternalPinCommand{OrgID: grant.OrgID, PluginID: grant.PluginID, OperationID: grant.OperationID, GrantFence: grant.Revision, ActorID: grant.ActorID, Action: "begin", RequestID: "pin-begin", Reason: "Explicitly approve exact Pin overrides"}
	env := grant.Intent.Environments[0]
	for _, module := range grant.Intent.Modules {
		tx, err := db.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		pin, err := db.CreateEnvironmentModuleVersionPin(t.Context(), tx, grant.OrgID, env.ProjectID, "project", env.EnvironmentID, "development", module, *grant.BaselineDefaults[module.String()], grant.ActorID, "user", "Protect current baseline", nil, false)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
		command.Targets = append(command.Targets, ExternalPinTarget{PinID: pin.ID, ExpectedResourceVersion: pin.ResourceVersion, TargetVersionUUID: grant.Intent.TargetVersions[module.String()]})
	}
	return control, grant, command
}

func TestExternalPinAtomicTransitionsWithTrustedEvidence(t *testing.T) {
	db := externalOperationDatabase(t)
	_, grant, command := externalPinFixture(t, db)
	invalid := command
	invalid.Targets = append([]ExternalPinTarget{}, command.Targets...)
	invalid.Targets[1].ExpectedResourceVersion++
	_, err := ApplyExternalPinCommand(t.Context(), db, invalid, nil)
	require.Error(t, err)
	for _, target := range command.Targets {
		pin, err := db.GetEnvironmentModuleVersionPin(t.Context(), nil, grant.OrgID, target.PinID, GetModeDefault)
		require.NoError(t, err)
		require.Equal(t, moduleversions.PinActive, pin.Status, "a failed bulk command must change no Pin")
	}
	pins, err := ApplyExternalPinCommand(t.Context(), db, command, nil)
	require.NoError(t, err)
	replayed, err := ApplyExternalPinCommand(t.Context(), db, command, nil)
	require.NoError(t, err)
	require.Equal(t, pins, replayed)
	other := command
	other.ActorID = uuid.New()
	_, err = ApplyExternalPinCommand(t.Context(), db, other, nil)
	require.Error(t, err, "idempotent receipts are actor-bound")
	activation := pins[0].ActivationEventID
	next := command
	next.Action, next.RequestID, next.Targets = "reconcile", "pin-failed", nil
	deployment := uuid.New()
	for _, pin := range pins {
		next.Targets = append(next.Targets, ExternalPinTarget{PinID: pin.ID, ExpectedResourceVersion: pin.ResourceVersion, DeploymentID: deployment})
	}
	// This model-level test supplies trusted evidence directly. Separate adapter
	// integration tests must prove its provenance from the real DP receipt.
	outcome, mode := "failed", "deploy"
	effective := map[uuid.UUID]uuid.UUID{}
	reader := func(_ context.Context, _ Tx, g *ExternalOperationGrant, pin EnvironmentModuleVersionPin, id uuid.UUID) (ExternalPinEvidence, error) {
		versions := map[uuid.UUID]uuid.UUID{}
		for _, module := range g.Intent.Modules {
			versions[module] = g.Intent.TargetVersions[module.String()]
			if mode == "rollback" {
				versions[module] = *g.BaselineDefaults[module.String()]
			}
		}
		return ExternalPinEvidence{OperationID: g.OperationID, EnvironmentUUID: pin.EnvironmentUUID, ProjectUUID: pin.ProjectUUID, DeploymentID: id, Status: outcome, Mode: mode, ModuleVersions: versions, EffectiveVersions: effective}, nil
	}
	pins, err = ApplyExternalPinCommand(t.Context(), db, next, reader)
	require.NoError(t, err)
	require.Equal(t, moduleversions.PinActive, pins[0].Status)
	require.Equal(t, activation, pins[0].ActivationEventID, "failed override must not create a new approval boundary")
	command.RequestID, command.Targets = "begin-again", nil
	for _, pin := range pins {
		command.Targets = append(command.Targets, ExternalPinTarget{PinID: pin.ID, ExpectedResourceVersion: pin.ResourceVersion, TargetVersionUUID: grant.Intent.TargetVersions[pin.ModuleUUID.String()]})
	}
	pins, err = ApplyExternalPinCommand(t.Context(), db, command, nil)
	require.NoError(t, err)
	next.RequestID, next.Targets, outcome = "pin-succeeded", nil, "succeeded"
	for _, pin := range pins {
		next.Targets = append(next.Targets, ExternalPinTarget{PinID: pin.ID, ExpectedResourceVersion: pin.ResourceVersion, DeploymentID: deployment})
	}
	_, err = ApplyExternalPinCommand(t.Context(), db, next, reader)
	require.ErrorContains(t, err, "exact override target", "a prior successful deployment is not current adoption")
	for _, module := range grant.Intent.Modules {
		effective[module] = grant.Intent.TargetVersions[module.String()]
	}
	pins, err = ApplyExternalPinCommand(t.Context(), db, next, reader)
	require.NoError(t, err)
	require.Equal(t, moduleversions.PinOverridden, pins[0].Status)
	next.Action, next.RequestID, next.Targets, mode = "restore", "pin-rollback", nil, "rollback"
	for _, pin := range pins {
		next.Targets = append(next.Targets, ExternalPinTarget{PinID: pin.ID, ExpectedResourceVersion: pin.ResourceVersion, DeploymentID: uuid.New()})
	}
	_, err = ApplyExternalPinCommand(t.Context(), db, next, reader)
	require.ErrorContains(t, err, "exact pinned version")
	for _, module := range grant.Intent.Modules {
		effective[module] = *grant.BaselineDefaults[module.String()]
	}
	pins, err = ApplyExternalPinCommand(t.Context(), db, next, reader)
	require.NoError(t, err)
	require.Equal(t, moduleversions.PinActive, pins[0].Status)
	require.NotEqual(t, activation, pins[0].ActivationEventID, "successful restoration creates a fresh approval boundary")
}

func TestExternalPinCancellationRequiresNoRecordedDispatch(t *testing.T) {
	db := externalOperationDatabase(t)
	control, grant, command := externalPinFixture(t, db)
	pins, err := ApplyExternalPinCommand(t.Context(), db, command, nil)
	require.NoError(t, err)
	cancel := command
	cancel.Action, cancel.RequestID, cancel.Targets = "cancel", "cancel-unstarted", nil
	for _, pin := range pins {
		cancel.Targets = append(cancel.Targets, ExternalPinTarget{PinID: pin.ID, ExpectedResourceVersion: pin.ResourceVersion})
	}
	pins, err = ApplyExternalPinCommand(t.Context(), db, cancel, nil)
	require.NoError(t, err)
	command.RequestID, command.Targets = "begin-second", nil
	for _, pin := range pins {
		command.Targets = append(command.Targets, ExternalPinTarget{PinID: pin.ID, ExpectedResourceVersion: pin.ResourceVersion, TargetVersionUUID: grant.Intent.TargetVersions[pin.ModuleUUID.String()]})
	}
	pins, err = ApplyExternalPinCommand(t.Context(), db, command, nil)
	require.NoError(t, err)
	env := grant.Intent.Environments[0]
	approved := map[string]uuid.UUID{}
	for _, pin := range pins {
		approved[pin.ModuleUUID.String()] = pin.ActivationEventID
	}
	require.NoError(t, RecordExternalDispatch(t.Context(), db, ExternalDispatch{OrgID: grant.OrgID, PluginID: grant.PluginID, OperationID: grant.OperationID, ActorID: grant.ActorID, Fence: grant.Revision, Action: "external-deployment.admit", DispatchID: "uncertain-response", ProjectUUID: env.ProjectID, EnvironmentUUID: env.EnvironmentID, RequestDigest: grant.Intent.Fingerprint, TargetVersions: grant.Intent.TargetVersions, ApprovedPins: approved}))
	control.Action, control.RequestID, control.ExpectedRevision = "suspend", "pause", grant.Revision
	grant, err = ApplyExternalGrantCommand(t.Context(), db, control)
	require.NoError(t, err)
	cancel.RequestID, cancel.GrantFence, cancel.Targets = "unsafe-cancel", grant.Revision, nil
	for _, pin := range pins {
		cancel.Targets = append(cancel.Targets, ExternalPinTarget{PinID: pin.ID, ExpectedResourceVersion: pin.ResourceVersion})
	}
	_, err = ApplyExternalPinCommand(t.Context(), db, cancel, nil)
	require.ErrorContains(t, err, "authoritative outcome", "suspension does not prove an in-flight deployment never started")
	command.GrantFence = grant.Revision
	_, err = ApplyExternalPinCommand(t.Context(), db, command, nil)
	require.ErrorContains(t, err, "active operation")
}
