package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
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
	RequiredFeature         string                         `json:"required_feature,omitempty"`
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

// Installation is operator-controlled configuration, not a signed
// commercial entitlement. Entitlement verification belongs to a separate gate.
type Installation struct {
	InstallationID        string                       `json:"installation_id,omitempty"`
	ManifestSHA256        string                       `json:"manifest_sha256,omitempty"`
	Entitlement           *EntitlementDeclaration      `json:"entitlement,omitempty"`
	OrganizationTimezones map[string]string            `json:"organization_timezones,omitempty"`
	ID                    string                       `json:"id"`
	DisplayName           string                       `json:"display_name"`
	Version               string                       `json:"version"`
	Backend               string                       `json:"backend"`
	Organizations         map[string]AvailabilityState `json:"organizations"`
	Capabilities          []string                     `json:"capabilities"`
	UI                    *UIContribution              `json:"ui,omitempty"`
	Operations            []Operation                  `json:"operations"`
	ServiceUserID         string                       `json:"service_user_id,omitempty"`
	AdmissionCheckerID    string                       `json:"admission_checker_id,omitempty"`
	ServiceGrants         []ServiceGrant               `json:"service_grants,omitempty"`
	AuthorizationPolicies []AuthorizationPolicy        `json:"authorization_policies,omitempty"`
	// OrganizationApprovalPolicies maps an organization to its deployment
	// approval scopes (environment, project) and the owned authorization
	// policy each requires.
	OrganizationApprovalPolicies map[string]map[string]string `json:"organization_approval_policies,omitempty"`
}

// InstallationRegistry serves read decisions (discovery, routing, UI and
// descriptors) from an immutable snapshot of the Plugin Manager state. A
// refresh replaces the whole snapshot atomically; readers never see a mix.
// The snapshot is never authority for a Core effect: effects re-verify the
// installation and organization enablement in their own transaction.
type InstallationRegistry struct {
	snapshot     atomic.Pointer[installationSnapshot]
	Entitlements EntitlementResolver
}

type installationSnapshot struct {
	revision      int64
	installations map[string]Installation
}

var emptySnapshot = &installationSnapshot{installations: map[string]Installation{}}

// NewInstallationRegistry validates every installation and fails if any is
// invalid.
func NewInstallationRegistry(items []Installation, allowDevelopmentHTTP bool) (*InstallationRegistry, error) {
	registry := &InstallationRegistry{}
	if err := errors.Join(registry.Replace(0, items, allowDevelopmentHTTP)...); err != nil {
		return nil, err
	}
	return registry, nil
}

// Replace publishes the installations of one Plugin Manager revision. An
// invalid installation is left out and reported; it degrades only that plugin.
func (r *InstallationRegistry) Replace(revision int64, items []Installation, allowDevelopmentHTTP bool) []error {
	next := &installationSnapshot{revision: revision, installations: make(map[string]Installation, len(items))}
	var rejected []error
	for _, item := range items {
		item = detached(item)
		if err := ValidateInstallation(item, allowDevelopmentHTTP); err != nil {
			rejected = append(rejected, fmt.Errorf("plugin %s: %w", item.ID, err))
			continue
		}
		if _, exists := next.installations[item.ID]; exists {
			rejected = append(rejected, fmt.Errorf("duplicate plugin %s", item.ID))
			continue
		}
		zones := map[string]string{}
		for org := range item.Organizations {
			zones[org], _ = NormalizeTimezone(item.OrganizationTimezones[org])
		}
		item.OrganizationTimezones = zones
		next.installations[item.ID] = item
	}
	r.snapshot.Store(next)
	return rejected
}

// Revision is the Plugin Manager revision of the served snapshot.
func (r *InstallationRegistry) Revision() int64 {
	return r.current().revision
}

func (r *InstallationRegistry) current() *installationSnapshot {
	if r == nil {
		return emptySnapshot
	}
	if snapshot := r.snapshot.Load(); snapshot != nil {
		return snapshot
	}
	return emptySnapshot
}

