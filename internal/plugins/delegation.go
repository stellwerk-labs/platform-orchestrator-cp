package plugins

import (
	"context"
	"crypto/ed25519"
	"slices"
	"time"

	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/plugincontext"
)

// DelegationProof retains exact body bytes (base64 in JSON), not a decoded JSON
// value which a serializer might change. It proves one original gateway call.
// It is never retained as a long-lived scheduler credential.
type DelegationProof struct {
	OperationID    string              `json:"operation_id"`
	PathParameters map[string]string   `json:"path_parameters,omitempty"`
	Query          map[string][]string `json:"query,omitempty"`
	Body           []byte              `json:"body,omitempty"`
	IdempotencyKey string              `json:"idempotency_key"`
	Assertion      string              `json:"assertion"`
}

func (g *Gateway) VerifyServiceDelegation(ctx context.Context, org, plugin, serviceUser, action string, proof DelegationProof) (plugincontext.Claims, error) {
	return g.verifyServiceDelegation(ctx, org, plugin, serviceUser, action, proof, true)
}

// VerifyServiceReadDelegation can authorize a declared read port from either a
// read or write source operation. It must never be used for a mutating port.
func (g *Gateway) VerifyServiceReadDelegation(ctx context.Context, org, plugin, serviceUser, action string, proof DelegationProof) (plugincontext.Claims, error) {
	return g.verifyServiceDelegation(ctx, org, plugin, serviceUser, action, proof, false)
}

func (g *Gateway) verifyServiceDelegation(ctx context.Context, org, plugin, serviceUser, action string, proof DelegationProof, requireMutation bool) (plugincontext.Claims, error) {
	if g == nil || g.Registry == nil || len(g.SigningKey) != ed25519.PrivateKeySize || len(proof.Body) > maxPluginBody {
		return serviceDelegationDenied()
	}
	item, descriptor, found := g.Registry.Installation(ctx, org, plugin)
	if !found {
		return serviceDelegationDenied()
	}
	return g.verifyServiceDelegationForInstallation(ctx, org, plugin, item, descriptor, serviceUser, action, proof, requireMutation)
}

// VerifyServiceDelegationWithFencedInstallation verifies a mutating proof without
// re-entering Registry discovery. The caller must hold the current installation,
// enablement and BOTH source-operation and service-action entitlement guards in
// its effect transaction. The descriptor is only a conservative preflight;
// current authority comes from those guards. Signature, scope and Admin checks
// still run here after the guards have been acquired.
func (g *Gateway) VerifyServiceDelegationWithFencedInstallation(ctx context.Context, org string, item Installation, descriptor Descriptor, serviceUser, action string, proof DelegationProof) (plugincontext.Claims, error) {
	return g.verifyServiceDelegationForInstallation(ctx, org, item.ID, item, descriptor, serviceUser, action, proof, true)
}

func (g *Gateway) verifyServiceDelegationForInstallation(ctx context.Context, org, plugin string, item Installation, descriptor Descriptor, serviceUser, action string, proof DelegationProof, requireMutation bool) (plugincontext.Claims, error) {
	if g == nil || len(g.SigningKey) != ed25519.PrivateKeySize || len(proof.Body) > maxPluginBody {
		return serviceDelegationDenied()
	}
	if item.ID != plugin || descriptor.ID != plugin || plugin == "" || serviceUser == "" || item.ServiceUserID != serviceUser {
		return serviceDelegationDenied()
	}
	index := slices.IndexFunc(item.Operations, func(op Operation) bool { return op.ID == proof.OperationID })
	if index < 0 {
		return serviceDelegationDenied()
	}
	operation := item.Operations[index]
	if requireMutation && !operation.Mutating || !slices.Contains(operation.ServiceActions, action) {
		return serviceDelegationDenied()
	}
	for parameter, values := range operation.ServiceActionPathValues[action] {
		if !slices.Contains(values, proof.PathParameters[parameter]) {
			return serviceDelegationDenied()
		}
	}
	allowed := descriptor.Allows(operation.RequiredFeature, requireMutation, operation.IsSafety(proof.PathParameters))
	grant, granted := item.ServiceGrant(action)
	allowed = allowed && granted && descriptor.Allows(grant.RequiredFeature, requireMutation, operation.IsSafety(proof.PathParameters))
	if !allowed {
		return serviceDelegationDenied()
	}
	path, err := operation.RequestPath(org, proof.PathParameters, proof.Query)
	if err != nil {
		return serviceDelegationDenied()
	}
	now := time.Now().UTC()
	if g.Now != nil {
		now = g.Now().UTC()
	}
	claims, err := plugincontext.Verify(g.SigningKey.Public().(ed25519.PublicKey), proof.Assertion, plugincontext.Request{Issuer: g.Issuer, Audience: "plugin:" + plugin, PluginID: plugin, OrgID: org, OperationID: operation.ID, Method: operation.Method, Path: path, Body: proof.Body, IdempotencyKey: proof.IdempotencyKey}, now)
	if err != nil {
		return serviceDelegationDenied()
	}
	if operation.RequireOrgAdmin {
		if err := g.RequireOrganizationAdmin(ctx, org, claims.ActorID); err != nil {
			return plugincontext.Claims{}, err
		}
	}
	return claims, nil
}

func serviceDelegationDenied() (plugincontext.Claims, error) {
	return plugincontext.Claims{}, &GatewayError{"plugin_delegation_forbidden", "A valid declared service delegation is required.", 403}
}
