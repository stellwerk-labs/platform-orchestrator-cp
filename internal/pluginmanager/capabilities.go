package pluginmanager

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	orchestratoriam "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"
	"go.uber.org/zap"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
)

// CapabilityReconcileTimeout bounds one reconciliation after a command.
const CapabilityReconcileTimeout = 30 * time.Second

// maxFailure bounds the reason a reconciliation records.
const maxFailure = 500

// IAM keeps a catalogue of the capabilities people can receive through the
// roles of an organization, per organization and plugin, and denies every
// check of a plugin capability it does not hold for the organization at that
// scope. Core registers the capability declarations of the package an
// installation serves for every organization the installation is enabled for:
//
//	enable an organization   -> reconcile that organization
//	activate (also upgrade)  -> reconcile every enabled organization
//	start-up, every interval -> reconcile everything (drift)
//
// A reconciliation reads IAM's catalogue first and replaces it only when it
// differs, so it is idempotent and repeating it costs IAM one read. Disabling
// an organization keeps its catalogue: the plugin's safety work there still
// needs IAM to check the original actor's capabilities. Removing an
// enablement or uninstalling keeps it too; without an enablement Core refuses
// every effect of the plugin in that organization, and the roles that name
// the capabilities stay readable.

// ReconcileCapabilities reconciles the capabilities of every installation
// that is enabled for an organization. It reports every failure; one never
// stops the others.
func (m *Manager) ReconcileCapabilities(ctx context.Context) error {
	var installations []model.PluginInstallation
	if err := readOnly(ctx, m.db, func(tx model.Tx) (err error) {
		installations, err = model.ListPluginInstallations(ctx, tx)
		return err
	}); err != nil {
		return err
	}
	var failures []error
	for _, installation := range installations {
		if len(installation.Organizations) == 0 {
			continue
		}
		if err := m.reconcileCapabilities(ctx, installation.InstallationID, ""); err != nil {
			failures = append(failures, fmt.Errorf("plugin %s installation %s: %w", installation.PluginID, installation.InstallationID, err))
		}
	}
	return errors.Join(failures...)
}

// reconcileAfterCommand reconciles right after a command committed, for org
// or, when org is empty, every enabled organization. The command stands
// whatever IAM answers; a failure is recorded and retried by the next
// reconciliation.
func (m *Manager) reconcileAfterCommand(ctx context.Context, id uuid.UUID, org string) {
	bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), CapabilityReconcileTimeout)
	defer cancel()
	if err := m.reconcileCapabilities(bounded, id, org); err != nil {
		m.logger.Error("Plugin capabilities are not registered in IAM yet", zap.Stringer("installation_id", id), zap.String("org_id", org), zap.Error(err))
	}
}

// reconcileCapabilities brings IAM's catalogue of the installation's plugin
// in line with the served package, for org or every enabled organization, and
// records the outcome per organization.
func (m *Manager) reconcileCapabilities(ctx context.Context, id uuid.UUID, org string) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	installation, err := model.GetPluginInstallation(ctx, tx, id, false)
	if err != nil {
		return err
	}
	if err := model.LockPluginCapabilities(ctx, tx, installation.PluginID); err != nil {
		return err
	}
	// Read again under the lock: a reconciliation that waited sees every
	// change committed before the one it waited for finished.
	if installation, err = model.GetPluginInstallation(ctx, tx, id, false); err != nil {
		return err
	}
	manifest, err := m.packageManifest(ctx, tx, installation.PackageSHA256)
	if err != nil {
		return err
	}
	var failures []error
	for _, enablement := range installation.Organizations {
		if org != "" && enablement.OrgID != org {
			continue
		}
		reconciliation := model.PluginCapabilityReconciliation{PluginID: installation.PluginID, OrgID: enablement.OrgID, PackageSHA256: installation.PackageSHA256}
		if err := m.registerCapabilities(ctx, enablement.OrgID, installation.PluginID, manifest.Capabilities); err != nil {
			failures = append(failures, fmt.Errorf("organization %s: %w", enablement.OrgID, err))
			reconciliation.Failure = truncate(err.Error(), maxFailure)
		}
		reconciliation.ReconciledAt = time.Now().UTC()
		if err := model.RecordPluginCapabilityReconciliation(ctx, tx, reconciliation); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return errors.Join(failures...)
}