// ValidateInstallation checks a complete installation: its manifest, operator
// endpoints and identities, and every organization's enablement.
func ValidateInstallation(item Installation, allowDevelopmentHTTP bool) error {
	manifest := item.Manifest()
	if err := manifest.Validate(); err != nil {
		return err
	}
	if err := validateEntitlementBinding(item); err != nil {
		return err
	}
	uiOrigin := ""
	if item.UI != nil {
		uiOrigin = item.UI.Origin
	}
	if err := ValidateEndpoints(manifest, item.Backend, uiOrigin, allowDevelopmentHTTP); err != nil {
		return err
	}
	if err := ValidateServiceIdentities(manifest, item.ServiceUserID, item.AdmissionCheckerID); err != nil {
		return err
	}
	for org, policies := range item.OrganizationApprovalPolicies {
		if _, exists := item.Organizations[org]; !exists {
			return fmt.Errorf("approval policies must name an installed organization")
		}
		if err := manifest.ValidateApprovalPolicies(policies); err != nil {
			return err
		}
	}
	for org, state := range item.Organizations {
		if org == "" || strings.ContainsAny(org, "/\\") {
			return fmt.Errorf("invalid plugin organization")
		}
		switch state {
		case StateAvailable, StateNotEnabled, StateNotEntitled, StateIncompatible, StateUnavailable:
		default:
			return fmt.Errorf("invalid plugin availability")
		}
	}
	for org, zone := range item.OrganizationTimezones {
		if _, exists := item.Organizations[org]; !exists {
			return fmt.Errorf("timezone must name an installed organization")
		}
		if _, err := NormalizeTimezone(zone); err != nil {
			return err
		}
	}
	return nil
}

// ValidateEndpoints checks the operator-chosen backend and UI origins. A UI
// origin exists exactly when the package contributes a UI.
func ValidateEndpoints(manifest Manifest, backend, uiOrigin string, allowDevelopmentHTTP bool) error {
	if err := validateOrigin(backend, allowDevelopmentHTTP); err != nil {
		return fmt.Errorf("backend: %w", err)
	}
	if manifest.UI == nil {
		if uiOrigin != "" {
			return fmt.Errorf("a UI origin requires a package UI contribution")
		}
		return nil
	}
	if err := validateOrigin(uiOrigin, allowDevelopmentHTTP); err != nil {
		return fmt.Errorf("UI: %w", err)
	}
	return nil
}

// ValidateServiceIdentities checks the plugin service user and admission
// checker. They exist exactly when the package declares service grants.
func ValidateServiceIdentities(manifest Manifest, serviceUser, admissionChecker string) error {
	if len(manifest.ServiceGrants) == 0 {
		if serviceUser != "" || admissionChecker != "" {
			return fmt.Errorf("service identities require explicit service grants")
		}
		return nil
	}
	for _, identity := range []string{serviceUser, admissionChecker} {
		id, err := uuid.Parse(identity)
		if err != nil || identity != id.String() || !userid.IsServiceUser(id) {
			return fmt.Errorf("plugin service identities must be canonical IAM service-user UUIDs")
		}
	}
	if serviceUser == admissionChecker {
		return fmt.Errorf("plugin and admission checker must have distinct identities")
	}
	return nil
}

// NormalizeTimezone returns the explicit IANA zone of an organization;
// omitted means UTC.
func NormalizeTimezone(zone string) (string, error) {
	if zone == "" {
		zone = "UTC"
	}
	if zone == "Local" {
		return "", fmt.Errorf("organization timezone must be an explicit IANA name")
	}
	if _, err := time.LoadLocation(zone); err != nil {
		return "", fmt.Errorf("invalid organization timezone: %w", err)
	}
	return zone, nil
}

// ServiceGrant is operator-owned policy. A plugin cannot choose the permission
// checked on its behalf, substitute a Core permission, or enlarge its scope.
type ServiceGrant struct {
	RequiredFeature string `json:"required_feature,omitempty"`
	Action          string `json:"action"`
	Permission      string `json:"permission"`
	Scope           string `json:"scope"`
}

