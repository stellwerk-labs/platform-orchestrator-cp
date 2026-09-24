package model

import (
	"context"

	"github.com/pkg/errors"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/moduleconformance"
)

func conformanceBadRequest(err error) error {
	var failure *moduleconformance.Error
	if errors.As(err, &failure) {
		return ErrBadRequest{Code: failure.Code, Message: failure.Error(),
			Details: map[string]any{"path": failure.Path, "rule": failure.Rule}}
	}
	return err
}

func (d *databaser) validateModuleConformance(ctx context.Context, tx Tx, definition *ModuleDefinitionVersion) error {
	resourceType, err := d.GetResourceType(ctx, tx, &definition.OrgId, definition.ResourceType)
	if err != nil {
		return err
	}
	if definition.SemanticVersion != "" {
		if err := moduleconformance.ValidateOutputs(resourceType.OutputsSchema, definition.OutputSchema); err != nil {
			return conformanceBadRequest(err)
		}
	}
	if resourceType.ModuleContract == nil {
		return nil
	}
	inputs := definition.ModuleInputs
	if inputs == nil {
		inputs = map[string]any{}
	}
	params := definition.ModuleParams
	if params == nil {
		params = map[string]ModuleParam{}
	}
	providers := definition.ProviderMapping
	if providers == nil {
		providers = map[string]string{}
	}
	dependencies := definition.Dependencies
	if dependencies == nil {
		dependencies = map[string]ModuleDefinitionDependency{}
	}
	coprovisioned := definition.CoProvisioned
	if coprovisioned == nil {
		coprovisioned = []ModuleDefinitionCoProvision{}
	}
	document := map[string]any{
		"module_inputs": inputs, "module_params": params, "provider_mapping": providers,
		"dependencies": dependencies, "coprovisioned": coprovisioned,
	}
	if definition.OutputSchema != nil {
		document["output_schema"] = definition.OutputSchema
	}
	return conformanceBadRequest(moduleconformance.ValidateDefinition(ctx, resourceType.ModuleContract, document))
}
