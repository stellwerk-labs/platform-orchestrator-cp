package pluginmanager

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/pluginentitlements"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
)

// Command carries what every operator write needs: who, an idempotency key,
// the revision the operator decided on and why.
type Command struct {
	Actor            uuid.UUID
	IdempotencyKey   string
	ExpectedRevision int64
	Reason           string
	RequestID        string
}

// Package is a package as of a Plugin Manager revision.
type Package struct {
	model.PluginPackage
	Revision int64 `json:"revision"`
}

// Installation is an installation as of a Plugin Manager revision, with what
// Core derives from it.
type Installation struct {
	model.PluginInstallation
	// Availability is derived from the state: available while active,
	// not_enabled (safety-only) while disabled, unavailable while installed
	// and not yet proven by the running plugin.
	Availability plugins.AvailabilityState `json:"availability"`
	// Diagnostic explains why the plugin's last status does not prove the
	// package Core is to serve.
	Diagnostic *Diagnostic `json:"diagnostic,omitempty"`
	Revision   int64       `json:"revision"`
}

// BundleImport reports an entitlement bundle import.
type BundleImport struct {
	pluginentitlements.BundleImport
	Revision int64 `json:"revision"`
}

// Action is an installation-wide lifecycle step.
type Action string

const (
	// ActionActivate serves the package registrations of an installed
	// installation once its running plugin proves the recorded package.
	ActionActivate Action = "activate"
	// ActionDisable keeps an active installation safety-only.
	ActionDisable Action = "disable"
	// ActionEnable returns a disabled installation to active.
	ActionEnable Action = "enable"
)

var manifestDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Command payloads identify a request in its idempotency fingerprint.
type installationPayload struct {
	InstallationID uuid.UUID `json:"installation_id"`
}

type identityPayload struct {
	InstallationID     uuid.UUID `json:"installation_id"`
	ServiceUserID      string    `json:"service_user_id"`
	AdmissionCheckerID string    `json:"admission_checker_id"`
}

type enablementPayload struct {
	InstallationID   uuid.UUID         `json:"installation_id"`
	OrgID            string            `json:"org_id"`
	State            string            `json:"state,omitempty"`
	Timezone         string            `json:"timezone,omitempty"`
	ApprovalPolicies map[string]string `json:"approval_policies,omitempty"`
}

func invalid(code string, err error) error {
	return model.ErrBadRequest{Code: code, Message: err.Error()}
}

func stateConflict(message string) error {
	return model.ErrConflict{Code: "plugin_installation_state", Message: message}
}

// execute runs a command and decodes the stored response into result, so a
// first response and its replay take the same path.
func (m *Manager) execute(ctx context.Context, command Command, name string, payload any, mutate func(context.Context, model.Tx) (*model.PluginManagerChange, error), respond func(context.Context, model.Tx, int64) (any, error), result any) error {
	encoded, err := json.Marshal(struct {
		Payload          any    `json:"payload"`
		ExpectedRevision int64  `json:"expected_revision"`
		Reason           string `json:"reason"`
	}{payload, command.ExpectedRevision, command.Reason})
	if err != nil {
		return err
	}
	response, err := model.ExecutePluginManagerCommand(ctx, m.db, model.PluginManagerCommand{
		Actor: command.Actor, IdempotencyKey: command.IdempotencyKey, Fingerprint: fmt.Sprintf("%s:sha256:%x", name, sha256.Sum256(encoded)),
		ExpectedRevision: command.ExpectedRevision, Reason: command.Reason, RequestID: command.RequestID,
	}, mutate, respond)
	if err != nil {
		return err
	}
	// This replica serves the change at once; the others within one interval.
	if err := m.Refresh(ctx); err != nil {
		m.logger.Error("Plugin Manager snapshot refresh after a command failed", zap.Error(err))
	}
	return json.Unmarshal(response, result)
}

func (m *Manager) respondInstallation(id uuid.UUID) func(context.Context, model.Tx, int64) (any, error) {
	return func(ctx context.Context, tx model.Tx, revision int64) (any, error) {
		installation, err := model.GetPluginInstallation(ctx, tx, id, false)
		if err != nil {
			return nil, err
		}
		return describe(ctx, tx, installation, revision)
	}
}

