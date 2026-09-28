package plugins

import (
	"context"
	"crypto/ed25519"
	"slices"
	"time"

	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v2/plugincontext"
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
	deny := func() (plugincontext.Claims, error) {
		return plugincontext.Claims{}, &GatewayError{"plugin_delegation_forbidden", "A valid declared service delegation is required.", 403}
	}
	if g == nil || g.Registry == nil || len(g.SigningKey) != ed25519.PrivateKeySize || len(proof.Body) > maxPluginBody {
		return deny()
	}
	item, descriptor, found := g.Registry.Installation(ctx, org, plugin)
	if !found || serviceUser == "" || item.ServiceUserID != serviceUser {
		return deny()
	}
	index := slices.IndexFunc(item.Operations, func(op Operation) bool { return op.ID == proof.OperationID })
	if index < 0 {
		return deny()
	}
	operation := item.Operations[index]
	if requireMutation && !operation.Mutating || !slices.Contains(operation.ServiceActions, action) {
		return deny()
	}
	for parameter, values := range operation.ServiceActionPathValues[action] {
		if !slices.Contains(values, proof.PathParameters[parameter]) {
			return deny()
		}
	}
	allowed := descriptor.State == StateAvailable || (descriptor.State == StateNotEnabled || descriptor.State == StateNotEntitled) && (!requireMutation || operation.IsSafety(proof.PathParameters))
	if !allowed {
		return deny()
	}
	path, err := operation.RequestPath(org, proof.PathParameters, proof.Query)
	if err != nil {
		return deny()
	}
	now := time.Now().UTC()
	if g.Now != nil {
		now = g.Now().UTC()
	}
	claims, err := plugincontext.Verify(g.SigningKey.Public().(ed25519.PublicKey), proof.Assertion, plugincontext.Request{Issuer: g.Issuer, Audience: "plugin:" + plugin, PluginID: plugin, OrgID: org, OperationID: operation.ID, Method: operation.Method, Path: path, Body: proof.Body, IdempotencyKey: proof.IdempotencyKey}, now)
	if err != nil {
		return deny()
	}
	return claims, nil
}
