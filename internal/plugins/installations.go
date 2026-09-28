package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"
)

type AccessMode string

const (
	ModeFull       AccessMode = "full"
	ModeReadOnly   AccessMode = "read_only"
	ModeSafetyOnly AccessMode = "safety_only"
	ModeHidden     AccessMode = "hidden"
)

type UIContribution struct {
	SDKVersion        string             `json:"sdk_version"`
	Origin            string             `json:"origin"`
	Entrypoint        string             `json:"entrypoint"`
	RoutePrefix       string             `json:"route_prefix"`
	NavigationLabel   string             `json:"navigation_label"`
	CoreOperations    []HostOperation    `json:"core_operations,omitempty"`
	ContextualActions []ContextualAction `json:"contextual_actions,omitempty"`
}

type ContextualAction struct {
	ID                  string               `json:"id"`
	ResourceType        string               `json:"resource_type"`
	Label               string               `json:"label"`
	RequiredOperationID string               `json:"required_operation_id"`
	Route               ContextualRoute      `json:"route"`
	When                ContextualConditions `json:"when"`
	Primary             bool                 `json:"primary,omitempty"`
}

type ContextualRoute struct {
	Path  string            `json:"path"`
	Query map[string]string `json:"query,omitempty"`
}

type ContextualConditions struct {
	LifecycleStatus         []string `json:"lifecycle_status"`
	SemanticVersionRequired bool     `json:"semantic_version_required"`
}

// HostOperation names a closed Console SDK operation. It is never sent to the
// plugin backend, and metadata is not a grant of the user's Core permissions.
type HostOperation struct {
	ID       string `json:"id"`
	Method   string `json:"method"`
	Mutating bool   `json:"mutating"`
}

type Operation struct {
	ID                      string                         `json:"id"`
	Method                  string                         `json:"method"`
	Path                    string                         `json:"path"`
	QueryKeys               []string                       `json:"query_keys,omitempty"`
	Mutating                bool                           `json:"mutating"`
	Safety                  bool                           `json:"safety,omitempty"`
	PathValues              map[string][]string            `json:"path_values,omitempty"`
	SafetyPathValues        map[string][]string            `json:"safety_path_values,omitempty"`
	ServiceActions          []string                       `json:"service_actions,omitempty"`
	ServiceActionPathValues map[string]map[string][]string `json:"service_action_path_values,omitempty"`
}

// Installation is operator-controlled experimental configuration, not a signed
// commercial entitlement. Entitlement verification belongs to a separate gate.
type Installation struct {
	ID                 string                       `json:"id"`
	DisplayName        string                       `json:"display_name"`
	Version            string                       `json:"version"`
	Backend            string                       `json:"backend"`
	Organizations      map[string]AvailabilityState `json:"organizations"`
	Capabilities       []string                     `json:"capabilities"`
	UI                 *UIContribution              `json:"ui,omitempty"`
	Operations         []Operation                  `json:"operations"`
	ServiceUserID      string                       `json:"service_user_id,omitempty"`
	AdmissionCheckerID string                       `json:"admission_checker_id,omitempty"`
	ServiceGrants      []ServiceGrant               `json:"service_grants,omitempty"`
}

type InstallationRegistry struct{ installations map[string]Installation }

var identifier = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[.-][a-z0-9]+)+$`)
var operationIdentifier = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_.-]{0,127}$`)
var pathParameter = regexp.MustCompile(`\{([a-zA-Z][a-zA-Z0-9_]*)\}`)

const orgPathParameter = "orgId"