// registerCapabilities replaces IAM's catalogue of plugin in org with
// declared unless IAM already holds exactly that.
func (m *Manager) registerCapabilities(ctx context.Context, org, pluginID string, declared []plugins.Capability) error {
	held, err := m.iam.ListPermissionsWithResponse(ctx, org)
	if err != nil {
		return fmt.Errorf("IAM cannot be reached to read the organization's permissions: %w", err)
	}
	if held.JSON200 == nil {
		return fmt.Errorf("IAM answered %s when Core read the organization's permissions", held.Status())
	}
	prefix := "plugin." + pluginID + "."
	registered := map[string]orchestratoriam.PluginCapabilityRegistration{}
	for _, permission := range held.JSON200.Items {
		name, ours := strings.CutPrefix(permission.Id, prefix)
		if !ours || strings.Contains(name, ".") {
			continue
		}
		scopes := make([]orchestratoriam.PluginCapabilityRegistrationScopes, 0, len(permission.Scopes))
		for _, scope := range permission.Scopes {
			scopes = append(scopes, orchestratoriam.PluginCapabilityRegistrationScopes(scope))
		}
		registered[name] = registration(name, permission.DisplayName, permission.Description, permission.Category, string(permission.Level), scopes)
	}
	body := orchestratoriam.PluginCapabilityRegistrationBody{Capabilities: make([]orchestratoriam.PluginCapabilityRegistration, 0, len(declared))}
	wanted := map[string]orchestratoriam.PluginCapabilityRegistration{}
	for _, capability := range declared {
		name := strings.TrimPrefix(capability.ID, prefix)
		scopes := make([]orchestratoriam.PluginCapabilityRegistrationScopes, 0, len(capability.Scopes))
		for _, scope := range capability.Scopes {
			scopes = append(scopes, orchestratoriam.PluginCapabilityRegistrationScopes(scope))
		}
		wanted[name] = registration(name, capability.DisplayName, capability.Description, capability.Category, capability.Level, scopes)
		body.Capabilities = append(body.Capabilities, wanted[name])
	}
	if maps.EqualFunc(registered, wanted, sameRegistration) {
		return nil
	}
	replaced, err := m.iam.ReplacePluginCapabilitiesWithResponse(ctx, org, pluginID, body)
	if err != nil {
		return fmt.Errorf("IAM cannot be reached to register the capabilities: %w", err)
	}
	if replaced.StatusCode() != 204 {
		return fmt.Errorf("IAM answered %s when Core registered the capabilities: %s", replaced.Status(), strings.TrimSpace(string(replaced.Body)))
	}
	return nil
}

func registration(name, displayName, description, category, level string, scopes []orchestratoriam.PluginCapabilityRegistrationScopes) orchestratoriam.PluginCapabilityRegistration {
	return orchestratoriam.PluginCapabilityRegistration{Name: name, DisplayName: displayName, Description: description, Category: category,
		Level: orchestratoriam.PluginCapabilityRegistrationLevel(level), Scopes: scopes}
}

// sameRegistration compares two registrations; scopes are a set.
func sameRegistration(a, b orchestratoriam.PluginCapabilityRegistration) bool {
	sorted := func(scopes []orchestratoriam.PluginCapabilityRegistrationScopes) []orchestratoriam.PluginCapabilityRegistrationScopes {
		return slices.Sorted(slices.Values(scopes))
	}
	return a.Name == b.Name && a.DisplayName == b.DisplayName && a.Description == b.Description && a.Category == b.Category && a.Level == b.Level &&
		slices.Equal(sorted(a.Scopes), sorted(b.Scopes))
}

// truncate cuts text to at most limit bytes on a rune boundary.
func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}
