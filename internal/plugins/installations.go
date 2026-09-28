package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

type AccessMode string

const (
	ModeFull       AccessMode = "full"
	ModeReadOnly   AccessMode = "read_only"
	ModeSafetyOnly AccessMode = "safety_only"
	ModeHidden     AccessMode = "hidden"
)

type UIContribution struct {
	SDKVersion      string `json:"sdk_version"`
	Origin          string `json:"origin"`
	Entrypoint      string `json:"entrypoint"`
	RoutePrefix     string `json:"route_prefix"`
	NavigationLabel string `json:"navigation_label"`
}

type Operation struct {
	ID               string              `json:"id"`
	Method           string              `json:"method"`
	Path             string              `json:"path"`
	QueryKeys        []string            `json:"query_keys,omitempty"`
	Mutating         bool                `json:"mutating"`
	Safety           bool                `json:"safety,omitempty"`
	PathValues       map[string][]string `json:"path_values,omitempty"`
	SafetyPathValues map[string][]string `json:"safety_path_values,omitempty"`
}

// Installation is operator-controlled experimental configuration, not a signed
// commercial entitlement. Entitlement verification belongs to a separate gate.
type Installation struct {
	ID            string                       `json:"id"`
	DisplayName   string                       `json:"display_name"`
	Version       string                       `json:"version"`
	Backend       string                       `json:"backend"`
	Organizations map[string]AvailabilityState `json:"organizations"`
	Capabilities  []string                     `json:"capabilities"`
	UI            *UIContribution              `json:"ui,omitempty"`
	Operations    []Operation                  `json:"operations"`
}

type InstallationRegistry struct{ installations map[string]Installation }

var identifier = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[.-][a-z0-9]+)+$`)
var operationIdentifier = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_.-]{0,127}$`)
var pathParameter = regexp.MustCompile(`\{([a-zA-Z][a-zA-Z0-9_]*)\}`)

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
		for _, op := range item.Operations {
			if !operationIdentifier.MatchString(op.ID) || seen[op.ID] || !safePath(op.Path) {
				return nil, fmt.Errorf("invalid or duplicate plugin operation")
			}
			seen[op.ID] = true
			switch op.Method {
			case "GET":
				if op.Mutating {
					return nil, fmt.Errorf("GET cannot mutate")
				}
			case "POST", "PUT", "PATCH", "DELETE":
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
		registry.installations[item.ID] = item
	}
	return registry, nil
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
		if key == "orgId" {
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
		if key == "orgId" || !used[key] {
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
