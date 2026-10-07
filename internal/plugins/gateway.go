package plugins

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v2/plugincontext"
	"go.uber.org/zap"
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
	Status             int                 `json:"status"`
	Body               json.RawMessage     `json:"body"`
	EntitlementWarning *EntitlementWarning `json:"entitlement_warning,omitempty"`
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
	// Logger records why a plugin call failed; nil uses the global logger.
	Logger *zap.Logger
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
	allowed := descriptor.Allows(operation.RequiredFeature, operation.Mutating, operation.IsSafety(invocation.PathParameters))
	if !allowed {
		code := "plugin_" + string(descriptor.State)
		if descriptor.State == StateAvailable {
			code = "plugin_not_entitled"
		}
		return nil, &GatewayError{code, "The plugin cannot accept this operation. Ask an administrator to check its availability and signed licence.", 409}
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
		return nil, g.callFailed(item, operation, &GatewayError{pluginUnavailable, "Plugin delegation is not configured.", 503}, "delegation", err)
	}
	req, err := http.NewRequestWithContext(ctx, operation.Method, strings.TrimSuffix(item.Backend, "/")+requestPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set(plugincontext.Header, assertion)
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	// Every receiver needs the generated request identity to exchange this exact
	// short-lived proof for a scoped Core authorization check. For mutations it
	// also remains the caller-supplied durable idempotency key.
	req.Header.Set("Idempotency-Key", requestID)
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
		class, cause := transportFailure(err)
		return nil, g.callFailed(item, operation, &GatewayError{pluginUnavailable, "The plugin service could not be reached.", 503}, class, cause)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return nil, g.callFailed(item, operation, &GatewayError{pluginUnavailable, "The plugin returned an unsupported redirect.", 502}, "redirect", fmt.Errorf("status %d", response.StatusCode))
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxPluginBody+1))
	if err != nil {
		class, cause := transportFailure(err)
		return nil, g.callFailed(item, operation, &GatewayError{pluginResponseInvalid, "The plugin response exceeded the permitted limit or was interrupted.", 502}, class, cause)
	}
	if len(data) > maxPluginBody {
		return nil, &GatewayError{pluginResponseInvalid, "The plugin response exceeded the permitted limit or was interrupted.", 502}
	}
	if len(data) == 0 {
		data = []byte("null")
	}
	if !json.Valid(data) {
		return nil, &GatewayError{pluginResponseInvalid, "The plugin returned a non-JSON response.", 502}
	}
	return &InvocationResult{Status: response.StatusCode, Body: data, EntitlementWarning: descriptor.Entitlement.GraceWarning(operation.RequiredFeature)}, nil
}

// callFailed logs why a call to the plugin service failed before the caller
// gets the stable result. It names the route template, never the concrete
// path, query, headers or body, which can carry organization data.
func (g *Gateway) callFailed(item Installation, operation *Operation, result *GatewayError, class string, cause error) *GatewayError {
	logger := g.Logger
	if logger == nil {
		logger = zap.L()
	}
	logger.Error("Plugin call failed", zap.String("plugin_id", item.ID), zap.String("installation_id", item.InstallationID),
		zap.String("operation_id", operation.ID), zap.String("method", operation.Method), zap.String("route", operation.Path),
		zap.String("code", result.Code), zap.Int("status", result.Status), zap.String("error_class", class), zap.String("cause", cause.Error()))
	return result
}

// transportFailure classifies a failed exchange with the plugin service. The
// cause drops the request URL that net/http wraps around it.
func transportFailure(err error) (string, error) {
	if wrapped, ok := errors.AsType[*url.Error](err); ok {
		err = wrapped.Err
	}
	var timeout net.Error
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled", err
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()):
		return "timeout", err
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection_refused", err
	case errors.Is(err, syscall.ECONNRESET) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
		return "connection_reset", err
	}
	if _, ok := errors.AsType[*net.DNSError](err); ok {
		return "dns", err
	}
	if _, ok := errors.AsType[*tls.CertificateVerificationError](err); ok {
		return "tls", err
	}
	return "transport", err
}

func (g *Gateway) Validate() error {
	if g == nil || g.Registry == nil || len(g.SigningKey) != ed25519.PrivateKeySize || g.Issuer == "" {
		return fmt.Errorf("plugin gateway needs registry, signing key and issuer")
	}
	return nil
}
