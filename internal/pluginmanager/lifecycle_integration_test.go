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
	"sync/atomic"
	"testing"
	"time"

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
	ops.installation(writer.manager.PutEnablement(t.Context(), ops.command(""), id, org, model.PluginEnablementEnabled, "", nil, installation.ServiceUserID))
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

// withOperation adds a GET operation to a package and returns its digest.
func withOperation(t *testing.T, raw json.RawMessage, id string) (json.RawMessage, string) {
	t.Helper()
	var manifest map[string]any
	require.NoError(t, json.Unmarshal(raw, &manifest))
	manifest["operations"] = append(manifest["operations"].([]any), map[string]any{"id": id, "method": "GET", "path": "/orgs/{orgId}/" + id})
	encoded, err := json.Marshal(manifest)
	require.NoError(t, err)
	sum := sha256.Sum256(encoded)
	return encoded, hex.EncodeToString(sum[:])
}

// served is what one replica serves for a plugin: package and operations.
type served struct {
	digest     string
	operations int
}

func requireEffect(t *testing.T, db model.Databaser, use model.PluginUse) error {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	return model.RequirePluginEnablement(t.Context(), tx, use)
}

func TestUpgradeSwitchesRegistrationsAtomicallyOnceTheSuccessorReports(t *testing.T) {
	db := testDatabase(t)
	org := testOrganization(t, db)
	process := startPlugin(t)
	writer := newReplicaReading(t, db, process.Client())
	reader := newReplica(t, db)
	ops := newSession(t, writer.manager)
	ctx := t.Context()
	const plugin = "dev.example.rollouts"
	v1, v1Digest := versionedPackage(t, plugin, "1.0.0", 1)
	v2, v2Digest := withOperation(t, mustVersion(t, plugin, "1.1.0", 2), "pause")
	installation := ops.installed(plugin, process.URL, v1, v1Digest)
	id := installation.InstallationID
	process.reports(id, v1Digest, 1)
	ops.installation(writer.manager.Transition(ctx, ops.command(""), id, ActionActivate))
	ops.installation(writer.manager.PutEnablement(ctx, ops.command(""), id, org, model.PluginEnablementEnabled, "", nil, installation.ServiceUserID))
	pkg, err := writer.manager.PushPackage(ctx, ops.command(""), v2Digest, v2)
	require.NoError(t, err)
	ops.revision = pkg.Revision

	// A reader replica polls throughout; every snapshot it serves is the
	// predecessor or the successor, never a mix.
	predecessor, successor := served{v1Digest, 2}, served{v2Digest, 3}
	readerCtx, stopReader := context.WithCancel(ctx)
	polled, sampled := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(polled)
		reader.manager.Run(readerCtx, 5*time.Millisecond)
	}()
	var samples []served
	var sawSuccessor atomic.Bool
	go func() {
		defer close(sampled)
		for readerCtx.Err() == nil {
			if item, _, found := reader.registry.Installation(readerCtx, org, plugin); found {
				sample := served{item.ManifestSHA256, len(item.Operations)}
				samples = append(samples, sample)
				sawSuccessor.Store(sawSuccessor.Load() || sample == successor)
			}
			time.Sleep(time.Millisecond)
		}
	}()
	servedBy := func(replica replica) served {
		item, descriptor, found := replica.registry.Installation(ctx, org, plugin)
		require.True(t, found)
		require.Equal(t, plugins.StateAvailable, descriptor.State)
		require.Equal(t, id.String(), item.InstallationID, "the installation UUID, and with it the licence binding, never changes")
		return served{item.ManifestSHA256, len(item.Operations)}
	}
	use := model.PluginUse{OrgID: org, PluginID: plugin, InstallationID: id.String(), PackageSHA256: v1Digest}

	upgrading := ops.installation(writer.manager.Upgrade(ctx, ops.command(""), id, v2Digest))
	require.Equal(t, v1Digest, upgrading.PackageSHA256)
	require.Equal(t, v2Digest, upgrading.TargetPackageSHA256)
	require.Equal(t, predecessor, servedBy(writer), "Core keeps serving the predecessor")
	require.NoError(t, requireEffect(t, db, use), "effects bound to the predecessor go on")

	// The plugin runtime rolls: old pods still answer, then new pods whose
	// migrations have not yet run.
	_, err = writer.manager.Transition(ctx, ops.command(""), id, ActionActivate)
	requireCode(t, err, DiagnosticPackageMismatch)
	process.reports(id, v2Digest, 1)
	_, err = writer.manager.Transition(ctx, ops.command(""), id, ActionActivate)
	requireCode(t, err, DiagnosticMigrationMismatch)
	pending, err := writer.manager.Installation(ctx, id)
	require.NoError(t, err)
	require.Equal(t, plugins.StateAvailable, pending.Availability, "a pending upgrade never degrades the served package")
	require.Equal(t, DiagnosticMigrationMismatch, pending.Diagnostic.Code)
	require.Equal(t, predecessor, servedBy(writer))
	require.Equal(t, ops.revision, pending.Revision, "refusals change nothing")

	process.reports(id, v2Digest, 2)
	switched := ops.installation(writer.manager.Transition(ctx, ops.command(""), id, ActionActivate))
	require.Equal(t, v2Digest, switched.PackageSHA256)
	require.Empty(t, switched.TargetPackageSHA256)
	require.Equal(t, model.PluginInstallationActive, switched.State)
	require.Nil(t, switched.Diagnostic)
	require.Equal(t, successor, servedBy(writer))
	require.Eventually(t, sawSuccessor.Load, 2*time.Second, 5*time.Millisecond, "the reader replica serves the successor within its refresh bound")
	stopReader()
	<-sampled
	<-polled
	require.Contains(t, samples, predecessor)
	require.Contains(t, samples, successor)
	for _, sample := range samples {
		require.Contains(t, []served{predecessor, successor}, sample, "a replica never serves a mix of both packages")
	}
	require.Equal(t, []string{v2Digest}, registeredPackages(t, db, id), "every registration belongs to the successor")
	requireCode(t, requireEffect(t, db, use), "plugin_not_installed", "an effect bound to the replaced package is refused")
	use.PackageSHA256 = v2Digest
	require.NoError(t, requireEffect(t, db, use))

	events := auditEvents(t, writer.manager)
	var before, after model.PluginInstallation
	last := events[len(events)-1]
	require.Equal(t, "plugin_installation.activate", last.Action)
	require.NoError(t, json.Unmarshal(last.Before, &before))
	require.NoError(t, json.Unmarshal(last.After, &after))
	require.Equal(t, v1Digest, before.PackageSHA256)
	require.Equal(t, v2Digest, before.TargetPackageSHA256)
	require.Equal(t, v2Digest, after.PackageSHA256)
	require.Equal(t, "plugin_installation.upgrade", events[len(events)-2].Action)

	regressing, regressingDigest := versionedPackage(t, plugin, "1.2.0", 1)
	pkg, err = writer.manager.PushPackage(ctx, ops.command(""), regressingDigest, regressing)
	require.NoError(t, err)
	ops.revision = pkg.Revision
	_, err = writer.manager.Upgrade(ctx, ops.command(""), id, regressingDigest)
	requireCode(t, err, "plugin_upgrade_invalid", "a later version never runs on an earlier schema generation")

	// A disabled installation is upgraded too and stays safety-only.
	v3, v3Digest := versionedPackage(t, plugin, "1.3.0", 2)
	pkg, err = writer.manager.PushPackage(ctx, ops.command(""), v3Digest, v3)
	require.NoError(t, err)
	ops.revision = pkg.Revision
	ops.installation(writer.manager.Transition(ctx, ops.command(""), id, ActionDisable))
	ops.installation(writer.manager.Upgrade(ctx, ops.command(""), id, v3Digest))
	process.reports(id, v3Digest, 2)
	disabled := ops.installation(writer.manager.Transition(ctx, ops.command(""), id, ActionActivate))
	require.Equal(t, model.PluginInstallationDisabled, disabled.State)
	require.Equal(t, v3Digest, disabled.PackageSHA256)
	require.Equal(t, plugins.StateNotEnabled, disabled.Availability)
}

