package model

import (
	"encoding/json"
	"github.com/google/uuid"
	p "github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/pipelineapi"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestExternalPipelineIntentRequiresReviewedRollbackChoice(t *testing.T) {
	env := ExternalOperationEnvironment{ProjectID: uuid.New(), EnvironmentID: uuid.New()}
	forwardOnly := false
	binding := ExternalPipelineBinding{ProjectUUID: env.ProjectID, EnvironmentUUID: env.EnvironmentID, Forward: p.BuiltinReference()}
	intent := ExternalOperationIntent{ForwardOnly: &forwardOnly, Environments: []ExternalOperationEnvironment{env}, Pipelines: []ExternalPipelineBinding{binding}}
	require.ErrorContains(t, validateExternalPipelineIntent(intent), "rollback")
	forwardOnly = true
	require.NoError(t, validateExternalPipelineIntent(intent))
	forwardOnly = false
	rollback := p.BuiltinReference()
	intent.Pipelines[0].Rollback = &rollback
	require.NoError(t, validateExternalPipelineIntent(intent))
	intent.Pipelines = append(intent.Pipelines, intent.Pipelines[0])
	require.ErrorContains(t, validateExternalPipelineIntent(intent), "unique")
	legacy := ExternalOperationIntent{}
	encoded, err := json.Marshal(legacy)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "pipelines")
	require.NotContains(t, string(encoded), "forward_only")
}
func TestExternalDispatchCannotSubstituteOperationPipeline(t *testing.T) {
	environment := ExternalOperationEnvironment{ProjectID: uuid.New(), EnvironmentID: uuid.New()}
	forwardOnly := false
	ref := p.BuiltinReference()
	intent := ExternalOperationIntent{ForwardOnly: &forwardOnly, Environments: []ExternalOperationEnvironment{environment}, Actions: []string{"external-deployment.admit", "external-deployment.rollback"}, Pipelines: []ExternalPipelineBinding{{ProjectUUID: environment.ProjectID, EnvironmentUUID: environment.EnvironmentID, Forward: ref, Rollback: &ref}}}
	grant := &ExternalOperationGrant{Revision: 2, Intent: intent}
	dispatch := ExternalDispatch{Fence: 2, ProjectUUID: environment.ProjectID, EnvironmentUUID: environment.EnvironmentID, Action: "external-deployment.admit", DispatchID: "original", RequestDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Pipeline: &ref}
	require.NoError(t, validateExternalDispatchScope(grant, dispatch))
	replaced := ref
	replaced.Digest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	dispatch.Pipeline = &replaced
	require.ErrorContains(t, validateExternalDispatchScope(grant, dispatch), "frozen")
	dispatch.Pipeline = nil
	require.ErrorContains(t, validateExternalDispatchScope(grant, dispatch), "frozen")
	grant.Intent.ForwardOnly = nil
	grant.Intent.Pipelines = nil
	require.NoError(t, validateExternalDispatchScope(grant, dispatch), "legacy dispatch identities keep their original protocol")
	dispatch.Pipeline = &ref
	require.ErrorContains(t, validateExternalDispatchScope(grant, dispatch), "newly prepared")
}
