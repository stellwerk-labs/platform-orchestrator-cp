package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/hecho"
	"github.com/stellwerk-labs/golib/herrors"
	iamauthz "github.com/stellwerk-labs/platform-orchestrator-iam/shared/authz"
	iamapi "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/pluginmanager"
)

// iamPeer stands in for IAM behind its HTTP contract. It answers platform
// checks from the operators configured for each permission, allows every
// organization check as for an organization administrator and counts them,
// answers workload binding and platform principal lookups from what it
// holds, and keeps each organization's catalogue of plugin capabilities as
// IAM does, all to the internal system identity that Core calls IAM with.
type iamPeer struct {
	mu         sync.Mutex
	holders    map[string][]uuid.UUID
	bindings   []iamapi.PluginWorkloadBinding
	principals map[uuid.UUID]iamapi.PlatformPrincipal
	// catalogue holds the capabilities of each organization and plugin.
	catalogue map[string]map[string][]iamapi.PluginCapabilityRegistration
	// replacements counts the catalogue replacements of each organization.
	replacements map[string]int
	platform     []iamapi.ResourcePermissionCheck
	// client calls the peer as Core's internal system identity.
	client    iamapi.ClientWithResponsesInterface
	orgChecks atomic.Int32
	// lookups counts binding and principal lookups, answered or not.
	lookups atomic.Int32
	// catalogueReads counts reads of organizations' permissions.
	catalogueReads atomic.Int32
	// lookupDown, principalsDown and catalogueDown make binding, principal
	// and capability requests fail as if IAM were unavailable;
	// membershipsDown does so for adding an organization's founder.
	lookupDown, principalsDown, catalogueDown, membershipsDown atomic.Bool
}

// founderRole is the Admin role IAM creates in every new organization.
var founderRole = iamapi.Role{Id: uuid.MustParse("0199a0c1-1d6e-72f1-8000-0000000000ad"), DisplayName: "Admin"}

// iamPermission is one permission of IAM's own catalogue, which every
// organization lists before the plugin capabilities.
var iamPermission = iamapi.PermissionDefinition{Id: "module_read", DisplayName: "Read modules", Description: "View modules.", Category: "Modules",
	Level: iamapi.Read, Scopes: []iamapi.PermissionDefinitionScopes{iamapi.Organization}}

// capabilitySlug is IAM's rule for the one-segment name of a capability.
var capabilitySlug = regexp.MustCompile(`^[a-z](?:[a-z0-9-]*[a-z0-9])?$`)

func (p *iamPeer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/internal/authorize":
		var body iamapi.InternalAuthorizeBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		failed := []iamapi.ResourcePermissionCheck{}
		p.mu.Lock()
		for _, check := range body.Checks {
			if check.Resource != iamauthz.PlatformResource {
				p.orgChecks.Add(1)
				continue
			}
			p.platform = append(p.platform, check)
			if !slices.Contains(p.holders[check.Permission], body.UserId) {
				failed = append(failed, check)
			}
		}
		p.mu.Unlock()
		if len(failed) == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "HTTP-403", "message": "denied", "details": map[string]any{"failed_checks": failed}})
	case r.Header.Get("From") != userid.InternalSystemUuid.String():
		w.WriteHeader(http.StatusForbidden)
	case r.Method == http.MethodGet && r.URL.Path == "/internal/plugin-workload-bindings":
		p.lookups.Add(1)
		if p.lookupDown.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		query := r.URL.Query()
		items := []iamapi.PluginWorkloadBinding{}
		p.mu.Lock()
		for _, binding := range p.bindings {
			if binding.PluginId == query.Get("plugin_id") && binding.InstallationId.String() == query.Get("installation_id") {
				items = append(items, binding)
			}
		}
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(iamapi.PluginWorkloadBindingList{Items: items})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/platform-principals/"):
		p.lookups.Add(1)
		if p.principalsDown.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		id, err := uuid.Parse(strings.TrimPrefix(r.URL.Path, "/platform-principals/"))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		p.mu.Lock()
		principal, found := p.principals[id]
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if !found {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "HTTP-404", "message": "platform principal not found"})
			return
		}
		_ = json.NewEncoder(w).Encode(principal)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/orgs/") && strings.HasSuffix(r.URL.Path, "/permissions"):
		p.catalogueReads.Add(1)
		if p.catalogueDown.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		org := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/orgs/"), "/permissions")
		page := iamapi.PermissionDefinitionPage{Items: []iamapi.PermissionDefinition{iamPermission}}
		p.mu.Lock()
		for plugin, capabilities := range p.catalogue[org] {
			for _, capability := range capabilities {
				scopes := []iamapi.PermissionDefinitionScopes{}
				for _, scope := range capability.Scopes {
					scopes = append(scopes, iamapi.PermissionDefinitionScopes(scope))
				}
				page.Items = append(page.Items, iamapi.PermissionDefinition{Id: "plugin." + plugin + "." + capability.Name, DisplayName: capability.DisplayName,
					Description: capability.Description, Category: capability.Category, Level: iamapi.PermissionDefinitionLevel(capability.Level), Scopes: scopes})
			}
		}
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(page)
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/orgs/") && strings.HasSuffix(r.URL.Path, "/capabilities"):
		if p.catalogueDown.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/orgs/"), "/")
		if len(parts) != 4 || parts[1] != "plugins" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		org, plugin := parts[0], parts[2]
		var body iamapi.PluginCapabilityRegistrationBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !validCapabilities(body.Capabilities) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "HTTP-400", "message": "invalid plugin capability definition"})
			return
		}
		p.mu.Lock()
		if p.catalogue[org] == nil {
			p.catalogue[org] = map[string][]iamapi.PluginCapabilityRegistration{}
		}
		if len(body.Capabilities) == 0 {
			delete(p.catalogue[org], plugin)
		} else {
			p.catalogue[org][plugin] = body.Capabilities
		}
		p.replacements[org]++
		p.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/users/") && strings.HasSuffix(r.URL.Path, "/memberships"):
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(iamapi.UserMembershipPage{Items: []iamapi.UserMembership{}})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/orgs/") && strings.HasSuffix(r.URL.Path, "/roles"):
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(iamapi.RolePage{Items: []iamapi.Role{founderRole}})
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/internal/orgs/") && strings.HasSuffix(r.URL.Path, "/memberships"):
		if p.membershipsDown.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	default:
		w.WriteHeader(http.StatusServiceUnavailable)
	}
}

