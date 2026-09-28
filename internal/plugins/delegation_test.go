package plugins

import (
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v2/plugincontext"
	"github.com/stretchr/testify/require"
)

func TestServiceDelegationBindsCallerActionAndExactOriginalRequest(t *testing.T) {
	_, key, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	item := Installation{ID: "dev.example.workflow", DisplayName: "Workflow", Version: "1.0.0", Backend: "https://workflow.example.test", ServiceUserID: uuid.NewString(), AdmissionCheckerID: uuid.NewString(), Organizations: map[string]AvailabilityState{"acme": StateAvailable}, ServiceGrants: []ServiceGrant{{Action: "operation.prepare", Permission: "plugin.dev.example.workflow.design", Scope: "organization"}}, Operations: []Operation{{ID: "prepare", Method: "POST", Path: "/orgs/{orgId}/workflows/{id}/prepare", Mutating: true, ServiceActions: []string{"operation.prepare"}}}}
	encoded, err := json.Marshal([]Installation{item})
	require.NoError(t, err)
	registry, err := LoadInstallations(strings.NewReader(string(encoded)), false)
	require.NoError(t, err)
	now := time.Now().UTC()
	gateway := &Gateway{Registry: registry, SigningKey: key, Issuer: "core", Now: func() time.Time { return now }}
	proof := DelegationProof{OperationID: "prepare", PathParameters: map[string]string{"id": uuid.NewString()}, Body: []byte("{\n  \"revision\": 1\n}"), IdempotencyKey: "request-1"}
	path, err := item.Operations[0].RequestPath("acme", proof.PathParameters, nil)
	require.NoError(t, err)
	claims := plugincontext.Claims{Version: 1, Issuer: "core", Audience: "plugin:" + item.ID, PluginID: item.ID, OrgID: "acme", ActorID: uuid.NewString(), OperationID: "prepare", Method: "POST", Path: path, BodySHA256: plugincontext.Digest(proof.Body), RequestID: proof.IdempotencyKey, IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix()}
	proof.Assertion, err = plugincontext.Sign(key, claims, now)
	require.NoError(t, err)
	// A JSON transport round trip must preserve whitespace in signed bytes.
	encoded, err = json.Marshal(proof)
	require.NoError(t, err)
	var transported DelegationProof
	require.NoError(t, json.Unmarshal(encoded, &transported))
	got, err := gateway.VerifyServiceDelegation(t.Context(), "acme", item.ID, item.ServiceUserID, "operation.prepare", transported)
	require.NoError(t, err)
	require.Equal(t, claims, got)
	_, err = gateway.VerifyServiceDelegation(t.Context(), "acme", item.ID, item.AdmissionCheckerID, "operation.prepare", proof)
	require.Error(t, err)
	_, err = gateway.VerifyServiceDelegation(t.Context(), "acme", item.ID, item.ServiceUserID, "module.version.unpin", proof)
	require.Error(t, err)
	_, err = gateway.VerifyServiceDelegation(t.Context(), "other-org", item.ID, item.ServiceUserID, "operation.prepare", proof)
	require.Error(t, err)
	changed := proof
	changed.Body = []byte(`{"revision":1}`)
	_, err = gateway.VerifyServiceDelegation(t.Context(), "acme", item.ID, item.ServiceUserID, "operation.prepare", changed)
	require.Error(t, err, "equal JSON meaning is not equal signed bytes")
	changed = proof
	changed.IdempotencyKey = "other-request"
	_, err = gateway.VerifyServiceDelegation(t.Context(), "acme", item.ID, item.ServiceUserID, "operation.prepare", changed)
	require.Error(t, err)
	gateway.Now = func() time.Time { return now.Add(time.Minute) }
	_, err = gateway.VerifyServiceDelegation(t.Context(), "acme", item.ID, item.ServiceUserID, "operation.prepare", proof)
	require.Error(t, err)
}

