package model

import (
	"slices"

	"github.com/google/uuid"
)

// ValidateExternalOperationRead permits inspection after suspension or release,
// but never turns a retained grant into execution authority. Callers must also
// check current installation and IAM permissions for the grant's current actor.
func ValidateExternalOperationRead(grant *ExternalOperationGrant, fence int64, action string, environments []ExternalOperationEnvironment, versions map[uuid.UUID]uuid.UUID) error {
	if grant == nil || fence <= 0 || grant.Revision != fence || grant.ActorID == uuid.Nil {
		return NewErrConflict("inspection requires the current operation grant")
	}
	if action != "external-deployment.readiness" && action != "external-deployment.plan" && action != "external-deployment.read" {
		return NewErrBadRequest("unsupported operation inspection action")
	}
	if !slices.Contains(grant.Intent.Actions, action) {
		return NewErrConflict("inspection action is outside the frozen grant")
	}
	if len(environments) == 0 {
		return NewErrBadRequest("exact inspection scope is required")
	}
	for _, environment := range environments {
		if !slices.Contains(grant.Intent.Environments, environment) {
			return NewErrConflict("inspection environment is outside the frozen grant")
		}
	}
	for module, version := range versions {
		if version == uuid.Nil || grant.Intent.TargetVersions[module.String()] != version {
			return NewErrConflict("inspection target is outside the frozen grant")
		}
	}
	return nil
}