// describe derives availability and the status diagnostic of an
// installation.
func describe(ctx context.Context, tx model.Tx, installation model.PluginInstallation, revision int64) (Installation, error) {
	result := Installation{PluginInstallation: installation, Availability: plugins.StateUnavailable, Revision: revision}
	switch installation.State {
	case model.PluginInstallationActive:
		result.Availability = plugins.StateAvailable
	case model.PluginInstallationDisabled:
		result.Availability = plugins.StateNotEnabled
	}
	if installation.Observation == nil {
		return result, nil
	}
	pkg, err := model.GetPluginPackage(ctx, tx, installation.PackageSHA256)
	if err != nil {
		return result, err
	}
	manifest, err := plugins.DecodeManifest(pkg.Manifest)
	if err != nil {
		return result, err
	}
	result.Diagnostic = diagnose(installation.Observation, installation.InstallationID, pkg.ManifestSHA256, manifest.MigrationGeneration)
	return result, nil
}

// PushPackage registers an immutable package. Pushing the same digest and
// manifest again changes nothing.
func (m *Manager) PushPackage(ctx context.Context, command Command, digest string, raw json.RawMessage) (Package, error) {
	manifest, err := plugins.DecodeManifest(raw)
	if err != nil {
		return Package{}, invalid("plugin_manifest_invalid", err)
	}
	if err := manifest.Validate(); err != nil {
		return Package{}, invalid("plugin_manifest_invalid", err)
	}
	if !manifestDigest.MatchString(digest) {
		return Package{}, invalid("plugin_manifest_invalid", fmt.Errorf("the manifest digest must be a lowercase SHA-256 hex string"))
	}
	canonical, err := json.Marshal(manifest)
	if err != nil {
		return Package{}, err
	}
	var result Package
	err = m.execute(ctx, command, "plugin_package.push", map[string]any{"manifest_sha256": digest, "manifest": manifest},
		func(ctx context.Context, tx model.Tx) (*model.PluginManagerChange, error) {
			existing, err := model.GetPluginPackage(ctx, tx, digest)
			if err == nil {
				stored, err := plugins.DecodeManifest(existing.Manifest)
				if err != nil {
					return nil, err
				}
				if same, err := equalJSON(stored, manifest); err != nil || !same {
					return nil, conflictOr(err, model.ErrConflict{Code: "plugin_package_conflict", Message: "The manifest digest is already registered with a different manifest."})
				}
				return nil, nil
			}
			if _, missing := model.IsErrNotFound(err); !missing {
				return nil, err
			}
			pkg := model.PluginPackage{ManifestSHA256: digest, PluginID: manifest.ID, Version: manifest.Version, Manifest: canonical, CreatedBy: command.Actor}
			if err := model.InsertPluginPackage(ctx, tx, pkg); err != nil {
				return nil, err
			}
			created, err := model.GetPluginPackage(ctx, tx, digest)
			return &model.PluginManagerChange{Action: "plugin_package.push", After: created}, err
		},
		func(ctx context.Context, tx model.Tx, revision int64) (any, error) {
			pkg, err := model.GetPluginPackage(ctx, tx, digest)
			return Package{PluginPackage: pkg, Revision: revision}, err
		}, &result)
	return result, err
}

func equalJSON(a, b any) (bool, error) {
	left, err := json.Marshal(a)
	if err != nil {
		return false, err
	}
	right, err := json.Marshal(b)
	return string(left) == string(right), err
}

func conflictOr(err error, conflict error) error {
	if err != nil {
		return err
	}
	return conflict
}