// AuthorizationPolicy is the operator-owned catalogue of plugin-local
// capabilities that the plugin service may ask IAM to evaluate. It grants no
// capability by itself and can never name Core or another plugin's permission.
type AuthorizationPolicy struct {
	Permission string   `json:"permission"`
	Scopes     []string `json:"scopes"`
}

// ResourceEventsReadAction is the service grant that subscribes an installation
// to an organization's resource event journal.
const ResourceEventsReadAction = "resource-events.read"

// InventoryReadAction is the service grant for inventory reads: with a user
// delegation, or unattended within an operation that delegates it.
const InventoryReadAction = "inventory.read"

// Manifest returns the package declarations of the installation.
func (item Installation) Manifest() Manifest {
	manifest := Manifest{
		ID: item.ID, DisplayName: item.DisplayName, Version: item.Version, Capabilities: item.Capabilities,
		Operations: item.Operations, ServiceGrants: item.ServiceGrants, AuthorizationPolicies: item.AuthorizationPolicies, Entitlement: item.Entitlement,
	}
	if item.UI != nil {
		ui := *item.UI
		ui.Origin = ""
		manifest.UI = &ui
	}
	return manifest
}

func (item Installation) ServiceGrant(action string) (ServiceGrant, bool) {
	return item.Manifest().ServiceGrant(action)
}

func (item Installation) AuthorizationPolicy(permission string) (AuthorizationPolicy, bool) {
	return item.Manifest().AuthorizationPolicy(permission)
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

func (r *InstallationRegistry) Get(ctx context.Context, orgID, pluginID string) (Descriptor, bool) {
	return r.get(ctx, r.current(), orgID, pluginID)
}

func (r *InstallationRegistry) get(ctx context.Context, snapshot *installationSnapshot, orgID, pluginID string) (Descriptor, bool) {
	item, found := snapshot.installations[pluginID]
	if !found {
		return Descriptor{}, false
	}
	state, enabled := item.Organizations[orgID]
	if !enabled {
		return Descriptor{}, false
	}
	descriptor := Descriptor{ID: item.ID, DisplayName: item.DisplayName, Version: item.Version, Capabilities: slices.Clone(item.Capabilities), State: state, Mode: accessMode(state)}
	return r.applyEntitlement(ctx, orgID, item, descriptor), true
}

func (r *InstallationRegistry) List(ctx context.Context, orgID string) []Descriptor {
	snapshot := r.current()
	result := []Descriptor{}
	for id := range snapshot.installations {
		if d, ok := r.get(ctx, snapshot, orgID, id); ok {
			result = append(result, d)
		}
	}
	slices.SortFunc(result, func(a, b Descriptor) int { return strings.Compare(a.ID, b.ID) })
	return result
}

// Installation returns the installation and its descriptor from one snapshot.
func (r *InstallationRegistry) Installation(ctx context.Context, orgID, pluginID string) (Installation, Descriptor, bool) {
	snapshot := r.current()
	d, ok := r.get(ctx, snapshot, orgID, pluginID)
	if !ok {
		return Installation{}, Descriptor{}, false
	}
	return detached(snapshot.installations[pluginID]), d, true
}

// ConfiguredInstallation reads only detached operator metadata. Service
// identity must be checked before evaluating or activating a licence feature.
// This method by itself makes no claim about the plugin's usable authority.
func (r *InstallationRegistry) ConfiguredInstallation(orgID, pluginID string) (Installation, bool) {
	item, found := r.current().installations[pluginID]
	if _, configured := item.Organizations[orgID]; !found || !configured {
		return Installation{}, false
	}
	return detached(item), true
}

// detached deep-copies an installation. The struct contains only JSON data,
// so a round trip detaches every nested map and slice.
func detached(item Installation) Installation {
	encoded, _ := json.Marshal(item)
	var clone Installation
	_ = json.Unmarshal(encoded, &clone)
	return clone
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
