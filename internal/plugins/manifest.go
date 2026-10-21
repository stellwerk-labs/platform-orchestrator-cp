package plugins

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
)

// MaxManifestBytes bounds one encoded registration manifest.
const MaxManifestBytes = 1 << 20

// Manifest is what a plugin package declares to Core: identity, migration
// generation, operations, UI contribution, service grants, authorization
// policies and the entitlement declaration. Origins, service identities and
// organization enablement are operator data and never part of it.
type Manifest struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Version     string `json:"version"`
	// MigrationGeneration is the plugin-owned schema generation this package
	// runs on; 0 while the plugin owns no schema. Core serves the package only
	// once the running plugin reports its database at exactly this generation.
	MigrationGeneration   int64                   `json:"migration_generation,omitempty"`
	Capabilities          []Capability            `json:"capabilities,omitempty"`
	UI                    *UIContribution         `json:"ui,omitempty"`
	Operations            []Operation             `json:"operations,omitempty"`
	ServiceGrants         []ServiceGrant          `json:"service_grants,omitempty"`
	AuthorizationPolicies []AuthorizationPolicy   `json:"authorization_policies,omitempty"`
	Entitlement           *EntitlementDeclaration `json:"entitlement,omitempty"`
}

// Capability is an IAM capability a package declares in its own namespace.
// People receive it through the roles of an organization; Core registers the
// declaration in IAM for every organization the installation is enabled for.
type Capability struct {
	// ID is plugin.<plugin ID>.<name> with a one-segment name.
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	Category    string `json:"category"`
	// Level is the risk level: read, write or manage.
	Level string `json:"level"`
	// Scopes are the resource types a role may grant the capability on:
	// organization, project, environment.
	Scopes []string `json:"scopes"`
}

// CapabilityIDs returns the IDs of capabilities in their order.
func CapabilityIDs(capabilities []Capability) []string {
	ids := make([]string, 0, len(capabilities))
	for _, capability := range capabilities {
		ids = append(ids, capability.ID)
	}
	return ids
}

var identifier = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[.-][a-z0-9]+)+$`)
var canonicalVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*))?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)
var operationIdentifier = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_.-]{0,127}$`)
var pathParameter = regexp.MustCompile(`\{([a-zA-Z][a-zA-Z0-9_]*)\}`)

// localCapability is the one-segment name of a capability below its plugin's
// namespace, spelled as IAM accepts it.
var localCapability = regexp.MustCompile(`^[a-z](?:[a-z0-9-]*[a-z0-9])?$`)

// maxCapabilityID bounds a complete capability ID, as IAM does.
const maxCapabilityID = 255

var capabilityLevels = []string{"read", "write", "manage"}

