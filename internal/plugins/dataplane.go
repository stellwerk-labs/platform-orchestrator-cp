package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/plugincontext"
)

const maxDataPlaneBody = 8 << 20
const MaxDeploymentMaterialBytes = 16 << 20
const DataPlaneLogsRead = "external-deployment.logs.read"
const DataPlaneOutputsRead = "external-deployment.outputs.read"

type DeploymentMaterialResult struct {
	Status int
	Body   []byte
}

// DataPlanePort sends only closed, typed-by-action service requests to the
// operator's fixed DP origin. It does not accept caller URLs or credentials.
// Callers authorize exact scopes before using it; mutation callers also record
// their exact dispatch for DP's independent Core callback before Call.
type protocolDecision struct {
	allowed bool
	until   time.Time
}

type DataPlanePort struct {
	protocolMutex     sync.Mutex
	protocolDecisions map[string]protocolDecision

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
	ObservationID   uuid.UUID
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
	case "observation.request.resolve", "observation.redeploy.resolve":
		if request.ObservationID == uuid.Nil || len(request.Body) == 0 {
			return "", "", invalid
		}
		collection := "requests"
		if request.Action == "observation.redeploy.resolve" {
			collection = "redeployments"
		}
		return http.MethodPost, base + "/observations/v1/" + collection + "/" + request.ObservationID.String() + "/resolve", nil
	case "observation.capabilities":
		if len(request.Body) != 0 {
			return "", "", invalid
		}
		return http.MethodGet, base + "/observations/v1/capabilities", nil
	case "observation.redeploy.get":
		if request.ObservationID == uuid.Nil || len(request.Body) != 0 {
			return "", "", invalid
		}
		return http.MethodGet, base + "/observations/v1/redeployments/" + request.ObservationID.String(), nil
	case "observation.redeploy":
		if request.ObservationID == uuid.Nil {
			return "", "", invalid
		}
		return http.MethodPost, base + "/observations/v1/runs/" + request.ObservationID.String() + "/redeploy", nil
	case "observation.try-start":
		return http.MethodPost, base + "/observations/v1/runs", nil
	case "observation.get", "observation.request.get":
		if request.ObservationID == uuid.Nil || len(request.Body) != 0 {
			return "", "", invalid
		}
		kind := "runs"
		if request.Action == "observation.request.get" {
			kind = "requests"
		}
		return http.MethodGet, base + "/observations/v1/" + kind + "/" + request.ObservationID.String(), nil
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
	case DataPlaneLogsRead, DataPlaneOutputsRead:
		if request.ProjectUUID == uuid.Nil || request.EnvironmentUUID == uuid.Nil || request.DeploymentID == uuid.Nil || request.DispatchID != "" || len(request.Body) != 0 {
			return "", "", invalid
		}
		kind := "logs"
		if request.Action == DataPlaneOutputsRead {
			kind = "outputs"
		}
		return http.MethodGet, base + "/projects/" + request.ProjectUUID.String() + "/envs/" + request.EnvironmentUUID.String() + "/external-deployments/" + request.DeploymentID.String() + "/" + kind, nil
	default:
		return "", "", invalid
	}
}

func (p *DataPlanePort) Call(ctx context.Context, request DataPlaneRequest) (*InvocationResult, error) {
	if request.Action == DataPlaneLogsRead || request.Action == DataPlaneOutputsRead {
		return nil, &GatewayError{pluginRequestInvalid, "Protected material requires the user-only material transport.", 400}
	}
	response, err := p.do(ctx, request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxDataPlaneBody+1))
	if err != nil || len(body) > maxDataPlaneBody || !json.Valid(body) {
		return nil, &GatewayError{pluginResponseInvalid, "The Data Plane returned an invalid or oversized response.", 502}
	}
	return &InvocationResult{Status: response.StatusCode, Body: body}, nil
}

// ReadMaterial is only used by the authenticated Core user proxy. The plugin
// service transport cannot expose this data through an invocation envelope.
func (p *DataPlanePort) ReadMaterial(ctx context.Context, request DataPlaneRequest) (*DeploymentMaterialResult, error) {
	if request.Action != DataPlaneLogsRead && request.Action != DataPlaneOutputsRead {
		return nil, &GatewayError{pluginRequestInvalid, "An exact Deployment material request is required.", 400}
	}
	response, err := p.do(ctx, request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	limit := MaxDeploymentMaterialBytes
	if response.StatusCode != http.StatusOK {
		limit = 8192
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, int64(limit+1)))
	if err != nil {
		return nil, &GatewayError{pluginResponseInvalid, "The Data Plane material response could not be read.", 502}
	}
	if len(body) > limit {
		return nil, &GatewayError{"deployment_material_too_large", "Deployment material exceeds the bounded download size.", http.StatusRequestEntityTooLarge}
	}
	if response.StatusCode == http.StatusOK {
		contentType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
		if err != nil || contentType != "text/plain" {
			return nil, &GatewayError{pluginResponseInvalid, "The Data Plane returned an unsupported material content type.", 502}
		}
	} else if !json.Valid(body) {
		return nil, &GatewayError{pluginResponseInvalid, "The Data Plane returned an invalid material error response.", 502}
	}
	return &DeploymentMaterialResult{Status: response.StatusCode, Body: body}, nil
}

func (p *DataPlanePort) do(ctx context.Context, request DataPlaneRequest) (*http.Response, error) {
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
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		_ = response.Body.Close()
		return nil, &GatewayError{pluginResponseInvalid, "The Data Plane returned an unsupported redirect.", 502}
	}
	return response, nil
}

// SupportsProtocol is a bounded no-effect capability probe, cached briefly to
// avoid adding a round trip to every discovery/proof check. A missing/old or
// incomplete DP never advertises the complete optional Core contract.
func (p *DataPlanePort) SupportsProtocol(ctx context.Context, org string, item Installation, protocol string) bool {
	if p == nil || protocol != EnvironmentObservationProtocol && protocol != EnvironmentObservationRecoveryProtocol {
		return false
	}
	identity, err := uuid.Parse(item.ServiceUserID)
	if err != nil {
		return false
	}
	key := org + ":" + item.InstallationID + ":" + item.ManifestSHA256 + ":" + item.ServiceUserID + ":" + protocol
	now := time.Now()
	p.protocolMutex.Lock()
	cached, exists := p.protocolDecisions[key]
	p.protocolMutex.Unlock()
	if exists && now.Before(cached.until) {
		return cached.allowed
	}
	bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	result, err := p.Call(bounded, DataPlaneRequest{OrgID: org, PluginID: item.ID, ActorID: identity, Action: "observation.capabilities", RequestID: uuid.NewString()})
	supported := false
	if err == nil && result.Status == http.StatusOK {
		var capabilities struct {
			Protocols []string `json:"protocols"`
		}
		if json.Unmarshal(result.Body, &capabilities) == nil {
			for _, supportedProtocol := range capabilities.Protocols {
				supported = supported || supportedProtocol == protocol
			}
		}
	}
	p.protocolMutex.Lock()
	if p.protocolDecisions == nil || len(p.protocolDecisions) > 10000 {
		p.protocolDecisions = map[string]protocolDecision{}
	}
	p.protocolDecisions[key] = protocolDecision{allowed: supported, until: time.Now().Add(2 * time.Second)}
	p.protocolMutex.Unlock()
	return supported
}