func LoadInstallations(reader io.Reader, allowDevelopmentHTTP bool) (*InstallationRegistry, error) {
	var items []Installation
	decoder := json.NewDecoder(io.LimitReader(reader, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&items); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("plugin configuration must contain one JSON document")
	}
	registry := &InstallationRegistry{installations: map[string]Installation{}}
	for _, item := range items {
		if !identifier.MatchString(item.ID) || item.DisplayName == "" || item.Version == "" {
			return nil, fmt.Errorf("invalid plugin identity")
		}
		if _, exists := registry.installations[item.ID]; exists {
			return nil, fmt.Errorf("duplicate plugin %s", item.ID)
		}
		if err := validateOrigin(item.Backend, allowDevelopmentHTTP); err != nil {
			return nil, fmt.Errorf("plugin %s backend: %w", item.ID, err)
		}
		if item.UI != nil {
			if err := validateOrigin(item.UI.Origin, allowDevelopmentHTTP); err != nil {
				return nil, fmt.Errorf("plugin %s UI: %w", item.ID, err)
			}
			if item.UI.SDKVersion != "1" || !safePath(item.UI.Entrypoint) || !safePath(item.UI.RoutePrefix) || item.UI.NavigationLabel == "" {
				return nil, fmt.Errorf("invalid plugin UI contribution")
			}
			hostSeen := map[string]bool{}
			for _, op := range item.UI.CoreOperations {
				if !operationIdentifier.MatchString(op.ID) || !reservedHostOperation(op.ID) || hostSeen[op.ID] || (op.Method != http.MethodGet && op.Method != http.MethodPost) || op.Method == http.MethodGet && op.Mutating {
					return nil, fmt.Errorf("invalid or duplicate Core host operation")
				}
				hostSeen[op.ID] = true
			}
		}
		for org, state := range item.Organizations {
			if org == "" || strings.ContainsAny(org, "/\\") {
				return nil, fmt.Errorf("invalid plugin organization")
			}
			switch state {
			case StateAvailable, StateNotEnabled, StateNotEntitled, StateIncompatible, StateUnavailable:
			default:
				return nil, fmt.Errorf("invalid plugin availability")
			}
		}
		seen := map[string]bool{}
		if err := validateServiceGrants(item); err != nil {
			return nil, err
		}
		for _, op := range item.Operations {
			if !operationIdentifier.MatchString(op.ID) || reservedHostOperation(op.ID) || seen[op.ID] || !safePath(op.Path) {
				return nil, fmt.Errorf("invalid or duplicate plugin operation")
			}
			seen[op.ID] = true
			for _, action := range op.ServiceActions {
				if _, found := item.ServiceGrant(action); !found {
					return nil, fmt.Errorf("operation refers to undeclared service action")
				}
			}
			for action, constraints := range op.ServiceActionPathValues {
				if !slices.Contains(op.ServiceActions, action) || len(constraints) == 0 {
					return nil, fmt.Errorf("service action constraints need a declared action")
				}
				for parameter, values := range constraints {
					if !strings.Contains(op.Path, "{"+parameter+"}") || parameter == orgPathParameter || len(values) == 0 {
						return nil, fmt.Errorf("service action constraint must bind a real operation path parameter")
					}
				}
			}
			switch op.Method {
			case "GET":
				if op.Mutating {
					return nil, fmt.Errorf("GET cannot mutate")
				}
			case "POST":
				// A bounded preview can use a JSON body without changing state.
			case "PUT", "PATCH", "DELETE":
				if !op.Mutating {
					return nil, fmt.Errorf("write operation must be marked mutating")
				}
			default:
				return nil, fmt.Errorf("unsupported plugin method")
			}
			if strings.Contains(pathParameter.ReplaceAllString(op.Path, ""), "{") || strings.Contains(pathParameter.ReplaceAllString(op.Path, ""), "}") {
				return nil, fmt.Errorf("invalid path template")
			}
		}
		if item.UI != nil {
			if err := validateContextualActions(item.UI.ContextualActions, seen); err != nil {
				return nil, err
			}
		}
		registry.installations[item.ID] = item
	}
	return registry, nil
}