func TestServiceDelegationCannotExchangeCancelForActivation(t *testing.T) {
	_, key, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	item := Installation{ID: "dev.example.workflow", DisplayName: "Workflow", Version: "1.0.0", Backend: "https://workflow.example.test", ServiceUserID: uuid.NewString(), AdmissionCheckerID: uuid.NewString(), Organizations: map[string]AvailabilityState{"acme": StateNotEntitled}, ServiceGrants: []ServiceGrant{
		{Action: "operation.activate", Permission: "plugin.dev.example.workflow.control", Scope: "organization"},
		{Action: "operation.release", Permission: "plugin.dev.example.workflow.control", Scope: "organization"},
	}, Operations: []Operation{{ID: "command", Method: "POST", Path: "/orgs/{orgId}/workflows/{id}/{command}", Mutating: true, ServiceActions: []string{"operation.activate", "operation.release"}, SafetyPathValues: map[string][]string{"command": {"cancel"}}, ServiceActionPathValues: map[string]map[string][]string{
		"operation.activate": {"command": {"start", "resume"}}, "operation.release": {"command": {"cancel"}},
	}}}}
	encoded, err := json.Marshal([]Installation{item})
	require.NoError(t, err)
	registry, err := LoadInstallations(strings.NewReader(string(encoded)), false)
	require.NoError(t, err)
	now := time.Now().UTC()
	gateway := &Gateway{Registry: registry, SigningKey: key, Issuer: "core", Now: func() time.Time { return now }}
	proof := DelegationProof{OperationID: "command", PathParameters: map[string]string{"id": uuid.NewString(), "command": "cancel"}, Body: []byte(`{"reason":"Cancel reviewed work"}`), IdempotencyKey: "cancel-1"}
	path, err := item.Operations[0].RequestPath("acme", proof.PathParameters, nil)
	require.NoError(t, err)
	proof.Assertion, err = plugincontext.Sign(key, plugincontext.Claims{Version: 1, Issuer: "core", Audience: "plugin:" + item.ID, PluginID: item.ID, OrgID: "acme", ActorID: uuid.NewString(), OperationID: "command", Method: "POST", Path: path, BodySHA256: plugincontext.Digest(proof.Body), RequestID: proof.IdempotencyKey, IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix()}, now)
	require.NoError(t, err)
	_, err = gateway.VerifyServiceDelegation(t.Context(), "acme", item.ID, item.ServiceUserID, "operation.release", proof)
	require.NoError(t, err, "entitlement expiry still permits declared safety actions")
	_, err = gateway.VerifyServiceDelegation(t.Context(), "acme", item.ID, item.ServiceUserID, "operation.activate", proof)
	require.Error(t, err, "a signed cancel is not activation authority")
}

func TestReadDelegationCannotAuthorizeMutation(t *testing.T) {
	_, key, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	item := Installation{ID: "dev.example.workflow", DisplayName: "Workflow", Version: "1.0.0", Backend: "https://workflow.example.test", ServiceUserID: uuid.NewString(), AdmissionCheckerID: uuid.NewString(), Organizations: map[string]AvailabilityState{"acme": StateNotEntitled}, ServiceGrants: []ServiceGrant{{Action: "inventory.read", Permission: "plugin.dev.example.workflow.read", Scope: "organization"}}, Operations: []Operation{{ID: "read", Method: "GET", Path: "/orgs/{orgId}/workflows", ServiceActions: []string{"inventory.read"}}}}
	encoded, err := json.Marshal([]Installation{item})
	require.NoError(t, err)
	registry, err := LoadInstallations(strings.NewReader(string(encoded)), false)
	require.NoError(t, err)
	now := time.Now().UTC()
	gateway := &Gateway{Registry: registry, SigningKey: key, Issuer: "core", Now: func() time.Time { return now }}
	proof := DelegationProof{OperationID: "read", IdempotencyKey: "read-1"}
	proof.Assertion, err = plugincontext.Sign(key, plugincontext.Claims{Version: 1, Issuer: "core", Audience: "plugin:" + item.ID, PluginID: item.ID, OrgID: "acme", ActorID: uuid.NewString(), OperationID: "read", Method: "GET", Path: "/orgs/acme/workflows", BodySHA256: plugincontext.Digest(nil), RequestID: proof.IdempotencyKey, IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix()}, now)
	require.NoError(t, err)
	_, err = gateway.VerifyServiceReadDelegation(t.Context(), "acme", item.ID, item.ServiceUserID, "inventory.read", proof)
	require.NoError(t, err, "historical inspection remains available after entitlement expiry")
	_, err = gateway.VerifyServiceDelegation(t.Context(), "acme", item.ID, item.ServiceUserID, "inventory.read", proof)
	require.Error(t, err, "the mutation verifier does not accept a read proof")
	_, err = gateway.VerifyServiceReadDelegation(t.Context(), "acme", item.ID, item.ServiceUserID, "operation.activate", proof)
	require.Error(t, err)
	gateway.Now = func() time.Time { return now.Add(time.Minute) }
	_, err = gateway.VerifyServiceReadDelegation(t.Context(), "acme", item.ID, item.ServiceUserID, "inventory.read", proof)
	require.Error(t, err, "read delegation still expires")
}
