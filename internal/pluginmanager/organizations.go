package pluginmanager

import (
	"context"
	"fmt"
	"maps"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"go.uber.org/zap"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
)

// An installation serves the organizations of one of two modes:
//
//	selected   the operator enables each organization (PutEnablement)
//	all        Core enables every active organization:
//	             set mode all        -> every organization without an
//	                                    enablement, in the command
//	             organization created -> that organization, in its creation
//	                                    transaction
//
// Core creates enabled enablements with the installation's defaults and never
// touches an existing one: a disabled enablement is the organization's
// opt-out and stays disabled across mode switches and restarts. Switching
// back to selected keeps every enablement as it is; only new organizations are
// no longer enabled. In mode all every active organization has an
// enablement, so removing one is refused. An organization created while the
// plugin host was off, or while the installation-wide grant's organization
// limit was reached, is created without an enablement and listed as pending
// until the operator sets mode all again.

type organizationModePayload struct {
	InstallationID   uuid.UUID         `json:"installation_id"`
	Mode             string            `json:"mode"`
	Timezone         string            `json:"default_timezone,omitempty"`
	ApprovalPolicies map[string]string `json:"default_approval_policies,omitempty"`
}

// SetOrganizationMode sets which organizations the installation serves and
// the defaults of the enablements Core creates. Setting mode all enables
// every active organization without an enablement in the same transaction,
// so it also enables organizations that are pending, or refuses as a whole
// when the installation-wide grant's organization limit does not allow them
// all. Setting the mode and defaults that already hold, with nothing
// pending, changes nothing.
func (m *Manager) SetOrganizationMode(ctx context.Context, command Command, id uuid.UUID, mode, timezone string, approvalPolicies map[string]string) (Installation, error) {
	if approvalPolicies == nil {
		approvalPolicies = map[string]string{}
	}
	var result Installation
	err := m.execute(ctx, command, "plugin_installation.organization_mode", organizationModePayload{InstallationID: id, Mode: mode, Timezone: timezone, ApprovalPolicies: approvalPolicies},
		func(ctx context.Context, tx model.Tx) (*model.PluginManagerChange, error) {
			before, manifest, err := m.lockInstallation(ctx, tx, id)
			if err != nil {
				return nil, err
			}
			if mode != model.PluginOrganizationsSelected && mode != model.PluginOrganizationsAll {
				return nil, invalid("plugin_manager_request_invalid", fmt.Errorf("the organization mode must be selected or all"))
			}
			zone, err := plugins.NormalizeTimezone(timezone)
			if err != nil {
				return nil, invalid("plugin_enablement_invalid", err)
			}
			if err := manifest.ValidateApprovalPolicies(approvalPolicies); err != nil {
				return nil, invalid("plugin_enablement_invalid", err)
			}
			if before.State == model.PluginInstallationPurged {
				return nil, stateConflict("The installation is purged.")
			}
			if mode == model.PluginOrganizationsAll && before.State != model.PluginInstallationActive && before.State != model.PluginInstallationDisabled {
				return nil, stateConflict("Activate the installation before it serves all organizations.")
			}
			after := before
			after.OrganizationMode, after.OrganizationDefaults = mode, model.PluginOrganizationDefaults{Timezone: zone, ApprovalPolicies: approvalPolicies}
			var pending []string
			if mode == model.PluginOrganizationsAll {
				if pending, err = model.PluginOrganizationsWithoutEnablement(ctx, tx, before.PluginID); err != nil {
					return nil, err
				}
			}
			if before.OrganizationMode == mode && before.OrganizationDefaults.Timezone == zone && maps.Equal(before.OrganizationDefaults.ApprovalPolicies, approvalPolicies) && len(pending) == 0 {
				return nil, nil
			}
			// All or nothing: a licence that cannot serve every organization
			// refuses the switch.
			if err := checkOrganizationLimit(ctx, tx, before, len(pending)); err != nil {
				return nil, err
			}
			if err := model.UpdatePluginInstallation(ctx, tx, after, command.Actor); err != nil {
				return nil, err
			}
			for _, org := range pending {
				if err := model.PutPluginEnablement(ctx, tx, after, defaultEnablement(after, org), command.Actor); err != nil {
					return nil, err
				}
			}
			return m.installationChange(ctx, tx, "plugin_installation.organization_mode", before, "")
		}, m.respondInstallation(id), &result)
	if err == nil && mode == model.PluginOrganizationsAll {
		m.reconcileAfterCommand(ctx, id, "")
	}
	return result, err
}

// defaultEnablement is the enablement Core creates for org.
func defaultEnablement(installation model.PluginInstallation, org string) model.PluginEnablement {
	return model.PluginEnablement{OrgID: org, State: model.PluginEnablementEnabled, Timezone: installation.OrganizationDefaults.Timezone,
		ApprovalPolicies: installation.OrganizationDefaults.ApprovalPolicies}
}

