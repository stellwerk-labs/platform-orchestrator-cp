package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	observation "github.com/stellwerk-labs/platform-orchestrator-cp/internal/observationapi"
	p "github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/pipelineapi"
	orchestratoriam "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"
	"slices"
)

func (s *Server) environmentPipelineChoices(ctx context.Context, tx model.Tx, env *model.Environment) (p.PipelineChoices, error) {
	project, err := s.Database.GetProject(ctx, tx, env.OrgId, env.ProjectId, model.GetModeDefault)
	if err != nil {
		return p.PipelineChoices{}, err
	}
	choices, err := readPipelineChoices(ctx, tx, project)
	if err != nil {
		return choices, err
	}
	setEffectivePipelineDefaults(&choices, env.Uuid)
	compatible, protocol, err := s.pipelineRunnerCompatibility(ctx, tx, env)
	if err != nil {
		return choices, err
	}
	eligibility := []p.PipelineEligibility{}
	for _, version := range choices.Versions {
		outcome := recipeCompatibility(version.Definition, compatible, protocol)
		eligibility = append(eligibility, p.PipelineEligibility{VersionId: version.Id, Deploy: outcome, Rollback: outcome})
	}
	choices.Eligibility = &eligibility
	return choices, nil
}
func (s *Server) pipelineRunnerCompatibility(ctx context.Context, tx model.Tx, env *model.Environment) (map[string]bool, string, error) {
	protocol := "stellwerk.pipeline/v1"
	id := env.RunnerId.Or("")
	if id == "" {
		matches, err := s.getMatchingRunners(ctx, tx, env.OrgId, env.ProjectId, env.EnvTypeId)
		if err != nil {
			return nil, protocol, err
		}
		if len(matches) == 0 {
			return map[string]bool{}, protocol, nil
		}
		id = matches[0].Rule.RunnerId
	}
	runner, err := s.Database.GetRunner(ctx, tx, env.OrgId, id, model.GetModeDefault)
	if err != nil {
		return nil, protocol, err
	}
	raw, err := json.Marshal(runner.RunnerConfiguration)
	if err != nil {
		return nil, protocol, err
	}
	var config struct {
		runnerExecutionConfiguration
		Job struct {
			Environment map[string]string `json:"environment"`
			Secrets     map[string]string `json:"secrets"`
			PodTemplate map[string]any    `json:"pod_template"`
		} `json:"job"`
	}
	if err = json.Unmarshal(raw, &config); err != nil {
		return nil, protocol, err
	}
	found := map[string]bool{}
	if env.Status != model.EnvironmentStatusActive || len(config.Job.Environment)+len(config.Job.Secrets)+len(config.Job.PodTemplate) > 0 {
		return found, protocol, nil
	}
	if runner.RunnerType == string(RunnerTypeNativeAgent) || config.Type == RunnerTypeNativeAgent {
		protocol = "stellwerk.pipeline/v2"
		if runner.RunnerType != string(RunnerTypeNativeAgent) || config.Type != RunnerTypeNativeAgent ||
			len(config.NativeExecution) == 0 || validateRunnerNativeExecution(runner.RunnerConfiguration) != nil {
			return found, protocol, nil
		}
	}
	for _, capability := range append(config.SupportedProtocols, config.Capabilities...) {
		found[capability] = true
	}
	return found, protocol, nil
}
func recipeCompatibility(recipe p.PipelineRecipe, available map[string]bool, protocol string) p.PipelineCompatibility {
	required := []string{protocol, "pipeline.recipe.v1"}
	for _, step := range recipe.Steps {
		for _, tool := range p.TrustedTools() {
			if tool.Id == string(step.ToolId) {
				required = append(required, tool.RequiredCapabilities...)
			}
		}
	}
	if recipe.Cost != nil && recipe.Cost.Enabled {
		required = append(required, "cost.infracost.v1")
		if !available["pricing.local"] {
			required = append(required, "network.pricing")
		}
	}
	for _, capability := range required {
		if !available[capability] {
			reason := "Runner lacks capability: " + capability
			return p.PipelineCompatibility{Compatible: false, Reason: &reason}
		}
	}
	return p.PipelineCompatibility{Compatible: true}
}
func pluginChoices(choices p.PipelineChoices) PluginPipelineChoices {
	result := PluginPipelineChoices{Protocol: StellwerkPluginPipelinesv1, DefaultSource: PluginPipelineChoicesDefaultSource(*choices.DefaultSource), Versions: []PluginPipelineChoice{}}
	if choices.EffectiveDefault != nil {
		result.EffectiveDefault = pipelineJSON[*PipelineReference](choices.EffectiveDefault)
	}
	result.EffectiveRollbackDefault = pipelineJSON[*PipelineReference](choices.EffectiveRollbackDefault)
	result.RollbackDefaultSource = pipelineJSON[*PluginPipelineChoicesRollbackDefaultSource](choices.RollbackDefaultSource)
	eligibility := map[uuid.UUID]p.PipelineEligibility{}
	if choices.Eligibility != nil {
		for _, item := range *choices.Eligibility {
			eligibility[item.VersionId] = item
		}
	}
	for _, version := range choices.Versions {
		item := eligibility[version.Id]
		id := version.Id
		pipelineID := version.PipelineId
		result.Versions = append(result.Versions, PluginPipelineChoice{Reference: PipelineReference{VersionId: &id, Digest: version.Digest}, PipelineId: &pipelineID, Version: version.Version, Name: version.Name, Deploy: pipelineJSON[PluginPipelineCompatibility](item.Deploy), Rollback: pipelineJSON[PluginPipelineCompatibility](item.Rollback)})
	}
	return result
}
func (s *Server) readPluginPipelineChoices(ctx context.Context, org string, environment pluginDeploymentEnvironment) (PluginPipelineChoices, error) {
	tx, err := s.Database.BeginTx(ctx, nil)
	if err != nil {
		return PluginPipelineChoices{}, err
	}
	defer func() {
		_ = tx.Rollback()
	}()
	env, err := s.Database.GetEnvironmentByUuid(ctx, tx, org, environment.EnvironmentUUID, model.GetModeDefault)
	if err != nil {
		return PluginPipelineChoices{}, err
	}
	choices, err := s.environmentPipelineChoices(ctx, tx, env)
	if err != nil {
		return PluginPipelineChoices{}, err
	}
	result := pluginChoices(choices)
	if !choices.Policy.Enabled {
		capabilities, protocol, err := s.pipelineRunnerCompatibility(ctx, tx, env)
		if err != nil {
			return result, err
		}
		outcome := recipeCompatibility(p.PipelineRecipe{Strategy: p.Apply, Steps: []p.PipelineRecipeStep{}}, capabilities, protocol)
		builtin := p.BuiltinReference()
		result.Versions = append([]PluginPipelineChoice{{Reference: pipelineJSON[PipelineReference](builtin), Version: 1, Name: "Apply", Deploy: pipelineJSON[PluginPipelineCompatibility](outcome), Rollback: pipelineJSON[PluginPipelineCompatibility](outcome)}}, result.Versions...)
	}
	return result, nil
}
func resolvePluginPipeline(choices PluginPipelineChoices, selection *PipelineReference, rollback bool) (*PipelineReference, error) {
	if selection == nil {
		selection = choices.EffectiveDefault
		if rollback {
			selection = choices.EffectiveRollbackDefault
		}
	}
	if selection == nil {
		return nil, pluginBoundaryError(409, "pipeline_unavailable", "No eligible pipeline default is configured.")
	}
	ref := pipelineJSON[p.PipelineReference](selection)
	if err := p.ValidateReference(ref); err != nil {
		return nil, pluginBoundaryError(400, "pipeline_invalid", err.Error())
	}
	for _, choice := range choices.Versions {
		if choice.Reference.Digest == selection.Digest && equalPipelineReference(choice.Reference, *selection) {
			eligibility := choice.Deploy
			if rollback {
				eligibility = choice.Rollback
			}
			if !eligibility.Compatible {
				return nil, pluginBoundaryError(409, "pipeline_incompatible", "Selected pipeline is incompatible with this operation and Runner.")
			}
			return selection, nil
		}
	}
	return nil, pluginBoundaryError(409, "pipeline_unavailable", "Selected pipeline is not allowed or has been retired.")
}
func equalPipelineReference(a, b PipelineReference) bool {
	return slices.Equal(pipelineReferenceBytes(a), pipelineReferenceBytes(b))
}
func pipelineReferenceBytes(ref PipelineReference) []byte {
	raw, _ := json.Marshal(ref)
	return raw
}
func (s *Server) QueryObservationPipelineChoices(ctx context.Context, r observation.QueryObservationPipelineChoicesRequestObject) (observation.QueryObservationPipelineChoicesResponseObject, error) {
	item, descriptor, err := s.pluginInstallationCaller(ctx, r.OrgId, r.PluginId, false)
	if err != nil {
		return nil, err
	}
	if r.Body == nil {
		return nil, pluginBoundaryError(400, "plugin_request_invalid", "Fresh delegated environment selection is required.")
	}
	action := "observation.pipeline-choices"
	if err = pluginServiceAllowed(item, descriptor, action, false, false); err != nil {
		return nil, err
	}
	claims, err := s.PluginGateway.VerifyServiceReadDelegation(ctx, r.OrgId, r.PluginId, item.ServiceUserID, action, observationProof(r.Body.Proof))
	if err != nil {
		return nil, pluginGatewayError(err)
	}
	actor, err := uuid.Parse(claims.ActorID)
	if err != nil {
		return nil, err
	}
	env, err := s.Database.GetEnvironment(ctx, nil, r.OrgId, r.Body.ProjectId, r.Body.EnvId, model.GetModeDefault)
	if err != nil {
		return nil, pluginModelError(err)
	}
	if err = s.pluginAuthorize(ctx, actor, []orchestratoriam.ResourcePermissionCheck{environmentCheck(env.Uuid, "deployment_write")}); err != nil {
		return nil, err
	}
	choices, err := s.readPluginPipelineChoices(ctx, r.OrgId, pluginDeploymentEnvironment{ProjectID: env.ProjectId, EnvironmentID: env.Id, ProjectUUID: env.ProjectUuid, EnvironmentUUID: env.Uuid})
	return observation.QueryObservationPipelineChoices200JSONResponse(pipelineJSON[observation.PluginPipelineChoices](choices)), err
}
func (s *Server) resolvePluginPipelineBindings(ctx context.Context, org string, environment pluginDeploymentEnvironment, forward, rollback *PipelineReference, forwardOnly, useRollbackDefault bool) (*DeploymentPipelineBindings, error) {
	choices, err := s.readPluginPipelineChoices(ctx, org, environment)
	if err != nil {
		return nil, err
	}
	selected, err := resolvePluginPipeline(choices, forward, false)
	if err != nil {
		return nil, err
	}
	if rollback == nil && !forwardOnly {
		if !useRollbackDefault {
			return nil, pluginBoundaryError(409, "rollback_pipeline_required", "Select an eligible rollback pipeline or explicitly accept a forward-only operation.")
		}
		rollback = choices.EffectiveRollbackDefault
		if rollback == nil {
			return nil, pluginBoundaryError(409, "pipeline_unavailable", "No eligible rollback pipeline default is configured.")
		}
	}
	if rollback != nil {
		rollback, err = resolvePluginPipeline(choices, rollback, true)
		if err != nil {
			return nil, err
		}
	}
	return &DeploymentPipelineBindings{Forward: *selected, Rollback: rollback, ForwardOnly: forwardOnly}, nil
}
func operationPipelineBindings(grant *model.ExternalOperationGrant, env pluginDeploymentEnvironment) (*DeploymentPipelineBindings, error) {
	for _, binding := range grant.Intent.Pipelines {
		if binding.ProjectUUID == env.ProjectUUID && binding.EnvironmentUUID == env.EnvironmentUUID {
			return &DeploymentPipelineBindings{Forward: pipelineJSON[PipelineReference](binding.Forward), Rollback: pipelineJSON[*PipelineReference](binding.Rollback), ForwardOnly: *grant.Intent.ForwardOnly}, nil
		}
	}
	return nil, pluginBoundaryError(409, "pipeline_binding_missing", "Environment has no pipeline binding in this operation.")
}
func (s *Server) checkPluginPipelineForNewDispatch(ctx context.Context, org, plugin string, operation uuid.UUID, env pluginDeploymentEnvironment, body PluginDeploymentSubmitRequest) error {
	previous, err := model.GetExternalDispatch(ctx, s.Database, org, plugin, operation, body.RequestId)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if previous != nil {
		return nil
	}
	if body.Pipeline == nil {
		return s.requireLegacyPipelinePolicyOff(ctx, org, env.ProjectUUID)
	}
	choices, err := s.readPluginPipelineChoices(ctx, org, env)
	if err != nil {
		return err
	}
	_, err = resolvePluginPipeline(choices, body.Pipeline, body.Mode == PluginDeploymentSubmitModeRollback)
	return err
}
func (s *Server) checkObservationPipelineForNewRequest(ctx context.Context, tx model.Tx, org, plugin string, body observation.ObservationRedeployRequest) error {
	if body.Pipeline == nil {
		return nil
	}
	_, err := model.ReadObservationRedeploymentTx(ctx, tx, org, plugin, body.RequestId)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		if _, missing := model.IsErrNotFound(err); !missing {
			return err
		}
	}
	choices, err := s.readPluginPipelineChoices(ctx, org, pluginDeploymentEnvironment{ProjectID: body.ProjectId, EnvironmentID: body.EnvId, ProjectUUID: body.ProjectUuid, EnvironmentUUID: body.EnvironmentUuid})
	if err != nil {
		return err
	}
	reference := pipelineJSON[PipelineReference](body.Pipeline)
	_, err = resolvePluginPipeline(choices, &reference, false)
	return err
}
func (s *Server) requireLegacyPipelinePolicyOff(ctx context.Context, org string, projectID uuid.UUID) error {
	tx, err := s.Database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		_ = tx.Rollback()
	}()
	project, err := s.Database.GetProjectByUuid(ctx, tx, org, projectID, model.GetModeDefault)
	if err != nil {
		return err
	}
	policy, err := model.ReadPipelinePolicy(ctx, tx, project.Uuid)
	if err != nil {
		return err
	}
	if policy.Enabled {
		return pluginBoundaryError(409, "pipeline_reprepare_required", "This project requires a newly prepared pipeline-aware operation.")
	}
	return nil
}