// consoleRoute is a Console path below an organization in its one spelling:
// lowercase segments without a leading or trailing slash, as the Console
// accepts it.
var consoleRoute = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9/-]*[a-z0-9])?$`)
var contextualQueryKey = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

const orgPathParameter = "orgId"

// reservedPathPrefix belongs to Core's contract with the plugin backend, such
// as the plugin status endpoint; no plugin operation routes there.
const reservedPathPrefix = "/.well-known/"
const environmentScope = "environment"

// DecodeManifest reads exactly one bounded JSON manifest and rejects unknown
// fields, so a typo can never silently drop a declaration.
func DecodeManifest(raw []byte) (Manifest, error) {
	var manifest Manifest
	if len(raw) > MaxManifestBytes {
		return manifest, fmt.Errorf("plugin manifest exceeds %d bytes", MaxManifestBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode plugin manifest: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Manifest{}, fmt.Errorf("plugin manifest must contain one JSON document")
	}
	return manifest, nil
}

// Validate checks everything the package alone decides. The UI origin is
// operator configuration and must be absent.
func (m Manifest) Validate() error {
	if err := validateEntitlementDeclaration(m); err != nil {
		return err
	}
	if !identifier.MatchString(m.ID) || m.DisplayName == "" || !canonicalVersion.MatchString(m.Version) {
		return fmt.Errorf("invalid plugin identity")
	}
	if err := validateCapabilities(m); err != nil {
		return err
	}
	if m.MigrationGeneration < 0 {
		return fmt.Errorf("the migration generation must not be negative")
	}
	if m.UI != nil {
		if m.UI.Origin != "" {
			return fmt.Errorf("the plugin UI origin is operator configuration, not part of the manifest")
		}
		if err := validateUIContribution(*m.UI); err != nil {
			return err
		}
	}
	if err := validateServiceGrants(m); err != nil {
		return err
	}
	operations, err := validateOperations(m)
	if err != nil {
		return err
	}
	if m.UI != nil {
		return validateContextualActions(m.UI.ContextualActions, operations)
	}
	return nil
}

// validConsoleRoute accepts a Console route without empty segments.
func validConsoleRoute(route string) bool {
	return consoleRoute.MatchString(route) && !strings.Contains(route, "//")
}

// validRoutePrefix accepts a UI route prefix only in its one spelling, a
// slash followed by a Console route. Prefixes the Console serves alike are
// then equal strings, which only one installation can register.
func validRoutePrefix(prefix string) bool {
	route, rooted := strings.CutPrefix(prefix, "/")
	return rooted && validConsoleRoute(route)
}

func validateUIContribution(ui UIContribution) error {
	if ui.SDKVersion != "1" || !safePath(ui.Entrypoint) || !validRoutePrefix(ui.RoutePrefix) || ui.NavigationLabel == "" {
		return fmt.Errorf("invalid plugin UI contribution")
	}
	hostSeen := map[string]bool{}
	for _, op := range ui.CoreOperations {
		if !operationIdentifier.MatchString(op.ID) || !reservedHostOperation(op.ID) || hostSeen[op.ID] || (op.Method != http.MethodGet && op.Method != http.MethodPost) || op.Method == http.MethodGet && op.Mutating {
			return fmt.Errorf("invalid or duplicate Core host operation")
		}
		hostSeen[op.ID] = true
	}
	return nil
}

// validateOperations returns the registered operation IDs.
func validateOperations(m Manifest) (map[string]bool, error) {
	seen := map[string]bool{}
	for _, op := range m.Operations {
		if !operationIdentifier.MatchString(op.ID) || reservedHostOperation(op.ID) || seen[op.ID] || !safePath(op.Path) {
			return nil, fmt.Errorf("invalid or duplicate plugin operation")
		}
		if strings.HasPrefix(op.Path, reservedPathPrefix) {
			return nil, fmt.Errorf("plugin operations cannot use the reserved path prefix %s", reservedPathPrefix)
		}
		seen[op.ID] = true
		for _, action := range op.ServiceActions {
			if _, found := m.ServiceGrant(action); !found {
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
	return seen, nil
}

func validateContextualActions(actions []ContextualAction, operations map[string]bool) error {
	if len(actions) > 100 {
		return fmt.Errorf("too many contextual actions")
	}
	seen := map[string]bool{}
	for _, action := range actions {
		if !operationIdentifier.MatchString(action.ID) || seen[action.ID] || action.ResourceType != "module_version" || strings.TrimSpace(action.Label) == "" || len(action.Label) > 100 || !operations[action.RequiredOperationID] || !validConsoleRoute(action.Route.Path) || len(action.When.LifecycleStatus) == 0 {
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

// owns reports whether capability is an IAM capability in the plugin's own
// namespace, plugin.<plugin ID>.<local name>. The local name is one segment,
// so a plugin never reaches into the namespace of a plugin whose ID extends
// its own.
func (m Manifest) owns(capability string) bool {
	local, found := strings.CutPrefix(capability, "plugin."+m.ID+".")
	return found && localCapability.MatchString(local)
}

// validateCapabilities accepts each declared capability once and only in
// the plugin's own namespace, so no package claims a capability of Core or of
// another plugin, and only as complete as IAM registers it.
func validateCapabilities(m Manifest) error {
	seen := map[string]bool{}
	for _, capability := range m.Capabilities {
		if seen[capability.ID] || !m.owns(capability.ID) || len(capability.ID) > maxCapabilityID {
			return fmt.Errorf("capabilities must be unique and in the plugin's own namespace")
		}
		seen[capability.ID] = true
		if strings.TrimSpace(capability.DisplayName) == "" || strings.TrimSpace(capability.Description) == "" || strings.TrimSpace(capability.Category) == "" {
			return fmt.Errorf("capability %s needs a display name, a description and a category", capability.ID)
		}
		if !slices.Contains(capabilityLevels, capability.Level) {
			return fmt.Errorf("capability %s needs the level read, write or manage", capability.ID)
		}
		if !validScopes(capability.Scopes) {
			return fmt.Errorf("capability %s needs unique scopes of organization, project and environment", capability.ID)
		}
	}
	return nil
}

// validScopes accepts a non-empty set of the resource types a permission
// applies to.
func validScopes(scopes []string) bool {
	seen := map[string]bool{}
	for _, scope := range scopes {
		if seen[scope] || !slices.Contains([]string{"organization", "project", environmentScope}, scope) {
			return false
		}
		seen[scope] = true
	}
	return len(scopes) > 0
}

func validateServiceGrants(m Manifest) error {
	if len(m.ServiceGrants) == 0 {
		if len(m.AuthorizationPolicies) != 0 {
			return fmt.Errorf("authorization policies require explicit service grants")
		}
		return nil
	}
	seen := map[string]bool{}
	for _, grant := range m.ServiceGrants {
		if !operationIdentifier.MatchString(grant.Action) || seen[grant.Action] || !m.owns(grant.Permission) || (grant.Scope != "organization" && grant.Scope != environmentScope) {
			return fmt.Errorf("service grants require unique actions, owned capabilities and exact scope")
		}
		seen[grant.Action] = true
	}
	policySeen := map[string]bool{}
	for _, policy := range m.AuthorizationPolicies {
		if policySeen[policy.Permission] || !m.owns(policy.Permission) || !validScopes(policy.Scopes) {
			return fmt.Errorf("authorization policies require unique owned capabilities and exact scopes")
		}
		policySeen[policy.Permission] = true
	}
	if len(m.AuthorizationPolicies) != 0 {
		grant, ok := m.ServiceGrant("authorization.check")
		policy, policyOK := m.AuthorizationPolicy(grant.Permission)
		if !ok || !policyOK || !slices.Contains(policy.Scopes, grant.Scope) {
			return fmt.Errorf("authorization policies require an authorization.check grant covered by policy")
		}
	} else if _, ok := m.ServiceGrant("authorization.check"); ok {
		return fmt.Errorf("authorization.check requires explicit authorization policies")
	}
	return nil
}

// ValidateApprovalPolicies checks an organization's deployment approval
// policies against the policies the package owns.
func (m Manifest) ValidateApprovalPolicies(policies map[string]string) error {
	if len(policies) != 0 && len(m.ServiceGrants) == 0 {
		return fmt.Errorf("deployment approvals require explicit service grants")
	}
	for scope, permission := range policies {
		policy, ok := m.AuthorizationPolicy(permission)
		if !slices.Contains([]string{environmentScope, "project"}, scope) || !ok || !slices.Contains(policy.Scopes, scope) {
			return fmt.Errorf("deployment approvals require an owned authorization policy for the exact scope")
		}
	}
	return nil
}

func (m Manifest) ServiceGrant(action string) (ServiceGrant, bool) {
	for _, grant := range m.ServiceGrants {
		if grant.Action == action {
			return grant, true
		}
	}
	return ServiceGrant{}, false
}

func (m Manifest) AuthorizationPolicy(permission string) (AuthorizationPolicy, bool) {
	for _, policy := range m.AuthorizationPolicies {
		if policy.Permission == permission {
			return policy, true
		}
	}
	return AuthorizationPolicy{}, false
}
