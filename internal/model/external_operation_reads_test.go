package model

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestExternalOperationReadCannotReviveExecution(t *testing.T) {
	environment := ExternalOperationEnvironment{ProjectID: uuid.New(), EnvironmentID: uuid.New()}
	module, version := uuid.New(), uuid.New()
	grant := &ExternalOperationGrant{ActorID: uuid.New(), Revision: 4, Intent: ExternalOperationIntent{Actions: []string{"external-deployment.read", "external-deployment.readiness", "external-deployment.plan"}, Environments: []ExternalOperationEnvironment{environment}, TargetVersions: map[string]uuid.UUID{module.String(): version}}}
	for _, state := range []string{"prepared", "active", "suspended", "released"} {
		grant.State = state
		require.NoError(t, ValidateExternalOperationRead(grant, 4, "external-deployment.read", []ExternalOperationEnvironment{environment}, nil), "read after %s must not require reactivation", state)
		require.Error(t, ValidateExternalOperationRead(grant, 4, "external-deployment.admit", []ExternalOperationEnvironment{environment}, nil))
	}
	require.NoError(t, ValidateExternalOperationRead(grant, 4, "external-deployment.plan", []ExternalOperationEnvironment{environment}, map[uuid.UUID]uuid.UUID{module: version}))
	require.Error(t, ValidateExternalOperationRead(grant, 3, "external-deployment.read", []ExternalOperationEnvironment{environment}, nil))
	require.Error(t, ValidateExternalOperationRead(grant, 4, "external-deployment.read", []ExternalOperationEnvironment{{ProjectID: environment.ProjectID, EnvironmentID: uuid.New()}}, nil))
	require.Error(t, ValidateExternalOperationRead(grant, 4, "external-deployment.read", []ExternalOperationEnvironment{{ProjectID: uuid.New(), EnvironmentID: environment.EnvironmentID}}, nil))
	require.Error(t, ValidateExternalOperationRead(grant, 4, "external-deployment.plan", []ExternalOperationEnvironment{environment}, map[uuid.UUID]uuid.UUID{module: uuid.New()}))
	require.Error(t, ValidateExternalOperationRead(grant, 4, "external-deployment.read", nil, nil))
	grant.Intent.Actions = nil
	require.Error(t, ValidateExternalOperationRead(grant, 4, "external-deployment.read", []ExternalOperationEnvironment{environment}, nil))
}
