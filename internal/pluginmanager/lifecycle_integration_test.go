//go:build integration

package pluginmanager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v2/pluginstatus"
)

// pluginProcess is a plugin backend on a real TLS listener. It reports
// whatever its runtime currently configures.
type pluginProcess struct {
	*httptest.Server
	mu     sync.Mutex
	report pluginstatus.Report
}

func startPlugin(t *testing.T) *pluginProcess {
	t.Helper()
	process := &pluginProcess{}
	process.Server = httptest.NewTLSServer(pluginstatus.Handler(func(context.Context) (pluginstatus.Report, error) {
		process.mu.Lock()
		defer process.mu.Unlock()
		if process.report.InstallationID == "" {
			return pluginstatus.Report{}, context.DeadlineExceeded
		}
		return process.report, nil
	}))
	t.Cleanup(process.Close)
	return process
}

func (p *pluginProcess) reports(installation uuid.UUID, digest string, generation int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.report = pluginstatus.Report{InstallationID: installation.String(), PackageSHA256: digest, MigrationGeneration: generation}
}

// versionedPackage is the test package of plugin at version with the
// migration generation it runs on.
func versionedPackage(t *testing.T, plugin, version string, generation int64) (json.RawMessage, string) {
	t.Helper()
	raw, _ := pluginPackage(t, plugin)
	var manifest map[string]any
	require.NoError(t, json.Unmarshal(raw, &manifest))
	manifest["version"], manifest["migration_generation"] = version, generation
	encoded, err := json.Marshal(manifest)
	require.NoError(t, err)
	sum := sha256.Sum256(encoded)
	return encoded, hex.EncodeToString(sum[:])
}

// installed pushes the package, installs it against backend and records its
// service identities, stopping before activation.
func (s *session) installed(plugin, backend string, raw json.RawMessage, digest string) Installation {
	s.t.Helper()
	ctx := s.t.Context()
	pkg, err := s.manager.PushPackage(ctx, s.command(""), digest, raw)
	require.NoError(s.t, err)
	s.revision = pkg.Revision
	installation := s.installation(s.manager.CreateInstallation(ctx, s.command(""), digest, backend, "https://ui."+plugin+".example.test"))
	return s.installation(s.manager.SetIdentity(ctx, s.command(""), installation.InstallationID, userid.NewServiceUserTokenId().String(), userid.NewServiceUserTokenId().String()))
}

// requireUnchangedAndUnavailable checks that a refused activation changed
// nothing but the recorded observation, which explains the refusal.
func requireUnchangedAndUnavailable(t *testing.T, writer replica, ops *session, id uuid.UUID, plugin, diagnostic string) {
	t.Helper()
	current, err := writer.manager.Installation(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, ops.revision, current.Revision, "a refused activation changes no Plugin Manager state")
	require.Equal(t, model.PluginInstallationInstalled, current.State)
	require.Equal(t, plugins.StateUnavailable, current.Availability)
	require.NotNil(t, current.Diagnostic)
	require.Equal(t, diagnostic, current.Diagnostic.Code)
	require.NotEmpty(t, current.Diagnostic.Message)
	require.NotNil(t, current.Observation, "Core records what it read from the plugin")
	_, _, served := writer.registry.Installation(t.Context(), "any", plugin)
	require.False(t, served)
	for _, event := range auditEvents(t, writer.manager) {
		require.NotEqual(t, "plugin_installation.activate", event.Action, "a refused activation writes no audit event")
	}
}

func TestActivationServesOnlyThePackageTheRunningPluginProves(t *testing.T) {
	db := testDatabase(t)
	org := testOrganization(t, db)
	process := startPlugin(t)
	writer := newReplicaReading(t, db, process.Client())
	ops := newSession(t, writer.manager)
	const plugin = "dev.example.rollouts"
	raw, digest := versionedPackage(t, plugin, "1.0.0", 2)
	installation := ops.installed(plugin, process.URL, raw, digest)
	id := installation.InstallationID

	// The operator decides once; every refusal leaves that decision valid,
	// so the same command is retried until the plugin runtime caught up.
	activate := ops.command("activate-rollouts")
	for _, step := range []struct {
		configure  func()
		diagnostic string
	}{
		{func() {}, DiagnosticStatusInvalid},
		{func() { process.reports(uuid.New(), digest, 2) }, DiagnosticInstallationMismatch},
		{func() { process.reports(id, strings.Repeat("f", 64), 2) }, DiagnosticPackageMismatch},
		{func() { process.reports(id, digest, 1) }, DiagnosticMigrationMismatch},
	} {
		step.configure()
		_, err := writer.manager.Transition(t.Context(), activate, id, ActionActivate)
		requireCode(t, err, step.diagnostic)
		requireUnchangedAndUnavailable(t, writer, ops, id, plugin, step.diagnostic)
	}

	process.reports(id, digest, 2)
	active := ops.installation(writer.manager.Transition(t.Context(), activate, id, ActionActivate))
	require.Equal(t, model.PluginInstallationActive, active.State)
	require.Equal(t, plugins.StateAvailable, active.Availability)
	require.Nil(t, active.Diagnostic)
	require.Equal(t, id, *active.Observation.InstallationID)
	require.Equal(t, digest, active.Observation.PackageSHA256)
	require.EqualValues(t, 2, *active.Observation.MigrationGeneration)
	ops.installation(writer.manager.PutEnablement(t.Context(), ops.command(""), id, org, model.PluginEnablementEnabled, "", nil))
	item, descriptor, found := writer.registry.Installation(t.Context(), org, plugin)
	require.True(t, found)
	require.Equal(t, plugins.StateAvailable, descriptor.State)
	require.Equal(t, digest, item.ManifestSHA256)
	require.Len(t, item.Operations, 2)

	// The plugin runtime later restarts with another configuration: an
	// active installation stays served, activation is a completed step.
	process.reports(uuid.New(), digest, 2)
	again := ops.installation(writer.manager.Transition(t.Context(), ops.command(""), id, ActionActivate))
	require.Equal(t, active.Revision+1, again.Revision, "only the enablement changed the revision")
	activations := 0
	for _, event := range auditEvents(t, writer.manager) {
		if event.Action == "plugin_installation.activate" {
			activations++
		}
	}
	require.Equal(t, 1, activations)
}

func TestProbeFailureLeavesTheInstallationUnavailable(t *testing.T) {
	db := testDatabase(t)
	stopped := httptest.NewTLSServer(nil)
	backend, client := stopped.URL, stopped.Client()
	stopped.Close()
	writer := newReplicaReading(t, db, client)
	ops := newSession(t, writer.manager)
	const plugin = "dev.example.rollouts"
	raw, digest := versionedPackage(t, plugin, "1.0.0", 0)
	installation := ops.installed(plugin, backend, raw, digest)

	_, err := writer.manager.Transition(t.Context(), ops.command(""), installation.InstallationID, ActionActivate)
	requireCode(t, err, DiagnosticStatusUnreachable)
	requireUnchangedAndUnavailable(t, writer, ops, installation.InstallationID, plugin, DiagnosticStatusUnreachable)
	page, err := writer.manager.Installations(t.Context())
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Equal(t, plugins.StateUnavailable, page.Items[0].Availability, "lists derive the same availability")
	require.Equal(t, DiagnosticStatusUnreachable, page.Items[0].Diagnostic.Code)
}
