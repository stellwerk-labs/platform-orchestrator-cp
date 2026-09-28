package plugins

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v2/plugincontext"
)

const maxPluginBody = 16 << 20
const pluginUnavailable = "plugin_unavailable"
const pluginRequestInvalid = "plugin_request_invalid"
const pluginResponseInvalid = "plugin_response_invalid"

type Invocation struct {
	OperationID    string              `json:"operation_id"`
	PathParameters map[string]string   `json:"path_parameters,omitempty"`
	Query          map[string][]string `json:"query,omitempty"`
	Body           json.RawMessage     `json:"body,omitempty"`
	IdempotencyKey string              `json:"idempotency_key,omitempty"`
}

type InvocationResult struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

type GatewayError struct {
	Code    string
	Message string
	Status  int
}

func (e *GatewayError) Error() string { return e.Message }

type Gateway struct {
	Registry   *InstallationRegistry
	SigningKey ed25519.PrivateKey
	Issuer     string
	Client     *http.Client
	Now        func() time.Time
}

// Invoke never forwards the caller's cookies, bearer token, From header, or an
// arbitrary URL. The caller must have authenticated and checked org membership.
// Each add-on must verify the assertion and perform resource-level IAM checks.
func (g *Gateway) Invoke(ctx context.Context, org, pluginID, actor string, invocation Invocation) (*InvocationResult, error) {
	if g == nil || g.Registry == nil {
		return nil, &GatewayError{"plugin_not_installed", "The plugin is not installed. Ask an administrator to install it.", 404}
	}
	item, descriptor, found := g.Registry.Installation(ctx, org, pluginID)
	if !found {
		return nil, &GatewayError{"plugin_not_installed", "The plugin is not installed for this organization.", 404}
	}
	var operation *Operation
	for i := range item.Operations {
		if item.Operations[i].ID == invocation.OperationID {
			operation = &item.Operations[i]
			break
		}
	}
	if operation == nil {
		return nil, &GatewayError{"plugin_operation_unknown", "This operation is not registered by the installed plugin.", 400}
	}
	allowed := descriptor.State == StateAvailable || ((descriptor.State == StateNotEnabled || descriptor.State == StateNotEntitled) && (!operation.Mutating || operation.IsSafety(invocation.PathParameters)))
	if !allowed {
		return nil, &GatewayError{"plugin_" + string(descriptor.State), "The plugin cannot accept this operation. Ask an administrator to check its availability.", 409}
	}
	if _, err := uuid.Parse(actor); err != nil {
		return nil, &GatewayError{"plugin_actor_invalid", "An authenticated actor is required.", 401}
	}
	if operation.Mutating && invocation.IdempotencyKey == "" || len(invocation.IdempotencyKey) > 128 || strings.ContainsAny(invocation.IdempotencyKey, "\r\n") {
		return nil, &GatewayError{"plugin_idempotency_required", "A non-empty idempotency key (up to 128 characters) is required.", 400}
	}
	requestPath, err := operation.RequestPath(org, invocation.PathParameters, invocation.Query)
	if err != nil {
		return nil, &GatewayError{pluginRequestInvalid, err.Error(), 400}
	}
	body := invocation.Body
	if len(body) > maxPluginBody || (len(body) > 0 && !json.Valid(body)) || (operation.Method == http.MethodGet && len(body) > 0 && string(body) != "null") {
		return nil, &GatewayError{pluginRequestInvalid, "Invalid plugin request body.", 400}
	}
	if operation.Method == http.MethodGet {
		body = nil
	}
	now := time.Now().UTC()
	if g.Now != nil {
		now = g.Now().UTC()
	}
	requestID := invocation.IdempotencyKey
	if requestID == "" {
		requestID = uuid.NewString()
	}
	assertion, err := plugincontext.Sign(g.SigningKey, plugincontext.Claims{
		Version: 1, Issuer: g.Issuer, Audience: "plugin:" + pluginID, PluginID: pluginID, OrgID: org, ActorID: actor,
		OperationID: operation.ID, Method: operation.Method, Path: requestPath, BodySHA256: plugincontext.Digest(body),
		RequestID: requestID, IssuedAt: now.Unix(), ExpiresAt: now.Add(plugincontext.MaxLifetime).Unix(),
	}, now)
	if err != nil {
		return nil, &GatewayError{pluginUnavailable, "Plugin delegation is not configured.", 503}
	}
	req, err := http.NewRequestWithContext(ctx, operation.Method, strings.TrimSuffix(item.Backend, "/")+requestPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set(plugincontext.Header, assertion)
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if operation.Method != http.MethodGet {
		req.Header.Set("Idempotency-Key", requestID)
	}
	client := g.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	// Copy before overriding redirect handling so caller-owned clients stay intact.
	bounded := *client
	if bounded.Timeout <= 0 || bounded.Timeout > 30*time.Second {
		bounded.Timeout = 30 * time.Second
	}
	bounded.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	response, err := bounded.Do(req)
	if err != nil {
		return nil, &GatewayError{pluginUnavailable, "The plugin service could not be reached.", 503}
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return nil, &GatewayError{pluginUnavailable, "The plugin returned an unsupported redirect.", 502}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxPluginBody+1))
	if err != nil || len(data) > maxPluginBody {
		return nil, &GatewayError{pluginResponseInvalid, "The plugin response exceeded the permitted limit or was interrupted.", 502}
	}
	if len(data) == 0 {
		data = []byte("null")
	}
	if !json.Valid(data) {
		return nil, &GatewayError{pluginResponseInvalid, "The plugin returned a non-JSON response.", 502}
	}
	return &InvocationResult{Status: response.StatusCode, Body: data}, nil
}

func (g *Gateway) Validate() error {
	if g == nil || g.Registry == nil || len(g.SigningKey) != ed25519.PrivateKeySize || g.Issuer == "" {
		return fmt.Errorf("plugin gateway needs registry, signing key and issuer")
	}
	return nil
}
