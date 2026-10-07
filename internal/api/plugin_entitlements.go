package api

import (
	"context"
	"database/sql"
	"errors"

	"github.com/stellwerk-labs/golib/herrors"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
)

// PluginEntitlementGuard revalidates licence authority in the transaction that
// commits the Core effect. Discovery and Gateway checks are only preflights.
type PluginEntitlementGuard interface {
	Guard(context.Context, model.Databaser, model.Tx, string, plugins.Installation, string, bool, bool) error
}

// GetPluginEntitlement is a service-only preflight, including before prepare.
// It exposes no issuer evidence and cannot create an operation or grant IAM.
func (s *Server) GetPluginEntitlement(ctx context.Context, request GetPluginEntitlementRequestObject) (GetPluginEntitlementResponseObject, error) {
	_, descriptor, err := s.pluginConfiguredCaller(ctx, request.OrgId, request.PluginId, false)
	if err != nil {
		return nil, err
	}
	return GetPluginEntitlement200JSONResponse{
		Id: descriptor.ID, DisplayName: descriptor.DisplayName,
		State: PluginAvailabilityState(descriptor.State), Capabilities: []string{},
		Entitlement: pluginEntitlementToAPI(descriptor.Entitlement),
	}, nil
}

func pluginFeatureAllowed(descriptor plugins.Descriptor, feature string, mutating, safety bool) error {
	if descriptor.Allows(feature, mutating, safety) {
		return nil
	}
	state := descriptor.State
	if state == plugins.StateAvailable {
		state = plugins.StateNotEntitled
	}
	return pluginBoundaryError(409, "plugin_"+string(state), "The plugin cannot perform this action. Ask an administrator to check its availability and signed licence for this organization.")
}

func pluginServiceAllowed(item plugins.Installation, descriptor plugins.Descriptor, action string, mutating, safety bool) error {
	policy, exists := item.ServiceGrant(action)
	if !exists {
		return pluginBoundaryError(403, "plugin_service_forbidden", "The installation does not declare this service action.")
	}
	return pluginFeatureAllowed(descriptor, policy.RequiredFeature, mutating, safety)
}

// pluginServiceWarning reports a grace-period licence for the feature a
// service action requires. It is display information, never authorization.
func pluginServiceWarning(item plugins.Installation, descriptor plugins.Descriptor, action string) *PluginEntitlementWarning {
	policy, exists := item.ServiceGrant(action)
	if !exists {
		return nil
	}
	return pluginEntitlementWarningToAPI(descriptor.Entitlement.GraceWarning(policy.RequiredFeature))
}

// Availability codes of the in-transaction enablement check.
const (
	pluginNotInstalled = "plugin_not_installed"
	pluginNotEnabled   = "plugin_not_enabled"
)

// pluginEffectDatabase wraps the database of every Core effect performed for
// a plugin. Each transaction it begins first re-verifies, inside that
// transaction, that the installation the caller's snapshot named is still
// installed and enabled for the organization, then the licence of a licensed
// plugin. The snapshot is only a preflight; this is the authority.
//
// Only explicit plugin-effect calls receive this wrapper. Ordinary Module
// Management and deployment paths never acquire a plugin dependency.
func (s *Server) pluginEffectDatabase(org string, item plugins.Installation, action string, safety bool) model.Databaser {
	return &pluginEffectDatabase{Databaser: s.Database, guard: s.PluginEntitlements, org: org, item: item, action: action, safety: safety}
}

type pluginEffectDatabase struct {
	model.Databaser
	guard  PluginEntitlementGuard
	org    string
	item   plugins.Installation
	action string
	safety bool
}

func (db *pluginEffectDatabase) BeginTx(ctx context.Context, options *sql.TxOptions) (model.TxWithCommit, error) {
	licensed := db.item.Licensed()
	policy, exists := db.item.ServiceGrant(db.action)
	if licensed && (!exists || policy.RequiredFeature == "" || db.guard == nil) {
		return nil, pluginBoundaryError(409, "plugin_not_entitled", "The plugin requires a verified signed licence before this operation can proceed.")
	}
	// Enablement checks hold row locks and entitlement decisions persist their
	// monotonic clock and audit evidence. No effect caller may accidentally
	// substitute a read-only transaction.
	if options != nil && options.ReadOnly {
		return nil, pluginBoundaryError(500, "plugin_effect_transaction_invalid", "The plugin effect requires a writable authorization transaction.")
	}
	tx, err := db.Databaser.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	if err := model.RequirePluginEnablement(ctx, tx, model.PluginUse{OrgID: db.org, PluginID: db.item.ID, InstallationID: db.item.InstallationID, PackageSHA256: db.item.ManifestSHA256, Safety: db.safety}); err != nil {
		_ = tx.Rollback()
		return nil, pluginModelError(err)
	}
	if !licensed {
		return tx, nil
	}
	if err := db.guard.Guard(ctx, db.Databaser, tx, db.org, db.item, policy.RequiredFeature, true, db.safety); err != nil {
		// No business code has run yet. Persist an observed expiry even when
		// denying the command, so a later clock rollback cannot revive it.
		var denied *herrors.PlatformOrchestratorError
		if errors.As(err, &denied) && (denied.Code == "plugin_not_entitled" || denied.Code == pluginNotEnabled) {
			if commitErr := tx.Commit(); commitErr != nil {
				return nil, commitErr
			}
			return nil, err
		}
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}