// EnableNewOrganization enables org, inside its creation transaction tx, for
// every installation that serves all organizations, with the installation's
// defaults. Each enablement is one Plugin Manager change by Core's internal
// identity. The Plugin Manager lock orders it against a concurrent switch to
// mode all: whichever commits second sees the other. An installation whose
// installation-wide grant allows no further organization skips org and lists
// it as pending; the organization is created all the same. It returns the
// installations it enabled; the caller reconciles their capabilities in org
// once the organization exists for good. tx is the caller's to commit.
func (m *Manager) EnableNewOrganization(ctx context.Context, tx model.Tx, org model.Org) ([]uuid.UUID, error) {
	revision, err := model.LockPluginManager(ctx, tx)
	if err != nil {
		return nil, err
	}
	ids, err := model.PluginInstallationsServingAllOrganizations(ctx, tx)
	if err != nil {
		return nil, err
	}
	enabled := []uuid.UUID{}
	for _, id := range ids {
		before, err := model.GetPluginInstallation(ctx, tx, id, true)
		if err != nil {
			return nil, err
		}
		// The organization is created in any case; an installation whose
		// licence allows no further organization lists it as pending.
		if err := checkOrganizationLimit(ctx, tx, before, 1); err != nil {
			if _, limited := model.IsErrConflict(err); !limited {
				return nil, err
			}
			m.logger.Warn("A new organization is not enabled for a plugin installation that serves all organizations", zap.String("org_id", org.Id),
				zap.String("plugin_id", before.PluginID), zap.Stringer("installation_id", id), zap.Error(err))
			continue
		}
		if err := model.PutPluginEnablement(ctx, tx, before, defaultEnablement(before, org.Id), userid.InternalSystemUuid); err != nil {
			return nil, err
		}
		change, err := m.installationChange(ctx, tx, "plugin_enablement.put", before, org.Id)
		if err != nil {
			return nil, err
		}
		if revision, err = model.RecordPluginManagerSystemChange(ctx, tx, revision, userid.InternalSystemUuid, "organization-created:"+org.Uuid.String(),
			fmt.Sprintf("Organization %s was created while installation %s serves all organizations.", org.Id, id), *change); err != nil {
			return nil, err
		}
		enabled = append(enabled, id)
	}
	return enabled, nil
}

// DiscardOrganization removes, inside tx, every enablement of org, an
// organization whose creation failed after it committed, so that tx can
// delete it: an enablement blocks the deletion of its organization. Each
// removal is one Plugin Manager change by Core's internal identity.
func (m *Manager) DiscardOrganization(ctx context.Context, tx model.Tx, org model.Org) error {
	revision, err := model.LockPluginManager(ctx, tx)
	if err != nil {
		return err
	}
	ids, err := model.PluginInstallationsEnabledFor(ctx, tx, org.Id)
	if err != nil {
		return err
	}
	for _, id := range ids {
		before, err := model.GetPluginInstallation(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if _, err := model.DeletePluginEnablement(ctx, tx, before.PluginID, org.Id); err != nil {
			return err
		}
		change, err := m.installationChange(ctx, tx, "plugin_enablement.remove", before, org.Id)
		if err != nil {
			return err
		}
		if revision, err = model.RecordPluginManagerSystemChange(ctx, tx, revision, userid.InternalSystemUuid, "organization-discarded:"+org.Uuid.String(),
			fmt.Sprintf("Organization %s was removed because its creation failed.", org.Id), *change); err != nil {
			return err
		}
	}
	return nil
}

// ReconcileOrganizationCapabilities registers the served package's
// capabilities of each installation in org, right after Core enabled a new
// organization for them, so that the organization's first administrator can
// grant them at once. A failure is logged and recorded; the periodic
// reconciliation retries it.
func (m *Manager) ReconcileOrganizationCapabilities(ctx context.Context, org string, installations []uuid.UUID) {
	bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), CapabilityReconcileTimeout)
	defer cancel()
	for _, id := range installations {
		if err := m.reconcileCapabilities(bounded, id, org); err != nil {
			m.logger.Error("Plugin capabilities are not registered in IAM yet", zap.Stringer("installation_id", id), zap.String("org_id", org), zap.Error(err))
		}
	}
}

// pendingOrganizations lists the active organizations without an enablement
// of an installation in mode all.
func pendingOrganizations(ctx context.Context, tx model.Tx, installation model.PluginInstallation) ([]string, error) {
	if installation.OrganizationMode != model.PluginOrganizationsAll {
		return nil, nil
	}
	return model.PluginOrganizationsWithoutEnablement(ctx, tx, installation.PluginID)
}