var contextualRoute = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9/-]*[a-z0-9])?$`)
var contextualQueryKey = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

func validateContextualActions(actions []ContextualAction, operations map[string]bool) error {
	if len(actions) > 100 {
		return fmt.Errorf("too many contextual actions")
	}
	seen := map[string]bool{}
	for _, action := range actions {
		if !operationIdentifier.MatchString(action.ID) || seen[action.ID] || action.ResourceType != "module_version" || strings.TrimSpace(action.Label) == "" || len(action.Label) > 100 || !operations[action.RequiredOperationID] || !contextualRoute.MatchString(action.Route.Path) || strings.Contains(action.Route.Path, "//") || len(action.When.LifecycleStatus) == 0 {
			return fmt.Errorf("invalid contextual action identity, route, condition or required plugin operation")
		}
		seen[action.ID] = true
		statuses := map[string]bool{}
		for _, status := range action.When.LifecycleStatus {
			if !slices.Contains([]string{"proposed", "default", "deprecated", "defective"}, status) || statuses[status] {
				return fmt.Errorf("invalid contextual lifecycle condition")
			}
			statuses[status] = true
		}
		for key, value := range action.Route.Query {
			if !contextualQueryKey.MatchString(key) || !slices.Contains([]string{"$module_id", "$semantic_version", "$version_uuid"}, value) {
				return fmt.Errorf("invalid contextual query substitution")
			}
		}
	}
	return nil
}

func reservedHostOperation(id string) bool {
	return strings.HasPrefix(id, "core.") || strings.HasPrefix(id, "iam.")
}

// ServiceGrant is operator-owned policy. A plugin cannot choose the permission
// checked on its behalf, substitute a Core permission, or enlarge its scope.
type ServiceGrant struct {
	Action     string `json:"action"`
	Permission string `json:"permission"`
	Scope      string `json:"scope"`
}

var localCapability = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

func validateServiceGrants(item Installation) error {
	if len(item.ServiceGrants) == 0 {
		if item.ServiceUserID != "" || item.AdmissionCheckerID != "" {
			return fmt.Errorf("service identities require explicit service grants")
		}
		return nil
	}
	for _, identity := range []string{item.ServiceUserID, item.AdmissionCheckerID} {
		id, err := uuid.Parse(identity)
		if err != nil || id == uuid.Nil || identity == "ffffffff-ffff-ffff-ffff-ffffffffffff" {
			return fmt.Errorf("plugin service identities must be explicit non-system UUIDs")
		}
	}
	if item.ServiceUserID == item.AdmissionCheckerID {
		return fmt.Errorf("plugin and admission checker must have distinct identities")
	}
	seen := map[string]bool{}
	for _, grant := range item.ServiceGrants {
		prefix := "plugin." + item.ID + "."
		if !operationIdentifier.MatchString(grant.Action) || seen[grant.Action] || !strings.HasPrefix(grant.Permission, prefix) || !localCapability.MatchString(strings.TrimPrefix(grant.Permission, prefix)) || (grant.Scope != "organization" && grant.Scope != "environment") {
			return fmt.Errorf("service grants require unique actions, owned capabilities and exact scope")
		}
		seen[grant.Action] = true
	}
	return nil
}

func (item Installation) ServiceGrant(action string) (ServiceGrant, bool) {
	for _, grant := range item.ServiceGrants {
		if grant.Action == action {
			return grant, true
		}
	}
	return ServiceGrant{}, false
}

func validateOrigin(raw string, development bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return fmt.Errorf("expected an origin without credentials, path or query")
	}
	if u.Scheme != "https" && (!development || u.Scheme != "http") {
		return fmt.Errorf("HTTPS required outside explicit development mode")
	}
	return nil
}

func safePath(path string) bool {
	return strings.HasPrefix(path, "/") && !strings.Contains(path, "//") && !strings.ContainsAny(path, "\\%?#\r\n") && !slices.Contains(strings.Split(path, "/"), "..") && !slices.Contains(strings.Split(path, "/"), ".")
}

func accessMode(state AvailabilityState) AccessMode {
	switch state {
	case StateAvailable:
		return ModeFull
	case StateNotEnabled, StateNotEntitled:
		return ModeSafetyOnly
	case StateUnavailable, StateIncompatible:
		return ModeHidden
	default:
		return ModeHidden
	}
}

func (r *InstallationRegistry) Get(_ context.Context, orgID, pluginID string) (Descriptor, bool) {
	if r == nil {
		return Descriptor{}, false
	}
	item, found := r.installations[pluginID]
	if !found {
		return Descriptor{}, false
	}
	state, enabled := item.Organizations[orgID]
	if !enabled {
		return Descriptor{}, false
	}
	return Descriptor{ID: item.ID, DisplayName: item.DisplayName, Version: item.Version, Capabilities: slices.Clone(item.Capabilities), State: state, Mode: accessMode(state)}, true
}

func (r *InstallationRegistry) List(ctx context.Context, orgID string) []Descriptor {
	result := []Descriptor{}
	if r == nil {
		return result
	}
	for id := range r.installations {
		if d, ok := r.Get(ctx, orgID, id); ok {
			result = append(result, d)
		}
	}
	slices.SortFunc(result, func(a, b Descriptor) int { return strings.Compare(a.ID, b.ID) })
	return result
}

func (r *InstallationRegistry) Installation(ctx context.Context, orgID, pluginID string) (Installation, Descriptor, bool) {
	d, ok := r.Get(ctx, orgID, pluginID)
	if !ok {
		return Installation{}, Descriptor{}, false
	}
	// JSON round trip avoids sharing mutable slices/maps with route callers.
	encoded, _ := json.Marshal(r.installations[pluginID])
	var i Installation
	_ = json.Unmarshal(encoded, &i)
	return i, d, true
}

func (op Operation) RequestPath(org string, parameters map[string]string, query map[string][]string) (string, error) {
	used := map[string]bool{}
	var invalid bool
	path := pathParameter.ReplaceAllStringFunc(op.Path, func(match string) string {
		key := strings.Trim(match, "{}")
		value, exists := parameters[key]
		if key == orgPathParameter {
			value, exists = org, true
		}
		if !exists || value == "" || value == "." || value == ".." || strings.ContainsAny(value, "/\\%?#\r\n") {
			invalid = true
		}
		if choices, ok := op.PathValues[key]; ok && !slices.Contains(choices, value) {
			invalid = true
		}
		used[key] = true
		return url.PathEscape(value)
	})
	for key := range parameters {
		if key == orgPathParameter || !used[key] {
			invalid = true
		}
	}
	if invalid {
		return "", fmt.Errorf("invalid plugin operation path parameters")
	}
	values := url.Values{}
	for key, value := range query {
		if !slices.Contains(op.QueryKeys, key) {
			return "", fmt.Errorf("unregistered plugin query parameter")
		}
		for _, part := range value {
			values.Add(key, part)
		}
	}
	if encoded := values.Encode(); encoded != "" {
		path += "?" + encoded
	}
	return path, nil
}

func (op Operation) IsSafety(parameters map[string]string) bool {
	if op.Safety {
		return true
	}
	if len(op.SafetyPathValues) == 0 {
		return false
	}
	for key, values := range op.SafetyPathValues {
		if !slices.Contains(values, parameters[key]) {
			return false
		}
	}
	return true
}
