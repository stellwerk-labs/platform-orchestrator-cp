package api

import (
	"github.com/google/uuid"

	orchestratoriam "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"
)

// Permission identifiers are kept local so the control plane can be built and
// released independently from the IAM shared module that evaluates them.
const (
	PermissionOrganizationRead          = "organization_read"
	PermissionProjectRead               = "project_read"
	PermissionProjectWrite              = "project_write"
	PermissionEnvironmentRead           = "environment_read"
	PermissionEnvironmentWrite          = "environment_write"
	PermissionEnvironmentTypeRead       = "environment_type_read"
	PermissionEnvironmentTypeWrite      = "environment_type_write"
	PermissionModuleRead                = "module_read"
	PermissionModuleWrite               = "module_write"
	PermissionModuleProviderRead        = "module_provider_read"
	PermissionModuleProviderWrite       = "module_provider_write"
	PermissionModuleRuleRead            = "module_rule_read"
	PermissionModuleRuleWrite           = "module_rule_write"
	PermissionModuleCoreRead            = "module.read"
	PermissionModuleArchive             = "module.archive"
	PermissionModuleVersionRead         = "module.version.read"
	PermissionModuleVersionPublish      = "module.version.publish"
	PermissionModuleVersionPromote      = "module.version.promote"
	PermissionModuleVersionDeprecate    = "module.version.deprecate"
	PermissionModuleVersionDefective    = "module.version.mark-defective"
	PermissionModuleVersionRestore      = "module.version.restore"
	PermissionModuleVersionUseProposed  = "module.version.use-proposed"
	PermissionModuleVersionPin          = "module.version.pin"
	PermissionModuleVersionPinNote      = "module.version.pin-note"
	PermissionModuleVersionUnpin        = "module.version.unpin"
	PermissionModuleVersionPinOverride  = "module.version.pin-override"
	PermissionModuleVersionPinRestore   = "module.version.pin-restore"
	PermissionModuleVersionPinDefective = "module.version.pin-defective"
	PermissionModuleRollbackRestricted  = "module.version.rollback-restricted"
	PermissionResourceTypeRead          = "resource_type_read"
	PermissionResourceTypeWrite         = "resource_type_write"
	PermissionRunnerRead                = "runner_read"
	PermissionRunnerWrite               = "runner_write"
	PermissionRunnerRuleRead            = "runner_rule_read"
	PermissionRunnerRuleWrite           = "runner_rule_write"
)

func orgCheck(orgID, permission string) orchestratoriam.ResourcePermissionCheck {
	return orchestratoriam.ResourcePermissionCheck{
		Permission: permission,
		Resource:   "organization:" + orgID,
	}
}

func projectCheck(projectID uuid.UUID, permission string) orchestratoriam.ResourcePermissionCheck {
	return orchestratoriam.ResourcePermissionCheck{
		Permission: permission,
		Resource:   "project:" + projectID.String(),
	}
}

func environmentCheck(environmentID uuid.UUID, permission string) orchestratoriam.ResourcePermissionCheck {
	return orchestratoriam.ResourcePermissionCheck{
		Permission: permission,
		Resource:   "env:" + environmentID.String(),
	}
}