// CreateInstallation installs a package. Core assigns the immutable
// installation UUID; a replay of the command returns the same one.
func (m *Manager) CreateInstallation(ctx context.Context, command Command, packageDigest, backend, uiOrigin string) (Installation, error) {
	id := uuid.New()
	var result Installation
	err := m.execute(ctx, command, "plugin_installation.create", map[string]string{"package_sha256": packageDigest, "backend_url": backend, "ui_origin": uiOrigin},
		func(ctx context.Context, tx model.Tx) (*model.PluginManagerChange, error) {
			pkg, err := model.GetPluginPackage(ctx, tx, packageDigest)
			if err != nil {
				return nil, err
			}
			manifest, err := plugins.DecodeManifest(pkg.Manifest)
			if err != nil {
				return nil, err
			}
			if err := plugins.ValidateEndpoints(manifest, backend, uiOrigin, m.allowDevelopmentHTTP); err != nil {
				return nil, invalid("plugin_installation_invalid", err)
			}
			installation := model.PluginInstallation{InstallationID: id, PluginID: pkg.PluginID, PackageSHA256: pkg.ManifestSHA256, State: model.PluginInstallationInstalled,
				BackendURL: backend, UIOrigin: uiOrigin, CreatedBy: command.Actor}
			if err := model.InsertPluginInstallation(ctx, tx, installation); err != nil {
				return nil, err
			}
			created, err := model.GetPluginInstallation(ctx, tx, id, false)
			return &model.PluginManagerChange{Action: "plugin_installation.create", InstallationID: id, After: created}, err
		}, m.respondInstallation(id), &result)
	return result, err
}

// SetIdentity records the plugin service user and admission checker before
// activation. Recording the same identities again changes nothing.
func (m *Manager) SetIdentity(ctx context.Context, command Command, id uuid.UUID, serviceUser, admissionChecker string) (Installation, error) {
	var result Installation
	err := m.execute(ctx, command, "plugin_installation.identity", identityPayload{InstallationID: id, ServiceUserID: serviceUser, AdmissionCheckerID: admissionChecker},
		func(ctx context.Context, tx model.Tx) (*model.PluginManagerChange, error) {
			before, manifest, err := m.lockInstallation(ctx, tx, id)
			if err != nil {
				return nil, err
			}
			if err := plugins.ValidateServiceIdentities(manifest, serviceUser, admissionChecker); err != nil {
				return nil, invalid("plugin_installation_invalid", err)
			}
			if before.ServiceUserID == serviceUser && before.AdmissionCheckerID == admissionChecker {
				return nil, nil
			}
			if before.State != model.PluginInstallationInstalled {
				return nil, stateConflict("Service identities are recorded before activation.")
			}
			after := before
			after.ServiceUserID, after.AdmissionCheckerID = serviceUser, admissionChecker
			if err := model.UpdatePluginInstallation(ctx, tx, after, command.Actor); err != nil {
				return nil, err
			}
			return m.installationChange(ctx, tx, "plugin_installation.identity", before, "")
		}, m.respondInstallation(id), &result)
	return result, err
}

// Transition applies an installation-wide lifecycle step. A step whose
// target state already holds changes nothing.
func (m *Manager) Transition(ctx context.Context, command Command, id uuid.UUID, action Action) (Installation, error) {
	if action == ActionActivate {
		return m.activate(ctx, command, id)
	}
	var result Installation
	err := m.execute(ctx, command, "plugin_installation."+string(action), installationPayload{InstallationID: id},
		func(ctx context.Context, tx model.Tx) (*model.PluginManagerChange, error) {
			before, _, err := m.lockInstallation(ctx, tx, id)
			if err != nil {
				return nil, err
			}
			after := before
			switch action {
			case ActionDisable, ActionEnable:
				target := map[Action]string{ActionDisable: model.PluginInstallationDisabled, ActionEnable: model.PluginInstallationActive}[action]
				if before.State == target {
					return nil, nil
				}
				if before.State != model.PluginInstallationActive && before.State != model.PluginInstallationDisabled {
					return nil, stateConflict("Activate the installation first.")
				}
				after.State = target
			default:
				return nil, invalid("plugin_manager_request_invalid", fmt.Errorf("unknown installation action %q", action))
			}
			if err := model.UpdatePluginInstallation(ctx, tx, after, command.Actor); err != nil {
				return nil, err
			}
			return m.installationChange(ctx, tx, "plugin_installation."+string(action), before, "")
		}, m.respondInstallation(id), &result)
	return result, err
}

