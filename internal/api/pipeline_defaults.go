package api

import (
	"github.com/google/uuid"
	p "github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/pipelineapi"
)

func setEffectivePipelineDefaults(choices *p.PipelineChoices, environment uuid.UUID) {
	forward, source := publishedPipelineDefault(choices, choices.Policy.DefaultVersionId, choices.Policy.EnvironmentDefaultVersionIds, environment)
	rollback := p.PipelineRollbackDefaults{}
	if choices.Policy.RollbackDefaults != nil {
		rollback = *choices.Policy.RollbackDefaults
	}
	rollbackRef, rollbackSource := publishedPipelineDefault(choices, rollback.DefaultVersionId, rollback.EnvironmentDefaultVersionIds, environment)
	if !choices.Policy.Enabled {
		builtin := p.BuiltinReference()
		forward, rollbackRef = &builtin, &builtin
		source, rollbackSource = string(p.PipelineChoicesDefaultSourceNone), string(p.PipelineChoicesDefaultSourceNone)
	}
	defaultSource := p.PipelineChoicesDefaultSource(source)
	defaultRollbackSource := p.PipelineChoicesRollbackDefaultSource(rollbackSource)
	choices.EffectiveDefault, choices.DefaultSource = forward, &defaultSource
	choices.EffectiveRollbackDefault, choices.RollbackDefaultSource = rollbackRef, &defaultRollbackSource
}

func publishedPipelineDefault(choices *p.PipelineChoices, projectDefault *uuid.UUID, environments map[string]uuid.UUID, environment uuid.UUID) (*p.PipelineReference, string) {
	selected, source := projectDefault, string(p.PipelineChoicesDefaultSourceNone)
	if id, exists := environments[environment.String()]; exists {
		selected, source = &id, string(p.PipelineChoicesDefaultSourceEnvironment)
	} else if selected != nil {
		source = string(p.PipelineChoicesDefaultSourceProject)
	}
	if selected != nil {
		for _, version := range choices.Versions {
			if version.Id == *selected {
				return &p.PipelineReference{VersionId: selected, Digest: version.Digest}, source
			}
		}
	}
	return nil, source
}
