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

// Only explicit plugin-effect calls receive this wrapper. Ordinary Module
// Management and deployment paths never acquire an entitlement dependency.
func (s *Server) pluginEffectDatabase(org string, item plugins.Installation, action string, safety bool) model.Databaser {
	if item.Entitlement == nil || item.Entitlement.Mode == "none" {
		return s.Database
	}
	return &pluginEntitlementDatabase{Databaser: s.Database, guard: s.PluginEntitlements, org: org, item: item, action: action, safety: safety}
}

type pluginEntitlementDatabase struct {
	model.Databaser
	guard  PluginEntitlementGuard
	org    string
	item   plugins.Installation
	action string
	safety bool
}

func (db *pluginEntitlementDatabase) BeginTx(ctx context.Context, options *sql.TxOptions) (model.TxWithCommit, error) {
	policy, exists := db.item.ServiceGrant(db.action)
	if !exists || policy.RequiredFeature == "" || db.guard == nil {
		return nil, pluginBoundaryError(409, "plugin_not_entitled", "The plugin requires a verified signed licence before this operation can proceed.")
	}
	// Entitlement decisions persist their monotonic clock and audit evidence.
	// No effect caller may accidentally substitute a read-only transaction.
	if options != nil && options.ReadOnly {
		return nil, pluginBoundaryError(500, "plugin_entitlement_transaction_invalid", "The plugin effect requires a writable authorization transaction.")
	}
	tx, err := db.Databaser.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	if err := db.guard.Guard(ctx, db.Databaser, tx, db.org, db.item, policy.RequiredFeature, true, db.safety); err != nil {
		// No business code has run yet. Persist an observed expiry even when
		// denying the command, so a later clock rollback cannot revive it.
		var denied *herrors.PlatformOrchestratorError
		if errors.As(err, &denied) && (denied.Code == "plugin_not_entitled" || denied.Code == "plugin_not_enabled") {
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