// activate serves the package registrations of an installed installation.
// Core first reads the running plugin's status, outside the serialised
// command, and records what it observed. The command then refuses with the
// diagnostic unless the plugin reports exactly the installation UUID, package
// digest and migration generation Core recorded; a refusal changes nothing,
// so the operator activates again once the plugin runtime caught up.
func (m *Manager) activate(ctx context.Context, command Command, id uuid.UUID) (Installation, error) {
	current, err := m.Installation(ctx, id)
	if err != nil {
		return Installation{}, err
	}
	var observed *model.PluginStatusObservation
	if current.State == model.PluginInstallationInstalled {
		observation := m.readStatus(ctx, current.BackendURL)
		if err := model.RecordPluginStatusObservation(ctx, m.db, id, observation); err != nil {
			return Installation{}, err
		}
		observed = &observation
	}
	var result Installation
	err = m.execute(ctx, command, "plugin_installation."+string(ActionActivate), installationPayload{InstallationID: id},
		func(ctx context.Context, tx model.Tx) (*model.PluginManagerChange, error) {
			before, manifest, err := m.lockInstallation(ctx, tx, id)
			if err != nil {
				return nil, err
			}
			if before.State == model.PluginInstallationActive {
				return nil, nil
			}
			if before.State != model.PluginInstallationInstalled {
				return nil, stateConflict("Only an installed installation is activated; a disabled one is enabled.")
			}
			after := before
			after.State = model.PluginInstallationActive
			if err := m.validateServed(after, manifest); err != nil {
				return nil, err
			}
			if observed == nil {
				return nil, stateConflict("The installation changed while Core read the plugin status. Read it and activate again.")
			}
			if diagnostic := diagnose(observed, id, after.PackageSHA256, manifest.MigrationGeneration); diagnostic != nil {
				return nil, model.ErrConflict{Code: diagnostic.Code, Message: diagnostic.Message}
			}
			rows, err := registrations(manifest)
			if err != nil {
				return nil, err
			}
			if err := model.ReplacePluginRegistrations(ctx, tx, after, rows); err != nil {
				return nil, err
			}
			if err := model.UpdatePluginInstallation(ctx, tx, after, command.Actor); err != nil {
				return nil, err
			}
			return m.installationChange(ctx, tx, "plugin_installation."+string(ActionActivate), before, "")
		}, m.respondInstallation(id), &result)
	return result, err
}

// validateServed checks that Core can serve the complete installation with
// the package manifest.
func (m *Manager) validateServed(installation model.PluginInstallation, manifest plugins.Manifest) error {
	item, err := view(installation, manifest)
	if err != nil {
		return err
	}
	item.Capabilities, item.Operations, item.ServiceGrants, item.AuthorizationPolicies = manifest.Capabilities, manifest.Operations, manifest.ServiceGrants, manifest.AuthorizationPolicies
	if manifest.UI != nil {
		ui := *manifest.UI
		ui.Origin = installation.UIOrigin
		item.UI = &ui
	}
	if err := plugins.ValidateInstallation(item, m.allowDevelopmentHTTP); err != nil {
		return model.ErrConflict{Code: "plugin_installation_incomplete", Message: "The installation cannot be activated: " + err.Error()}
	}
	return nil
}

// PutEnablement enables or disables an active installation for one
// organization. In v1alpha an installation serves exactly one organization.
func (m *Manager) PutEnablement(ctx context.Context, command Command, id uuid.UUID, org, state, timezone string, approvalPolicies map[string]string) (Installation, error) {
	if approvalPolicies == nil {
		approvalPolicies = map[string]string{}
	}
	var result Installation
	err := m.execute(ctx, command, "plugin_enablement.put", enablementPayload{InstallationID: id, OrgID: org, State: state, Timezone: timezone, ApprovalPolicies: approvalPolicies},
		func(ctx context.Context, tx model.Tx) (*model.PluginManagerChange, error) {
			before, manifest, err := m.lockInstallation(ctx, tx, id)
			if err != nil {
				return nil, err
			}
			if state != model.PluginEnablementEnabled && state != model.PluginEnablementDisabled {
				return nil, invalid("plugin_enablement_invalid", fmt.Errorf("the enablement state must be enabled or disabled"))
			}
			zone, err := plugins.NormalizeTimezone(timezone)
			if err != nil {
				return nil, invalid("plugin_enablement_invalid", err)
			}
			if err := manifest.ValidateApprovalPolicies(approvalPolicies); err != nil {
				return nil, invalid("plugin_enablement_invalid", err)
			}
			if before.State != model.PluginInstallationActive && before.State != model.PluginInstallationDisabled {
				return nil, stateConflict("Activate the installation before enabling an organization.")
			}
			if _, err := m.db.GetOrg(ctx, tx, org); err != nil {
				if _, missing := model.IsErrNotFound(err); missing {
					return nil, model.ErrNotFound{Code: "organization_not_found", Message: "The organization does not exist."}
				}
				return nil, err
			}
			for _, existing := range before.Organizations {
				if existing.OrgID == org && existing.State == state && existing.Timezone == zone && maps.Equal(existing.ApprovalPolicies, approvalPolicies) {
					return nil, nil
				}
			}
			if err := model.PutPluginEnablement(ctx, tx, before, model.PluginEnablement{OrgID: org, State: state, Timezone: zone, ApprovalPolicies: approvalPolicies}, command.Actor); err != nil {
				return nil, err
			}
			return m.installationChange(ctx, tx, "plugin_enablement.put", before, org)
		}, m.respondInstallation(id), &result)
	return result, err
}

