package model

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/google/uuid"
	p "github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/pipelineapi"
)

func validateRollbackPipelineDefaults(ctx context.Context, tx Tx, project *Project, body p.PipelinePolicyWrite) ([]byte, error) {
	defaults := p.PipelineRollbackDefaults{}
	if body.RollbackDefaults != nil {
		defaults = *body.RollbackDefaults
	}
	if !body.Enabled && (defaults.DefaultVersionId != nil || len(defaults.EnvironmentDefaultVersionIds) > 0) {
		return nil, NewErrBadRequest("clear rollback defaults explicitly before disabling project pipeline policy")
	}
	if defaults.DefaultVersionId != nil && !slices.Contains(body.AllowedVersionIds, *defaults.DefaultVersionId) {
		return nil, NewErrBadRequest("rollback default requires an allowed published version")
	}
	for key := range defaults.EnvironmentDefaultVersionIds {
		id, err := uuid.Parse(key)
		if err != nil || id == uuid.Nil || key != id.String() {
			return nil, NewErrBadRequest("rollback environment defaults require canonical immutable environment UUIDs")
		}
	}
	_, err := validateEnvironmentPipelineDefaults(ctx, tx, project, p.PipelinePolicyWrite{
		AllowedVersionIds:            body.AllowedVersionIds,
		EnvironmentDefaultVersionIds: defaults.EnvironmentDefaultVersionIds,
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(defaults)
}