// validCapabilities applies IAM's rules for a capability catalogue.
func validCapabilities(capabilities []iamapi.PluginCapabilityRegistration) bool {
	seen := map[string]bool{}
	for _, capability := range capabilities {
		if !capabilitySlug.MatchString(capability.Name) || seen[capability.Name] || capability.DisplayName == "" || capability.Description == "" ||
			capability.Category == "" || len(capability.Scopes) == 0 || !capability.Level.Valid() {
			return false
		}
		seen[capability.Name] = true
		for _, scope := range capability.Scopes {
			if !scope.Valid() {
				return false
			}
		}
	}
	return true
}

// platformChecks returns the platform checks asked so far.
func (p *iamPeer) platformChecks() []iamapi.ResourcePermissionCheck {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.platform)
}

// newIAMPeer serves an iamPeer in which writers hold plugin_manager_write
// and readers plugin_manager_read, and returns a client for it that calls as
// the internal system identity, like Core's.
func newIAMPeer(t *testing.T, writers, readers []uuid.UUID) (*iamPeer, iamapi.ClientWithResponsesInterface) {
	t.Helper()
	peer := &iamPeer{holders: map[string][]uuid.UUID{
		iamauthz.PermissionPluginManagerRead:  readers,
		iamauthz.PermissionPluginManagerWrite: writers,
	}, principals: map[uuid.UUID]iamapi.PlatformPrincipal{}, catalogue: map[string]map[string][]iamapi.PluginCapabilityRegistration{}, replacements: map[string]int{}}
	iam := httptest.NewServer(peer)
	t.Cleanup(iam.Close)
	client, err := iamapi.NewClientWithResponses(iam.URL, iamapi.WithRequestEditorFn(func(_ context.Context, request *http.Request) error {
		request.Header.Set("From", userid.InternalSystemUuid.String())
		return nil
	}))
	require.NoError(t, err)
	peer.client = client
	return peer, client
}

func TestOnlyConfiguredOperatorsHoldPluginManagerPermissions(t *testing.T) {
	writer, reader := userid.NewHumanUserId(), userid.NewHumanUserId()
	peer, client := newIAMPeer(t, []uuid.UUID{writer}, []uuid.UUID{writer, reader})
	s := &Server{IamClient: client, PluginManager: &pluginmanager.Manager{}}
	as := func(actor uuid.UUID) context.Context {
		return context.WithValue(t.Context(), hecho.ContextKeyUserID, actor.String())
	}
	allowed := func(actor uuid.UUID, permission string) bool {
		t.Helper()
		caller, err := s.pluginManagerCaller(as(actor), permission)
		if err != nil {
			var refused *herrors.PlatformOrchestratorError
			require.ErrorAs(t, err, &refused)
			require.Equal(t, http.StatusForbidden, refused.StatusCode)
			require.Equal(t, "plugin_manager_forbidden", refused.Code)
			return false
		}
		require.Equal(t, actor, caller)
		return true
	}

	require.True(t, allowed(writer, iamauthz.PermissionPluginManagerWrite))
	require.True(t, allowed(writer, iamauthz.PermissionPluginManagerRead))
	require.True(t, allowed(reader, iamauthz.PermissionPluginManagerRead))
	require.False(t, allowed(reader, iamauthz.PermissionPluginManagerWrite))
	for name, actor := range map[string]uuid.UUID{
		"organization admin": userid.NewHumanUserId(), "plugin workload": userid.NewServiceUserTokenId(), "internal system": userid.InternalSystemUuid,
	} {
		require.False(t, allowed(actor, iamauthz.PermissionPluginManagerRead), name)
	}
	require.Equal(t, iamauthz.PlatformCheck(iamauthz.PermissionPluginManagerRead), peer.platformChecks()[len(peer.platformChecks())-1],
		"Core asks IAM for the platform permission, never for an organization role")
	require.Zero(t, peer.orgChecks.Load())

	asked := len(peer.platformChecks())
	disabled := &Server{IamClient: client}
	_, err := disabled.pluginManagerCaller(as(writer), iamauthz.PermissionPluginManagerRead)
	require.ErrorContains(t, err, "plugin_host_disabled")
	require.Len(t, peer.platformChecks(), asked, "no Plugin Manager, no IAM question")

	unavailable, err := iamapi.NewClientWithResponses("http://127.0.0.1:1")
	require.NoError(t, err)
	_, err = (&Server{IamClient: unavailable, PluginManager: &pluginmanager.Manager{}}).pluginManagerCaller(as(writer), iamauthz.PermissionPluginManagerWrite)
	require.Error(t, err)
	var refused *herrors.PlatformOrchestratorError
	require.NotErrorAs(t, err, &refused, "an unreachable IAM is an error, never a decision")
}
