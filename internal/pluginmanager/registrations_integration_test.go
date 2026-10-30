//go:build integration

package pluginmanager

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/plugincontext"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/pluginstatus"
)

// edited returns raw changed by edit, with its digest.
func edited(t *testing.T, raw json.RawMessage, edit func(manifest map[string]any)) (json.RawMessage, string) {
	t.Helper()
	var manifest map[string]any
	require.NoError(t, json.Unmarshal(raw, &manifest))
	edit(manifest)
	encoded, err := json.Marshal(manifest)
	require.NoError(t, err)
	sum := sha256.Sum256(encoded)
	return encoded, hex.EncodeToString(sum[:])
}

func routePrefix(prefix string) func(map[string]any) {
	return func(manifest map[string]any) { manifest["ui"].(map[string]any)["route_prefix"] = prefix }
}

// pluginBackends stands in for the plugin backends behind the gateway: it
// records which backend each call reached, with the delegation Core signed.
type pluginBackends struct {
	mu    sync.Mutex
	calls []*http.Request
}

func (b *pluginBackends) RoundTrip(request *http.Request) (*http.Response, error) {
	b.mu.Lock()
	b.calls = append(b.calls, request)
	b.mu.Unlock()
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"items":[]}`)), Request: request}, nil
}

func (b *pluginBackends) last() *http.Request {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls[len(b.calls)-1]
}

func TestRegistrationsNeverCollideAcrossInstallations(t *testing.T) {
	db := testDatabase(t)
	org := testOrganization(t, db)
	writer := newReplica(t, db)
	ops := newSession(t, writer.manager)
	ctx := t.Context()
	const rollouts, second = "dev.example.rollouts", "dev.example.second"
	first := ops.install(rollouts, org)
	other := ops.install(second, org)

	// Both plugins register the operations start and list. Core routes an
	// operation below its plugin ID, so each call reaches only the backend of
	// that plugin, with a delegation bound to that plugin.
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	backends := &pluginBackends{}
	gateway := &plugins.Gateway{Registry: writer.registry, SigningKey: private, Issuer: "core", Client: &http.Client{Transport: backends}}
	actor := userid.NewHumanUserId().String()
	for _, installation := range []Installation{first, other} {
		_, err := gateway.Invoke(ctx, org, installation.PluginID, actor, plugins.Invocation{OperationID: "list"})
		require.NoError(t, err)
		call := backends.last()
		backend, err := url.Parse(installation.BackendURL)
		require.NoError(t, err)
		require.Equal(t, backend.Host, call.URL.Host, "%s list reaches its own backend", installation.PluginID)
		_, err = plugincontext.Verify(public, call.Header.Get(plugincontext.Header), plugincontext.Request{
			Issuer: "core", Audience: "plugin:" + installation.PluginID, PluginID: installation.PluginID, OrgID: org,
			Method: call.Method, Path: call.URL.RequestURI(), OperationID: "list", IdempotencyKey: call.Header.Get("Idempotency-Key"),
		}, time.Now())
		require.NoError(t, err, "the delegation names %s", installation.PluginID)
	}

	// Each plugin serves only the capabilities of its own namespace.
	for plugin, want := range map[string][]string{
		rollouts: {"plugin.dev.example.rollouts.read", "plugin.dev.example.rollouts.approve"},
		second:   {"plugin.dev.example.second.read", "plugin.dev.example.second.approve"},
	} {
		descriptor, found := writer.registry.Get(ctx, org, plugin)
		require.True(t, found)
		require.ElementsMatch(t, want, descriptor.Capabilities, plugin)
	}

	// A package can neither claim another plugin's capability nor spell a
	// served UI route prefix differently.
	third, _ := pluginPackage(t, "dev.example.third")
	for name, edit := range map[string]func(map[string]any){
		"another plugin's capability": func(manifest map[string]any) {
			manifest["capabilities"].([]any)[0].(map[string]any)["id"] = "plugin.dev.example.rollouts.read"
		},
		"the served prefix with a trailing slash": routePrefix("/rollouts/"),
		"the served prefix in capitals":           routePrefix("/Rollouts"),
	} {
		raw, digest := edited(t, third, edit)
		_, err := writer.manager.PushPackage(ctx, ops.command(""), digest, raw)
		requireCode(t, err, "plugin_manifest_invalid", name)
	}

	// An upgrade cannot take the route prefix another installation serves:
	// the activation that would switch is refused and changes nothing, and
	// the predecessor stays served.
	successor, successorDigest := edited(t, mustVersion(t, second, "1.1.0", 0), routePrefix("/rollouts"))
	pkg, err := writer.manager.PushPackage(ctx, ops.command(""), successorDigest, successor)
	require.NoError(t, err)
	ops.revision = pkg.Revision
	ops.installation(writer.manager.Upgrade(ctx, ops.command(""), other.InstallationID, successorDigest))
	fleet.serve(t, other.BackendURL, pluginstatus.Report{InstallationID: other.InstallationID.String(), PackageSHA256: successorDigest})
	_, err = writer.manager.Transition(ctx, ops.command(""), other.InstallationID, ActionActivate)
	requireCode(t, err, "plugin_registration_conflict")
	pending, err := writer.manager.Installation(ctx, other.InstallationID)
	require.NoError(t, err)
	require.Equal(t, ops.revision, pending.Revision, "a refused switch changes no Plugin Manager state")
	require.Equal(t, other.PackageSHA256, pending.PackageSHA256)
	require.Equal(t, successorDigest, pending.TargetPackageSHA256)
	require.Equal(t, []string{other.PackageSHA256}, registeredPackages(t, db, other.InstallationID))
	servedUI := func(plugin string) string {
		t.Helper()
		item, _, found := writer.registry.Installation(ctx, org, plugin)
		require.True(t, found, plugin)
		return item.UI.RoutePrefix
	}
	require.Equal(t, "/second", servedUI(second))
	require.Equal(t, "/rollouts", servedUI(rollouts))

	// Uninstall releases the prefix: the switch now succeeds, and the
	// reinstalled first plugin cannot take its old prefix back.
	ops.installation(writer.manager.Transition(ctx, ops.command(""), first.InstallationID, ActionUninstall))
	switched := ops.installation(writer.manager.Transition(ctx, ops.command(""), other.InstallationID, ActionActivate))
	require.Equal(t, successorDigest, switched.PackageSHA256)
	require.Equal(t, "/rollouts", servedUI(second))
	ops.installation(writer.manager.Transition(ctx, ops.command(""), first.InstallationID, ActionReinstall))
	_, err = writer.manager.Transition(ctx, ops.command(""), first.InstallationID, ActionActivate)
	requireCode(t, err, "plugin_registration_conflict")
	reinstalled, err := writer.manager.Installation(ctx, first.InstallationID)
	require.NoError(t, err)
	require.Equal(t, model.PluginInstallationInstalled, reinstalled.State)
	require.Empty(t, registeredPackages(t, db, first.InstallationID))
}
