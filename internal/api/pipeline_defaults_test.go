package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	p "github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/pipelineapi"
	"github.com/stretchr/testify/require"
)

func TestPipelineDefaultsResolveBothDirectionsIndependently(t *testing.T) {
	environment := uuid.New()
	forward, rollback, environmentForward, environmentRollback := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	choices := p.PipelineChoices{Policy: p.PipelinePolicy{Enabled: true, DefaultVersionId: &forward,
		RollbackDefaults: &p.PipelineRollbackDefaults{DefaultVersionId: &rollback}},
		Versions: []p.PublishedPipelineVersion{{Id: forward, Digest: strings.Repeat("a", 64)}, {Id: rollback, Digest: strings.Repeat("b", 64)},
			{Id: environmentForward, Digest: strings.Repeat("c", 64)}, {Id: environmentRollback, Digest: strings.Repeat("d", 64)}}}
	setEffectivePipelineDefaults(&choices, environment)
	require.Equal(t, forward, *choices.EffectiveDefault.VersionId)
	require.Equal(t, rollback, *choices.EffectiveRollbackDefault.VersionId)
	require.Equal(t, strings.Repeat("b", 64), choices.EffectiveRollbackDefault.Digest)
	require.Equal(t, "project", string(*choices.DefaultSource))
	require.Equal(t, "project", string(*choices.RollbackDefaultSource))
	choices.Policy.EnvironmentDefaultVersionIds = map[string]uuid.UUID{environment.String(): environmentForward}
	choices.Policy.RollbackDefaults.EnvironmentDefaultVersionIds = map[string]uuid.UUID{environment.String(): environmentRollback}
	setEffectivePipelineDefaults(&choices, environment)
	require.Equal(t, environmentForward, *choices.EffectiveDefault.VersionId)
	require.Equal(t, environmentRollback, *choices.EffectiveRollbackDefault.VersionId)
	require.Equal(t, strings.Repeat("d", 64), choices.EffectiveRollbackDefault.Digest)
	require.Equal(t, "environment", string(*choices.DefaultSource))
	require.Equal(t, "environment", string(*choices.RollbackDefaultSource))
}

func TestMissingRollbackDefaultNeverReusesForward(t *testing.T) {
	forward := uuid.New()
	choices := p.PipelineChoices{Policy: p.PipelinePolicy{Enabled: true, DefaultVersionId: &forward},
		Versions: []p.PublishedPipelineVersion{{Id: forward, Digest: strings.Repeat("a", 64)}}}
	setEffectivePipelineDefaults(&choices, uuid.New())
	require.NotNil(t, choices.EffectiveDefault)
	require.Nil(t, choices.EffectiveRollbackDefault)
	require.Equal(t, "none", string(*choices.RollbackDefaultSource))
	choices.Policy.Enabled = false
	setEffectivePipelineDefaults(&choices, uuid.New())
	require.Equal(t, p.BuiltinReference(), *choices.EffectiveDefault)
	require.Equal(t, p.BuiltinReference(), *choices.EffectiveRollbackDefault)
	require.Equal(t, "none", string(*choices.RollbackDefaultSource))
}

func TestRollbackDefaultOptInPreservesLegacyRequestBytes(t *testing.T) {
	encoded, err := json.Marshal(PluginDeploymentReadRequest{UseRollbackDefault: false})
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "use_rollback_default")
	encoded, err = json.Marshal(PluginDeploymentReadRequest{UseRollbackDefault: true})
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"use_rollback_default":true`)
}
