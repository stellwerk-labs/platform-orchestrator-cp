package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v2/plugincontext"
)

const maxDataPlaneBody = 8 << 20

// DataPlanePort sends only closed, typed-by-action service requests to the
// operator's fixed DP origin. It does not accept caller URLs or credentials.
// Callers authorize exact scopes before using it; mutation callers also record
// their exact dispatch for DP's independent Core callback before Call.
type DataPlanePort struct {
	Origin  string
	Gateway *Gateway
	Client  *http.Client
}

type DataPlaneRequest struct {
	OrgID           string
	PluginID        string
	ActorID         uuid.UUID
	Action          string
	RequestID       string
	ProjectUUID     uuid.UUID
	EnvironmentUUID uuid.UUID
	DeploymentID    uuid.UUID
	DispatchID      string
	Body            json.RawMessage
}

func NewDataPlanePort(origin string, gateway *Gateway, allowHTTP bool) (*DataPlanePort, error) {
	if err := validateOrigin(origin, allowHTTP); err != nil {
		return nil, err
	}
	if err := gateway.Validate(); err != nil {
		return nil, err
	}
	return &DataPlanePort{Origin: strings.TrimSuffix(origin, "/"), Gateway: gateway}, nil
}

func dataPlaneRequestPath(request DataPlaneRequest) (string, string, error) {
	invalid := &GatewayError{pluginRequestInvalid, "An exact supported Data Plane service request is required.", 400}
	if request.OrgID == "" || request.OrgID == "." || request.OrgID == ".." || strings.ContainsAny(request.OrgID, "/\\%?#\r\n") || !identifier.MatchString(request.PluginID) || request.ActorID == uuid.Nil || request.ActorID.String() == "ffffffff-ffff-ffff-ffff-ffffffffffff" || request.RequestID == "" || len(request.RequestID) > 128 || strings.ContainsAny(request.RequestID, "\r\n") {
		return "", "", invalid
	}
	base := "/orgs/" + url.PathEscape(request.OrgID) + "/plugins/" + url.PathEscape(request.PluginID)
	switch request.Action {
	case "external-deployment.readiness":
		return http.MethodPost, base + "/external-deployment-readiness", nil
	case "external-deployment.plan":
		return http.MethodPost, base + "/external-deployment-plans", nil
	case "external-deployment.admit":
		return http.MethodPost, base + "/external-deployments", nil
	case "external-deployment.read":
		if request.ProjectUUID == uuid.Nil || request.EnvironmentUUID == uuid.Nil || len(request.Body) != 0 {
			return "", "", invalid
		}
		scope := base + "/projects/" + request.ProjectUUID.String() + "/envs/" + request.EnvironmentUUID.String()
		if request.DispatchID != "" {
			if request.DeploymentID != uuid.Nil || len(request.DispatchID) > 128 || request.DispatchID == "." || request.DispatchID == ".." || strings.ContainsAny(request.DispatchID, "/\\%?#\r\n") {
				return "", "", invalid
			}
			return http.MethodGet, scope + "/external-deployment-dispatches/" + url.PathEscape(request.DispatchID), nil
		}
		if request.DeploymentID == uuid.Nil {
			return "", "", invalid
		}
		return http.MethodGet, scope + "/external-deployments/" + request.DeploymentID.String(), nil
	default:
		return "", "", invalid
	}
}

func (p *DataPlanePort) Call(ctx context.Context, request DataPlaneRequest) (*InvocationResult, error) {
	if p == nil || p.Gateway == nil {
		return nil, &GatewayError{pluginUnavailable, "The Data Plane service port is not configured.", 503}
	}
	method, path, err := dataPlaneRequestPath(request)
	if err != nil {
		return nil, err
	}
	if len(request.Body) > maxDataPlaneBody || method == http.MethodPost && !json.Valid(request.Body) {
		return nil, &GatewayError{pluginRequestInvalid, "A bounded JSON Data Plane request is required.", 400}
	}
	now := time.Now().UTC()
	if p.Gateway.Now != nil {
		now = p.Gateway.Now().UTC()
	}
	assertion, err := plugincontext.Sign(p.Gateway.SigningKey, plugincontext.Claims{Version: 1, Issuer: p.Gateway.Issuer, Audience: "platform-orchestrator-dp", PluginID: request.PluginID, OrgID: request.OrgID, ActorID: request.ActorID.String(), OperationID: request.Action, Method: method, Path: path, BodySHA256: plugincontext.Digest(request.Body), RequestID: request.RequestID, IssuedAt: now.Unix(), ExpiresAt: now.Add(plugincontext.MaxLifetime).Unix()}, now)
	if err != nil {
		return nil, &GatewayError{pluginUnavailable, "Data Plane delegation is not configured.", 503}
	}
	outbound, err := http.NewRequestWithContext(ctx, method, p.Origin+path, bytes.NewReader(request.Body))
	if err != nil {
		return nil, err
	}
	outbound.Header.Set(plugincontext.Header, assertion)
	outbound.Header.Set("Idempotency-Key", request.RequestID)
	if method == http.MethodPost {
		outbound.Header.Set("Content-Type", "application/json")
	}
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	bounded := *client
	if bounded.Timeout <= 0 || bounded.Timeout > 30*time.Second {
		bounded.Timeout = 30 * time.Second
	}
	bounded.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	response, err := bounded.Do(outbound)
	if err != nil {
		return nil, &GatewayError{pluginUnavailable, "The Data Plane could not be reached; reconcile an uncertain mutation before retrying execution.", 503}
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return nil, &GatewayError{pluginResponseInvalid, "The Data Plane returned an unsupported redirect.", 502}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxDataPlaneBody+1))
	if err != nil || len(body) > maxDataPlaneBody || !json.Valid(body) {
		return nil, &GatewayError{pluginResponseInvalid, "The Data Plane returned an invalid or oversized response.", 502}
	}
	return &InvocationResult{Status: response.StatusCode, Body: body}, nil
}