// registeredPackages lists the packages the installation's registrations
// belong to.
func registeredPackages(t *testing.T, db model.Databaser, id uuid.UUID) []string {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(t.Context(), `SELECT DISTINCT package_sha256 FROM plugin_extension_registrations WHERE installation_uuid=$1 ORDER BY 1`, id)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	digests := []string{}
	for rows.Next() {
		var digest string
		require.NoError(t, rows.Scan(&digest))
		digests = append(digests, digest)
	}
	require.NoError(t, rows.Err())
	return digests
}

// mustVersion is the test package of plugin at version and generation.
func mustVersion(t *testing.T, plugin, version string, generation int64) json.RawMessage {
	t.Helper()
	raw, _ := versionedPackage(t, plugin, version, generation)
	return raw
}

func TestUpgradeAcceptsOnlyALaterPackageOfThePlugin(t *testing.T) {
	db := testDatabase(t)
	writer := newReplica(t, db)
	ops := newSession(t, writer.manager)
	ctx := t.Context()
	const plugin = "dev.example.rollouts"
	installation := ops.install(plugin, testOrganization(t, db))
	id := installation.InstallationID
	push := func(raw json.RawMessage, digest string) string {
		pkg, err := writer.manager.PushPackage(ctx, ops.command(""), digest, raw)
		require.NoError(t, err)
		ops.revision = pkg.Revision
		return digest
	}
	older := push(versionedPackage(t, plugin, "0.9.0", 0))
	prerelease := push(versionedPackage(t, plugin, "1.0.0-rc.1", 0))
	otherRaw, _ := versionedPackage(t, "dev.example.other", "2.0.0", 0)
	foreign := push(otherRaw, hexDigest(otherRaw))
	later := push(versionedPackage(t, plugin, "1.1.0", 0))
	require.NoError(t, writer.manager.Refresh(ctx))

	for name, digest := range map[string]string{"older": older, "prerelease of the served version": prerelease, "other plugin": foreign} {
		_, err := writer.manager.Upgrade(ctx, ops.command(""), id, digest)
		requireCode(t, err, "plugin_upgrade_invalid", name)
	}
	_, err := writer.manager.Upgrade(ctx, ops.command(""), id, strings.Repeat("0", 64))
	requireCode(t, err, "plugin_package_not_found")

	pending := ops.installation(writer.manager.Upgrade(ctx, ops.command(""), id, later))
	require.Equal(t, later, pending.TargetPackageSHA256)
	same := ops.installation(writer.manager.Upgrade(ctx, ops.command(""), id, later))
	require.Equal(t, pending.Revision, same.Revision, "requesting the pending target again changes nothing")
	withdrawn := ops.installation(writer.manager.Upgrade(ctx, ops.command(""), id, installation.PackageSHA256))
	require.Empty(t, withdrawn.TargetPackageSHA256, "requesting the served package withdraws the upgrade")
	require.Equal(t, installation.PackageSHA256, withdrawn.PackageSHA256)
	unchanged := ops.installation(writer.manager.Upgrade(ctx, ops.command(""), id, installation.PackageSHA256))
	require.Equal(t, withdrawn.Revision, unchanged.Revision)
}

func hexDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func TestUninstallKeepsTheInstallationForAReinstall(t *testing.T) {
	db := testDatabase(t)
	org := testOrganization(t, db)
	writer := newReplica(t, db)
	ops := newSession(t, writer.manager)
	ctx := t.Context()
	const plugin = "dev.example.rollouts"
	installation := ops.install(plugin, org)
	id := installation.InstallationID
	_, digest := pluginPackage(t, plugin)

	_, err := writer.manager.Transition(ctx, ops.command(""), id, ActionPurge)
	requireCode(t, err, "plugin_installation_state", "purge needs a prior uninstall")
	_, err = writer.manager.Transition(ctx, ops.command(""), id, ActionReinstall)
	requireCode(t, err, "plugin_installation_state", "only an uninstalled installation is reinstalled")

	uninstalled := ops.installation(writer.manager.Transition(ctx, ops.command(""), id, ActionUninstall))
	require.Equal(t, model.PluginInstallationUninstalled, uninstalled.State)
	require.Equal(t, plugins.StateNotInstalled, uninstalled.Availability)
	require.Empty(t, uninstalled.Organizations, "uninstall removes every enablement")
	require.Equal(t, installation.PackageSHA256, uninstalled.PackageSHA256)
	require.Equal(t, installation.ServiceUserID, uninstalled.ServiceUserID, "identities stay for the reinstall")
	require.Empty(t, registeredPackages(t, db, id), "uninstall removes every registration")
	require.Equal(t, plugins.StateNotInstalled, availability(writer.registry, org, plugin))
	requireCode(t, requireEffect(t, db, model.PluginUse{OrgID: org, PluginID: plugin, InstallationID: id.String(), PackageSHA256: digest, Safety: true}), "plugin_not_installed")

	for name, step := range map[string]func() error{
		"enable": func() error { _, err := writer.manager.Transition(ctx, ops.command(""), id, ActionEnable); return err },
		"activate": func() error {
			_, err := writer.manager.Transition(ctx, ops.command(""), id, ActionActivate)
			return err
		},
		"upgrade": func() error { _, err := writer.manager.Upgrade(ctx, ops.command(""), id, digest); return err },
		"organization": func() error {
			_, err := writer.manager.PutEnablement(ctx, ops.command(""), id, org, model.PluginEnablementEnabled, "", nil, installation.ServiceUserID)
			return err
		},
	} {
		requireCode(t, step(), "plugin_installation_state", name)
	}
	_, err = writer.manager.CreateInstallation(ctx, ops.command(""), digest, installation.BackendURL, installation.UIOrigin)
	requireCode(t, err, "plugin_installation_exists", "a retained installation is reinstalled, never shadowed by a new UUID")

	reinstalled := ops.installation(writer.manager.Transition(ctx, ops.command(""), id, ActionReinstall))
	require.Equal(t, id, reinstalled.InstallationID, "reinstall reuses the installation UUID")
	require.Equal(t, model.PluginInstallationInstalled, reinstalled.State)
	ops.installation(writer.manager.Transition(ctx, ops.command(""), id, ActionActivate))
	ops.installation(writer.manager.PutEnablement(ctx, ops.command(""), id, org, model.PluginEnablementEnabled, "", nil, installation.ServiceUserID))
	require.Equal(t, plugins.StateAvailable, availability(writer.registry, org, plugin))
	item, _, found := writer.registry.Installation(ctx, org, plugin)
	require.True(t, found)
	require.Equal(t, id.String(), item.InstallationID, "the licence binding of the UUID applies again")

	ops.installation(writer.manager.Transition(ctx, ops.command(""), id, ActionUninstall))
	purged := ops.installation(writer.manager.Transition(ctx, ops.command(""), id, ActionPurge))
	require.Equal(t, model.PluginInstallationPurged, purged.State)
	require.Equal(t, plugins.StateNotInstalled, purged.Availability)
	_, err = writer.manager.Transition(ctx, ops.command(""), id, ActionReinstall)
	requireCode(t, err, "plugin_installation_state", "a purged UUID is retired for good")
	_, err = writer.manager.Transition(ctx, ops.command(""), id, ActionUninstall)
	requireCode(t, err, "plugin_installation_state")
	fresh := ops.installation(writer.manager.CreateInstallation(ctx, ops.command(""), digest, installation.BackendURL, installation.UIOrigin))
	require.NotEqual(t, id, fresh.InstallationID, "after a purge the plugin is installed under a new UUID")

	actions := []string{}
	for _, event := range auditEvents(t, writer.manager) {
		if event.InstallationID != nil && *event.InstallationID == id {
			actions = append(actions, event.Action)
		}
	}
	require.Equal(t, []string{"plugin_installation.create", "plugin_installation.identity", "plugin_installation.activate", "plugin_enablement.put",
		"plugin_installation.uninstall", "plugin_installation.reinstall", "plugin_installation.activate", "plugin_enablement.put",
		"plugin_installation.uninstall", "plugin_installation.purge"}, actions, "every lifecycle change is audited exactly once")
}

