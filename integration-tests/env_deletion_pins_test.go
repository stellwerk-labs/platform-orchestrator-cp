package integrationtests

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/moduleversions"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v2/genclient"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"
)

func TestEnvironmentDeletionTerminallyRemovesPinsExceptPendingOverrides(t *testing.T) {
	client := MustServerClient(t)
	internalClient := MustInternalServerClient(t)
	database := MustDatabaser(t)
	orgID := MustCreateOrg(t, internalClient).Id
	envType := MustCreateEnvType(t, client, orgID, "delete-pins-"+strings.ToLower(randToken(t)))
	project := MustCreateProject(t, client, orgID, "pin-delete-"+strings.ToLower(randToken(t)))

	createVersion := func(moduleID string) genclient.CoreModuleVersion {
		resourceType := MustCreateResourceType(t, client, orgID, moduleID+"-type")
		MustCreateEmptyModule(t, client, orgID, moduleID, resourceType.Id)
		response, err := client.GetModuleVersionWithResponse(t.Context(), orgID, moduleID, "1.0.0")
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, response.StatusCode(), string(response.Body))
		return response.JSON200.Version
	}
	createPin := func(env *genclient.Environment, version genclient.CoreModuleVersion) model.EnvironmentModuleVersionPin {
		tx, err := database.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		pin, err := database.CreateEnvironmentModuleVersionPin(t.Context(), tx, orgID, project.Uuid, project.Id,
			env.Uuid, env.Id, version.ModuleUuid, version.Uuid, userid.InternalSystemUuid, "user", "Protect deployed version", nil, false)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
		return *pin
	}
	transitionPin := func(pin model.EnvironmentModuleVersionPin, target moduleversions.PinStatus, eventType string, operationID, targetVersionID, deploymentID *uuid.UUID) model.EnvironmentModuleVersionPin {
		tx, err := database.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		updated, err := database.TransitionEnvironmentModuleVersionPin(t.Context(), tx, orgID, pin.ID, pin.ResourceVersion,
			target, eventType, userid.InternalSystemUuid, "addon", "Synthetic add-on state for deletion safety", operationID, targetVersionID, deploymentID)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
		return *updated
	}

	t.Run("active and overridden pins become retained tombstones", func(t *testing.T) {
		env := MustCreateEnv(t, client, orgID, envType.Id, project.Id, "retained-"+strings.ToLower(randToken(t)))
		activeVersion := createVersion("active-delete-" + strings.ToLower(randToken(t)))
		overriddenVersion := createVersion("overridden-delete-" + strings.ToLower(randToken(t)))
		activePin := createPin(env, activeVersion)
		overriddenPin := createPin(env, overriddenVersion)
		operationID, targetVersionID, deploymentID := uuid.New(), uuid.New(), uuid.New()
		overriddenPin = transitionPin(overriddenPin, moduleversions.PinOverridePending, "override_requested", &operationID, &targetVersionID, nil)
		overriddenPin = transitionPin(overriddenPin, moduleversions.PinOverridden, "override_succeeded", &operationID, &targetVersionID, &deploymentID)

		response, err := internalClient.InternalForceDeleteEnvironmentWithResponse(t.Context(), orgID, project.Id, env.Id,
			&genclient.InternalForceDeleteEnvironmentParams{})
		require.NoError(t, err)
		require.Equal(t, http.StatusNoContent, response.StatusCode(), string(response.Body))

		pins, err := database.ListEnvironmentModuleVersionPins(t.Context(), nil, orgID, &env.Uuid, nil, true)
		require.NoError(t, err)
		require.Len(t, pins, 2)
		for _, pin := range pins {
			require.Equal(t, moduleversions.PinRemoved, pin.Status)
			require.NotNil(t, pin.RemovedAt)
			events, err := database.ListModuleVersionPinEvents(t.Context(), nil, orgID, pin.ID)
			require.NoError(t, err)
			require.Equal(t, "environment_deleted", events[len(events)-1].EventType)
			require.Equal(t, "environment_deleted", events[len(events)-1].Reason)
			require.Equal(t, userid.InternalSystemUuid, events[len(events)-1].Actor)
		}
		require.Contains(t, []uuid.UUID{pins[0].ID, pins[1].ID}, activePin.ID)
		require.Contains(t, []uuid.UUID{pins[0].ID, pins[1].ID}, overriddenPin.ID)
	})

	t.Run("override pending pin blocks deletion without changing state", func(t *testing.T) {
		env := MustCreateEnv(t, client, orgID, envType.Id, project.Id, "blocked-"+strings.ToLower(randToken(t)))
		version := createVersion("pending-delete-" + strings.ToLower(randToken(t)))
		pin := createPin(env, version)
		operationID, targetVersionID := uuid.New(), uuid.New()
		pending := transitionPin(pin, moduleversions.PinOverridePending, "override_requested", &operationID, &targetVersionID, nil)

		response, err := internalClient.InternalForceDeleteEnvironmentWithResponse(t.Context(), orgID, project.Id, env.Id,
			&genclient.InternalForceDeleteEnvironmentParams{})
		require.NoError(t, err)
		require.Equal(t, http.StatusConflict, response.StatusCode(), string(response.Body))

		current, err := database.GetEnvironmentModuleVersionPin(t.Context(), nil, orgID, pin.ID, model.GetModeDefault)
		require.NoError(t, err)
		require.Equal(t, moduleversions.PinOverridePending, current.Status)
		require.Equal(t, pending.ResourceVersion, current.ResourceVersion)
		events, err := database.ListModuleVersionPinEvents(t.Context(), nil, orgID, pin.ID)
		require.NoError(t, err)
		require.Equal(t, "override_requested", events[len(events)-1].EventType)
		envResponse, err := client.GetEnvironmentWithResponse(t.Context(), orgID, project.Id, env.Id)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, envResponse.StatusCode(), string(envResponse.Body))
	})
}

func randToken(t *testing.T) string {
	t.Helper()
	return strings.ToLower(strings.ReplaceAll(uuid.NewString(), "-", ""))
}
