package pluginmanager

import (
	"encoding/json"
	"fmt"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
)

// registrations materialises what a package contributes, one row per
// capability, operation, service grant, authorization policy and UI.
func registrations(manifest plugins.Manifest) ([]model.PluginRegistration, error) {
	var rows []model.PluginRegistration
	add := func(kind, key string, definition any) error {
		encoded, err := json.Marshal(definition)
		if err != nil {
			return err
		}
		rows = append(rows, model.PluginRegistration{Kind: kind, Key: key, Definition: encoded})
		return nil
	}
	for _, capability := range manifest.Capabilities {
		if err := add(model.PluginRegistrationCapability, capability, capability); err != nil {
			return nil, err
		}
	}
	for _, operation := range manifest.Operations {
		if err := add(model.PluginRegistrationOperation, operation.ID, operation); err != nil {
			return nil, err
		}
	}
	for _, grant := range manifest.ServiceGrants {
		if err := add(model.PluginRegistrationServiceGrant, grant.Action, grant); err != nil {
			return nil, err
		}
	}
	for _, policy := range manifest.AuthorizationPolicies {
		if err := add(model.PluginRegistrationAuthorizationPolicy, policy.Permission, policy); err != nil {
			return nil, err
		}
	}
	if manifest.UI != nil {
		if err := add(model.PluginRegistrationUI, manifest.UI.RoutePrefix, manifest.UI); err != nil {
			return nil, err
		}
	}
	return rows, nil
}

// servedInstallations turns the installations of a snapshot into the
// registry's view. Availability is derived, never stored: an organization is
// available while the installation is active and its enablement enabled, and
// safety-only (not_enabled) while either is disabled.
func servedInstallations(snapshot model.PluginManagerSnapshot) ([]plugins.Installation, []error) {
	items := []plugins.Installation{}
	var rejected []error
	for _, served := range snapshot.Installations {
		item, err := servedInstallation(served)
		if err != nil {
			rejected = append(rejected, fmt.Errorf("plugin %s installation %s: %w", served.Installation.PluginID, served.Installation.InstallationID, err))
			continue
		}
		items = append(items, item)
	}
	return items, rejected
}

func servedInstallation(served model.PluginSnapshotInstallation) (plugins.Installation, error) {
	manifest, err := plugins.DecodeManifest(served.Package.Manifest)
	if err != nil {
		return plugins.Installation{}, err
	}
	item := view(served.Installation, manifest)
	for _, registration := range served.Registrations {
		if err := applyRegistration(&item, registration); err != nil {
			return plugins.Installation{}, fmt.Errorf("registration %s %s: %w", registration.Kind, registration.Key, err)
		}
	}
	if item.UI != nil {
		item.UI.Origin = served.Installation.UIOrigin
	}
	return item, nil
}

// view is the registry view of an installation without its contributions.
// Every enablement keeps its own state, timezone and approval policies.
func view(installation model.PluginInstallation, manifest plugins.Manifest) plugins.Installation {
	item := plugins.Installation{
		InstallationID: installation.InstallationID.String(), ManifestSHA256: installation.PackageSHA256, Entitlement: manifest.Entitlement,
		ID: installation.PluginID, DisplayName: manifest.DisplayName, Version: manifest.Version, Backend: installation.BackendURL,
		ServiceUserID: installation.ServiceUserID, AdmissionCheckerID: installation.AdmissionCheckerID,
		Organizations: map[string]plugins.AvailabilityState{}, OrganizationTimezones: map[string]string{},
		OrganizationApprovalPolicies: map[string]map[string]string{},
	}
	for _, enablement := range installation.Organizations {
		state := plugins.StateNotEnabled
		if installation.State == model.PluginInstallationActive && enablement.State == model.PluginEnablementEnabled {
			state = plugins.StateAvailable
		}
		item.Organizations[enablement.OrgID] = state
		item.OrganizationTimezones[enablement.OrgID] = enablement.Timezone
		if len(enablement.ApprovalPolicies) > 0 {
			item.OrganizationApprovalPolicies[enablement.OrgID] = enablement.ApprovalPolicies
		}
	}
	return item
}

func applyRegistration(item *plugins.Installation, registration model.PluginRegistration) error {
	switch registration.Kind {
	case model.PluginRegistrationCapability:
		var capability string
		if err := json.Unmarshal(registration.Definition, &capability); err != nil {
			return err
		}
		item.Capabilities = append(item.Capabilities, capability)
	case model.PluginRegistrationOperation:
		var operation plugins.Operation
		if err := json.Unmarshal(registration.Definition, &operation); err != nil {
			return err
		}
		item.Operations = append(item.Operations, operation)
	case model.PluginRegistrationServiceGrant:
		var grant plugins.ServiceGrant
		if err := json.Unmarshal(registration.Definition, &grant); err != nil {
			return err
		}
		item.ServiceGrants = append(item.ServiceGrants, grant)
	case model.PluginRegistrationAuthorizationPolicy:
		var policy plugins.AuthorizationPolicy
		if err := json.Unmarshal(registration.Definition, &policy); err != nil {
			return err
		}
		item.AuthorizationPolicies = append(item.AuthorizationPolicies, policy)
	case model.PluginRegistrationUI:
		var ui plugins.UIContribution
		if err := json.Unmarshal(registration.Definition, &ui); err != nil {
			return err
		}
		item.UI = &ui
	default:
		return fmt.Errorf("unknown registration kind")
	}
	return nil
}