func TestInterruptedLifecycleStepsResume(t *testing.T) {
	db := testDatabase(t)
	org := testOrganization(t, db)
	process := startPlugin(t)
	writer := newReplicaReading(t, db, process.Client())
	ops := newSession(t, writer.manager)
	ctx := t.Context()
	const plugin = "dev.example.rollouts"
	v1, v1Digest := versionedPackage(t, plugin, "1.0.0", 0)
	v2, v2Digest := versionedPackage(t, plugin, "2.0.0", 1)
	installation := ops.installed(plugin, process.URL, v1, v1Digest)
	id := installation.InstallationID
	process.reports(id, v1Digest, 0)
	ops.installation(writer.manager.Transition(ctx, ops.command(""), id, ActionActivate))
	ops.installation(writer.manager.PutEnablement(ctx, ops.command(""), id, org, model.PluginEnablementEnabled, "", nil, installation.ServiceUserID))
	pkg, err := writer.manager.PushPackage(ctx, ops.command(""), v2Digest, v2)
	require.NoError(t, err)
	ops.revision = pkg.Revision

	for _, step := range []struct {
		name string
		run  func(Command) (Installation, error)
		want func(Installation)
	}{
		{"upgrade", func(c Command) (Installation, error) { return writer.manager.Upgrade(ctx, c, id, v2Digest) },
			func(i Installation) { require.Equal(t, v2Digest, i.TargetPackageSHA256) }},
		{"switch", func(c Command) (Installation, error) {
			process.reports(id, v2Digest, 1)
			return writer.manager.Transition(ctx, c, id, ActionActivate)
		}, func(i Installation) { require.Equal(t, v2Digest, i.PackageSHA256) }},
		{"uninstall", func(c Command) (Installation, error) { return writer.manager.Transition(ctx, c, id, ActionUninstall) },
			func(i Installation) { require.Equal(t, model.PluginInstallationUninstalled, i.State) }},
		{"reinstall", func(c Command) (Installation, error) { return writer.manager.Transition(ctx, c, id, ActionReinstall) },
			func(i Installation) { require.Equal(t, model.PluginInstallationInstalled, i.State) }},
		{"activate", func(c Command) (Installation, error) { return writer.manager.Transition(ctx, c, id, ActionActivate) },
			func(i Installation) { require.Equal(t, model.PluginInstallationActive, i.State) }},
		{"uninstall again", func(c Command) (Installation, error) { return writer.manager.Transition(ctx, c, id, ActionUninstall) },
			func(i Installation) { require.Equal(t, model.PluginInstallationUninstalled, i.State) }},
		{"purge", func(c Command) (Installation, error) { return writer.manager.Transition(ctx, c, id, ActionPurge) },
			func(i Installation) { require.Equal(t, model.PluginInstallationPurged, i.State) }},
	} {
		// The step commits but its reply is lost: retrying the request
		// replays the stored result, and resuming from the recorded state
		// with a new request finds the step done.
		audited := len(auditEvents(t, writer.manager))
		inFlight := ops.command("")
		first, err := step.run(inFlight)
		require.NoError(t, err, step.name)
		step.want(first)
		replayed, err := step.run(inFlight)
		require.NoError(t, err, step.name)
		require.Equal(t, first.Revision, replayed.Revision, step.name)
		ops.revision = first.Revision
		resumed := ops.installation(step.run(ops.command("")))
		step.want(resumed)
		require.Equal(t, first.Revision, resumed.Revision, "%s: a completed step changes nothing", step.name)
		require.Len(t, auditEvents(t, writer.manager), audited+1, "%s changed state exactly once", step.name)
		require.Equal(t, id, resumed.InstallationID)
	}
}