// RemoveEnablement removes an organization's enablement. Its resource event
// journal pauses in the same transaction and later resumes with a gap.
// Removing an absent enablement changes nothing.
func (m *Manager) RemoveEnablement(ctx context.Context, command Command, id uuid.UUID, org string) (Installation, error) {
	var result Installation
	err := m.execute(ctx, command, "plugin_enablement.remove", enablementPayload{InstallationID: id, OrgID: org},
		func(ctx context.Context, tx model.Tx) (*model.PluginManagerChange, error) {
			before, _, err := m.lockInstallation(ctx, tx, id)
			if err != nil {
				return nil, err
			}
			deleted, err := model.DeletePluginEnablement(ctx, tx, before.PluginID, org)
			if err != nil || !deleted {
				return nil, err
			}
			return m.installationChange(ctx, tx, "plugin_enablement.remove", before, org)
		}, m.respondInstallation(id), &result)
	return result, err
}

// ImportEntitlementBundle imports signed grants and revocations. Grants match
// only bindings of installations enabled for an organization.
func (m *Manager) ImportEntitlementBundle(ctx context.Context, command Command, bundle []byte) (BundleImport, error) {
	var report pluginentitlements.BundleImport
	var result BundleImport
	err := m.execute(ctx, command, "plugin_entitlement_bundle.import", map[string]string{"bundle_sha256": fmt.Sprintf("%x", sha256.Sum256(bundle))},
		func(ctx context.Context, tx model.Tx) (*model.PluginManagerChange, error) {
			var err error
			report, err = m.entitlements.ImportBundle(ctx, tx, bundle, command.Actor.String())
			if err != nil || report.ImportedGrants+report.ImportedRevocations == 0 {
				return nil, err
			}
			return &model.PluginManagerChange{Action: "plugin_entitlement_bundle.import", After: map[string]any{
				"bundle_sha256": fmt.Sprintf("%x", report.Digest), "imported_grants": report.ImportedGrants, "imported_revocations": report.ImportedRevocations,
			}}, nil
		},
		func(_ context.Context, _ model.Tx, revision int64) (any, error) {
			return BundleImport{BundleImport: report, Revision: revision}, nil
		}, &result)
	return result, err
}

func (m *Manager) lockInstallation(ctx context.Context, tx model.Tx, id uuid.UUID) (model.PluginInstallation, plugins.Manifest, error) {
	installation, err := model.GetPluginInstallation(ctx, tx, id, true)
	if err != nil {
		return installation, plugins.Manifest{}, err
	}
	pkg, err := model.GetPluginPackage(ctx, tx, installation.PackageSHA256)
	if err != nil {
		return installation, plugins.Manifest{}, err
	}
	manifest, err := plugins.DecodeManifest(pkg.Manifest)
	return installation, manifest, err
}

func (m *Manager) installationChange(ctx context.Context, tx model.Tx, action string, before model.PluginInstallation, org string) (*model.PluginManagerChange, error) {
	after, err := model.GetPluginInstallation(ctx, tx, before.InstallationID, false)
	if err != nil {
		return nil, err
	}
	return &model.PluginManagerChange{Action: action, InstallationID: before.InstallationID, OrgID: org, Before: before, After: after}, nil
}
