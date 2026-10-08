//go:build integration

package pluginmanager

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/entitlements"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/pgtest"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/pluginentitlements"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v2/pluginstatus"
)

var operator = userid.NewHumanUserId()

// runningPlugins stands in for the plugin runtimes: each backend host serves
// the status handler of the plugin SDK with what its runtime was configured
// with. Nothing listens on any other host.
type runningPlugins struct {
	mu      sync.Mutex
	reports map[string]pluginstatus.Report
}

var fleet = &runningPlugins{reports: map[string]pluginstatus.Report{}}

func (p *runningPlugins) RoundTrip(request *http.Request) (*http.Response, error) {
	p.mu.Lock()
	report, running := p.reports[request.URL.Host]
	p.mu.Unlock()
	if !running {
		return nil, fmt.Errorf("dial tcp %s: connect: connection refused", request.URL.Host)
	}
	recorder := httptest.NewRecorder()
	pluginstatus.Handler(func(context.Context) (pluginstatus.Report, error) { return report, nil }).ServeHTTP(recorder, request)
	return recorder.Result(), nil
}

// serve makes the plugin behind backend report report.
func (p *runningPlugins) serve(t *testing.T, backend string, report pluginstatus.Report) {
	t.Helper()
	parsed, err := url.Parse(backend)
	require.NoError(t, err)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reports[parsed.Host] = report
}

// run starts the installation's plugin as a correctly configured runtime
// does: with the Core-assigned UUID, the recorded package and no plugin-owned
// schema.
func (p *runningPlugins) run(t *testing.T, installation Installation) {
	t.Helper()
	p.serve(t, installation.BackendURL, pluginstatus.Report{InstallationID: installation.InstallationID.String(), PackageSHA256: installation.PackageSHA256})
}

func testDatabase(t *testing.T) model.Databaser {
	t.Helper()
	dsn := os.Getenv("PLUGIN_TEST_DATABASE_URL")
	require.NotEmpty(t, dsn, "PLUGIN_TEST_DATABASE_URL must identify a disposable PostgreSQL instance")
	admin, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	schema := "plugin_manager_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = admin.Exec(`CREATE SCHEMA ` + schema)
	require.NoError(t, err)
	db, err := model.NewDatabaser(t.Context(), zap.NewNop(), pgtest.WithSearchPath(dsn, schema), model.WithExtensionResourceJournal())
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
		_, err := admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		require.NoError(t, err)
		require.NoError(t, admin.Close())
	})
	return db
}

func testOrganization(t *testing.T, db model.Databaser) string {
	t.Helper()
	org := "plugin-manager-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	_, err := db.CreateOrg(t.Context(), nil, &model.Org{Id: org, CreatedBy: operator, CreatedAt: time.Now(), Source: model.OrgSourceInternal, Plan: model.OrgPlanCustom})
	require.NoError(t, err)
	return org
}

// replica is one Control Plane's Plugin Manager and its read snapshot.
type replica struct {
	manager  *Manager
	registry *plugins.InstallationRegistry
}

func newReplica(t *testing.T, db model.Databaser) replica {
	t.Helper()
	return newReplicaReading(t, db, &http.Client{Transport: fleet})
}

// newReplicaReading is a replica that reads plugin status with client.
func newReplicaReading(t *testing.T, db model.Databaser, client *http.Client) replica {
	t.Helper()
	adapter, _, err := pluginentitlements.New(db, entitlements.Development, "", nil)
	require.NoError(t, err)
	registry := &plugins.InstallationRegistry{Entitlements: adapter}
	manager, err := New(Config{Database: db, Registry: registry, Entitlements: adapter, StatusClient: client})
	require.NoError(t, err)
	require.NoError(t, manager.Refresh(t.Context()))
	return replica{manager: manager, registry: registry}
}

// session issues commands at the revision the previous one returned, as a
// careful operator or the CLI does.
type session struct {
	t        *testing.T
	manager  *Manager
	revision int64
}

func newSession(t *testing.T, manager *Manager) *session {
	t.Helper()
	page, err := manager.Installations(t.Context())
	require.NoError(t, err)
	return &session{t: t, manager: manager, revision: page.Revision}
}

func (s *session) command(key string) Command {
	if key == "" {
		key = uuid.NewString()
	}
	return Command{Actor: operator, IdempotencyKey: key, ExpectedRevision: s.revision, Reason: "Operate " + key}
}

func (s *session) installation(installation Installation, err error) Installation {
	s.t.Helper()
	require.NoError(s.t, err)
	s.revision = installation.Revision
	return installation
}

// pluginPackage returns the test manifest renamed to plugin, with its digest.
func pluginPackage(t *testing.T, plugin string) (json.RawMessage, string) {
	t.Helper()
	raw := json.RawMessage(strings.ReplaceAll(testManifest, "dev.example.rollouts", plugin))
	raw = json.RawMessage(strings.ReplaceAll(string(raw), `"route_prefix": "/rollouts"`, `"route_prefix": "/`+strings.TrimPrefix(plugin, "dev.example.")+`"`))
	sum := sha256.Sum256(raw)
	return raw, hex.EncodeToString(sum[:])
}

// install runs every installation step for plugin and org.
func (s *session) install(plugin, org string) Installation {
	s.t.Helper()
	ctx := s.t.Context()
	raw, digest := pluginPackage(s.t, plugin)
	pkg, err := s.manager.PushPackage(ctx, s.command(""), digest, raw)
	require.NoError(s.t, err)
	s.revision = pkg.Revision
	installation := s.installation(s.manager.CreateInstallation(ctx, s.command(""), digest, "https://"+plugin+".example.test", "https://ui."+plugin+".example.test"))
	fleet.run(s.t, installation)
	identified := s.installation(s.manager.SetIdentity(ctx, s.command(""), installation.InstallationID, userid.NewServiceUserTokenId().String(), userid.NewServiceUserTokenId().String()))
	s.installation(s.manager.Transition(ctx, s.command(""), installation.InstallationID, ActionActivate))
	return s.installation(s.manager.PutEnablement(ctx, s.command(""), installation.InstallationID, org, model.PluginEnablementEnabled, "Europe/Berlin", nil, identified.ServiceUserID))
}

func availability(registry *plugins.InstallationRegistry, org, plugin string) plugins.AvailabilityState {
	descriptor, found := registry.Get(context.Background(), org, plugin)
	if !found {
		return plugins.StateNotInstalled
	}
	return descriptor.State
}

func TestReplicasServeInstallationChangesWithoutRestart(t *testing.T) {
	db := testDatabase(t)
	org := testOrganization(t, db)
	writer, other := newReplica(t, db), newReplica(t, db)
	const interval = 50 * time.Millisecond
	go other.manager.Run(t.Context(), interval)
	// The bound is one refresh interval plus a load; leave room for a slow CI.
	const bound = 2 * time.Second
	ops := newSession(t, writer.manager)
	served := func(state plugins.AvailabilityState) {
		t.Helper()
		require.Equal(t, state, availability(writer.registry, org, "dev.example.rollouts"), "the writing replica serves its change at once")
		started := time.Now()
		require.Eventually(t, func() bool { return availability(other.registry, org, "dev.example.rollouts") == state }, bound, 10*time.Millisecond)
		t.Logf("other replica served %s after %s", state, time.Since(started))
	}

	installation := ops.install("dev.example.rollouts", org)
	served(plugins.StateAvailable)
	item, descriptor, found := other.registry.Installation(t.Context(), org, "dev.example.rollouts")
	require.True(t, found)
	require.Equal(t, plugins.ModeFull, descriptor.Mode)
	require.Equal(t, installation.InstallationID.String(), item.InstallationID)
	require.Equal(t, "Europe/Berlin", item.OrganizationTimezones[org])
	require.Len(t, item.Operations, 2)

	ops.installation(writer.manager.PutEnablement(t.Context(), ops.command(""), installation.InstallationID, org, model.PluginEnablementDisabled, "Europe/Berlin", nil, installation.ServiceUserID))
	served(plugins.StateNotEnabled)
	ops.installation(writer.manager.PutEnablement(t.Context(), ops.command(""), installation.InstallationID, org, model.PluginEnablementEnabled, "Europe/Berlin", nil, installation.ServiceUserID))
	served(plugins.StateAvailable)
	ops.installation(writer.manager.Transition(t.Context(), ops.command(""), installation.InstallationID, ActionDisable))
	served(plugins.StateNotEnabled)
	ops.installation(writer.manager.Transition(t.Context(), ops.command(""), installation.InstallationID, ActionEnable))
	served(plugins.StateAvailable)
	ops.installation(writer.manager.RemoveEnablement(t.Context(), ops.command(""), installation.InstallationID, org))
	served(plugins.StateNotInstalled)
}

func TestEveryChangeWritesOneAuditEvent(t *testing.T) {
	db := testDatabase(t)
	org := testOrganization(t, db)
	writer := newReplica(t, db)
	ops := newSession(t, writer.manager)
	installation := ops.install("dev.example.rollouts", org)
	id := installation.InstallationID
	ops.installation(writer.manager.Transition(t.Context(), ops.command("disable"), id, ActionDisable))
	// A replay, and steps whose state already holds, change nothing.
	_, err := writer.manager.Transition(t.Context(), Command{Actor: ops.command("").Actor, IdempotencyKey: "disable", ExpectedRevision: ops.revision - 1, Reason: "Operate disable"}, id, ActionDisable)
	require.NoError(t, err)
	ops.installation(writer.manager.Transition(t.Context(), ops.command(""), id, ActionDisable))
	ops.installation(writer.manager.Transition(t.Context(), ops.command(""), id, ActionEnable))
	ops.installation(writer.manager.Transition(t.Context(), ops.command(""), id, ActionEnable))
	ops.installation(writer.manager.Transition(t.Context(), ops.command(""), id, ActionActivate))
	ops.installation(writer.manager.RemoveEnablement(t.Context(), ops.command(""), id, org))
	ops.installation(writer.manager.RemoveEnablement(t.Context(), ops.command(""), id, org))

	page, err := writer.manager.AuditEvents(t.Context(), 0, MaxAuditPage)
	require.NoError(t, err)
	actions := []string{}
	for i, event := range page.Items {
		actions = append(actions, event.Action)
		require.Equal(t, int64(i+1), event.Revision, "one event per revision")
		require.Equal(t, operator, event.Actor)
		require.NotEmpty(t, event.Reason)
		require.NotEmpty(t, event.After)
	}
	require.Equal(t, []string{"plugin_package.push", "plugin_installation.create", "plugin_installation.identity", "plugin_installation.activate",
		"plugin_enablement.put", "plugin_installation.disable", "plugin_installation.enable", "plugin_enablement.remove"}, actions)
	require.Equal(t, ops.revision, int64(len(page.Items)))
	require.Empty(t, page.Items[0].Before, "a new package has no previous state")
	require.Empty(t, page.Items[1].Before, "a new installation has no previous state")
	var before, after model.PluginInstallation
	require.NoError(t, json.Unmarshal(page.Items[5].Before, &before))
	require.NoError(t, json.Unmarshal(page.Items[5].After, &after))
	require.Equal(t, model.PluginInstallationActive, before.State)
	require.Equal(t, model.PluginInstallationDisabled, after.State)
	require.Equal(t, id, *page.Items[5].InstallationID)
	require.NoError(t, json.Unmarshal(page.Items[7].Before, &before))
	require.NoError(t, json.Unmarshal(page.Items[7].After, &after))
	require.Len(t, before.Organizations, 1)
	require.Empty(t, after.Organizations)
	require.Equal(t, org, page.Items[7].OrgID)

	paged, err := writer.manager.AuditEvents(t.Context(), page.Items[5].ID, 2)
	require.NoError(t, err)
	require.Equal(t, page.Items[6:8], paged.Items)
	for _, limit := range []int{0, MaxAuditPage + 1} {
		_, err := writer.manager.AuditEvents(t.Context(), 0, limit)
		require.Error(t, err)
	}
}

func TestReplayAndStaleRevisionsNeverDuplicateAnInstallation(t *testing.T) {
	db := testDatabase(t)
	writer := newReplica(t, db)
	ops := newSession(t, writer.manager)
	raw, digest := pluginPackage(t, "dev.example.rollouts")
	pkg, err := writer.manager.PushPackage(t.Context(), ops.command("push"), digest, raw)
	require.NoError(t, err)
	replayedPackage, err := writer.manager.PushPackage(t.Context(), ops.command("push"), digest, raw)
	require.NoError(t, err, "the replay carries the original expected revision")
	require.Equal(t, pkg, replayedPackage)
	ops.revision = pkg.Revision

	create := ops.command("create")
	first, err := writer.manager.CreateInstallation(t.Context(), create, digest, "https://rollouts.example.test", "https://ui.rollouts.example.test")
	require.NoError(t, err)
	replayed, err := writer.manager.CreateInstallation(t.Context(), create, digest, "https://rollouts.example.test", "https://ui.rollouts.example.test")
	require.NoError(t, err)
	require.Equal(t, first.InstallationID, replayed.InstallationID, "Core assigns the UUID once")
	_, err = writer.manager.CreateInstallation(t.Context(), create, digest, "https://other.example.test", "https://ui.rollouts.example.test")
	requireCode(t, err, "plugin_manager_idempotency_conflict")
	_, err = writer.manager.CreateInstallation(t.Context(), ops.command(""), digest, "https://rollouts.example.test", "https://ui.rollouts.example.test")
	requireCode(t, err, "plugin_manager_revision_conflict")
	ops.revision = first.Revision
	_, err = writer.manager.CreateInstallation(t.Context(), ops.command(""), digest, "https://rollouts.example.test", "https://ui.rollouts.example.test")
	requireCode(t, err, "plugin_installation_exists")

	// Concurrent operators deciding on the same revision serialise.
	second, secondDigest := pluginPackage(t, "dev.example.second")
	pkg, err = writer.manager.PushPackage(t.Context(), ops.command(""), secondDigest, second)
	require.NoError(t, err)
	ops.revision = pkg.Revision
	var wg sync.WaitGroup
	results := make(chan error, 6)
	for range 6 {
		wg.Go(func() {
			_, err := writer.manager.CreateInstallation(t.Context(), ops.command(""), secondDigest, "https://second.example.test", "https://ui.second.example.test")
			results <- err
		})
	}
	wg.Wait()
	close(results)
	created := 0
	for err := range results {
		if err == nil {
			created++
		} else {
			requireCode(t, err, "plugin_manager_revision_conflict")
		}
	}
	require.Equal(t, 1, created)
	page, err := writer.manager.Installations(t.Context())
	require.NoError(t, err)
	require.Len(t, page.Items, 2, "one installation per plugin")
}

func TestInterruptedInstallationResumesWithoutDuplicates(t *testing.T) {
	db := testDatabase(t)
	writer := newReplica(t, db)
	const steps = 5
	for interrupted := range steps + 1 {
		t.Run(fmt.Sprintf("after %d steps", interrupted), func(t *testing.T) {
			org := testOrganization(t, db)
			plugin := fmt.Sprintf("dev.example.resume%d", interrupted)
			raw, digest := pluginPackage(t, plugin)
			service, checker := userid.NewServiceUserTokenId().String(), userid.NewServiceUserTokenId().String()
			var id uuid.UUID
			// step runs installation step i as command and returns the
			// revision it reports.
			step := func(i int, command Command) (int64, error) {
				ctx := t.Context()
				var installation Installation
				var err error
				switch i {
				case 0:
					pkg, err := writer.manager.PushPackage(ctx, command, digest, raw)
					return pkg.Revision, err
				case 1:
					installation, err = writer.manager.CreateInstallation(ctx, command, digest, "https://"+plugin+".example.test", "https://ui."+plugin+".example.test")
					if err == nil {
						id = installation.InstallationID
						fleet.run(t, installation)
					}
				case 2:
					installation, err = writer.manager.SetIdentity(ctx, command, id, service, checker)
				case 3:
					installation, err = writer.manager.Transition(ctx, command, id, ActionActivate)
				case 4:
					installation, err = writer.manager.PutEnablement(ctx, command, id, org, model.PluginEnablementEnabled, "", nil, service)
				}
				return installation.Revision, err
			}
			current := func() int64 { return newSession(t, writer.manager).revision }
			fresh := func() Command {
				return Command{Actor: operator, IdempotencyKey: uuid.NewString(), ExpectedRevision: current(), Reason: "Install " + plugin}
			}
			auditBefore := len(auditEvents(t, writer.manager))

			// The interrupted run completes the first steps; the reply to the
			// last one is lost.
			var inFlight Command
			for i := range interrupted {
				inFlight = fresh()
				_, err := step(i, inFlight)
				require.NoError(t, err)
			}
			revision := current()
			if interrupted > 0 {
				// Retrying the in-flight request replays its stored result.
				replayed, err := step(interrupted-1, inFlight)
				require.NoError(t, err)
				require.Equal(t, revision, replayed)
				require.Equal(t, revision, current(), "a replay changes nothing")
			}
			// Resuming reruns every step at the current revision: completed
			// steps change nothing and the installation is never duplicated.
			for range 2 {
				for i := range steps {
					_, err := step(i, fresh())
					if i == 1 && id != uuid.Nil && err != nil {
						requireCode(t, err, "plugin_installation_exists")
						continue
					}
					require.NoError(t, err)
				}
			}

			page, err := writer.manager.Installations(t.Context())
			require.NoError(t, err)
			installed := 0
			for _, item := range page.Items {
				if item.PluginID != plugin {
					continue
				}
				installed++
				require.Equal(t, id, item.InstallationID)
				require.Equal(t, model.PluginInstallationActive, item.State)
				require.Len(t, item.Organizations, 1)
				require.Equal(t, org, item.Organizations[0].OrgID)
				require.Equal(t, service, item.ServiceUserID)
			}
			require.Equal(t, 1, installed, "reruns converge on one installation")
			require.Len(t, auditEvents(t, writer.manager), auditBefore+steps, "every step changed state exactly once")
			require.NoError(t, writer.manager.Refresh(t.Context()))
			require.Equal(t, plugins.StateAvailable, availability(writer.registry, org, plugin))
		})
	}
}

func auditEvents(t *testing.T, manager *Manager) []model.PluginManagerAuditEvent {
	t.Helper()
	events := []model.PluginManagerAuditEvent{}
	var after int64
	for {
		page, err := manager.AuditEvents(t.Context(), after, MaxAuditPage)
		require.NoError(t, err)
		if len(page.Items) == 0 {
			return events
		}
		events = append(events, page.Items...)
		after = page.Items[len(page.Items)-1].ID
	}
}

func TestCommandsRejectInvalidAndOutOfOrderSteps(t *testing.T) {
	db := testDatabase(t)
	org, second := testOrganization(t, db), testOrganization(t, db)
	writer := newReplica(t, db)
	ops := newSession(t, writer.manager)
	ctx := t.Context()
	raw, digest := pluginPackage(t, "dev.example.rollouts")
	for name, manifest := range map[string]string{
		"unknown field": strings.Replace(string(raw), `"version"`, `"surprise": true, "version"`, 1),
		"UI origin":     strings.Replace(string(raw), `"sdk_version"`, `"origin": "https://ui.example.test", "sdk_version"`, 1),
		"foreign grant": strings.Replace(string(raw), "plugin.dev.example.rollouts.read", "plugin.dev.example.other.read", 1),
		"two documents": string(raw) + "{}",
	} {
		_, err := writer.manager.PushPackage(ctx, ops.command(""), digest, json.RawMessage(manifest))
		requireCode(t, err, "plugin_manifest_invalid", name)
	}
	_, err := writer.manager.PushPackage(ctx, ops.command(""), strings.ToUpper(digest), raw)
	requireCode(t, err, "plugin_manifest_invalid")
	pkg, err := writer.manager.PushPackage(ctx, ops.command(""), digest, raw)
	require.NoError(t, err)
	ops.revision = pkg.Revision
	unchanged, err := writer.manager.PushPackage(ctx, ops.command(""), digest, raw)
	require.NoError(t, err)
	require.Equal(t, pkg.Revision, unchanged.Revision, "pushing the same package again changes nothing")
	changed := strings.Replace(string(raw), `"Rollouts"`, `"Changed"`, 1)
	_, err = writer.manager.PushPackage(ctx, ops.command(""), digest, json.RawMessage(changed))
	requireCode(t, err, "plugin_package_conflict", "a digest names exactly one manifest")
	sum := sha256.Sum256([]byte(changed))
	_, err = writer.manager.PushPackage(ctx, ops.command(""), hex.EncodeToString(sum[:]), json.RawMessage(changed))
	requireCode(t, err, "plugin_package_conflict", "a version names exactly one digest")

	_, err = writer.manager.CreateInstallation(ctx, ops.command(""), strings.Repeat("0", 64), "https://rollouts.example.test", "https://ui.example.test")
	requireCode(t, err, "plugin_package_not_found")
	_, err = writer.manager.CreateInstallation(ctx, ops.command(""), digest, "http://rollouts.example.test", "https://ui.example.test")
	requireCode(t, err, "plugin_installation_invalid", "HTTP needs explicit development mode")
	_, err = writer.manager.CreateInstallation(ctx, ops.command(""), digest, "https://rollouts.example.test", "")
	requireCode(t, err, "plugin_installation_invalid", "the package contributes a UI")
	installation := ops.installation(writer.manager.CreateInstallation(ctx, ops.command(""), digest, "https://rollouts.example.test", "https://ui.example.test"))
	id := installation.InstallationID
	fleet.run(t, installation)

	_, err = writer.manager.PutEnablement(ctx, ops.command(""), id, org, model.PluginEnablementEnabled, "", nil, "")
	requireCode(t, err, "plugin_installation_state", "enablement needs an active installation")
	_, err = writer.manager.Transition(ctx, ops.command(""), id, ActionEnable)
	requireCode(t, err, "plugin_installation_state")
	_, err = writer.manager.Transition(ctx, ops.command(""), id, ActionActivate)
	requireCode(t, err, "plugin_installation_incomplete", "service grants need service identities")
	_, err = writer.manager.SetIdentity(ctx, ops.command(""), id, userid.NewHumanUserId().String(), userid.NewServiceUserTokenId().String())
	requireCode(t, err, "plugin_installation_invalid", "a human account is never a plugin identity")
	service := userid.NewServiceUserTokenId().String()
	_, err = writer.manager.SetIdentity(ctx, ops.command(""), id, service, service)
	requireCode(t, err, "plugin_installation_invalid")
	ops.installation(writer.manager.SetIdentity(ctx, ops.command(""), id, service, userid.NewServiceUserTokenId().String()))
	ops.installation(writer.manager.Transition(ctx, ops.command(""), id, ActionActivate))
	_, err = writer.manager.SetIdentity(ctx, ops.command(""), id, userid.NewServiceUserTokenId().String(), userid.NewServiceUserTokenId().String())
	requireCode(t, err, "plugin_installation_state", "identities do not change after activation")

	for name, call := range map[string]func() error{
		"state": func() error {
			_, err := writer.manager.PutEnablement(ctx, ops.command(""), id, org, "available", "", nil, service)
			return err
		},
		"timezone": func() error {
			_, err := writer.manager.PutEnablement(ctx, ops.command(""), id, org, model.PluginEnablementEnabled, "Local", nil, service)
			return err
		},
		"approval": func() error {
			_, err := writer.manager.PutEnablement(ctx, ops.command(""), id, org, model.PluginEnablementEnabled, "", map[string]string{"organization": "plugin.dev.example.rollouts.approve"}, service)
			return err
		},
	} {
		requireCode(t, call(), "plugin_enablement_invalid", name)
	}
	_, err = writer.manager.PutEnablement(ctx, ops.command(""), id, "missing-org", model.PluginEnablementEnabled, "", nil, service)
	requireCode(t, err, "organization_not_found")
	for name, verified := range map[string]string{"nothing verified": "", "another service user verified": userid.NewServiceUserTokenId().String()} {
		_, err = writer.manager.PutEnablement(ctx, ops.command(""), id, org, model.PluginEnablementEnabled, "", nil, verified)
		requireCode(t, err, "plugin_installation_state", "a new enablement needs IAM's word on the recorded service user: %s", name)
	}
	ops.installation(writer.manager.PutEnablement(ctx, ops.command(""), id, org, model.PluginEnablementEnabled, "", map[string]string{"project": "plugin.dev.example.rollouts.approve"}, service))
	_, err = writer.manager.PutEnablement(ctx, ops.command(""), id, second, model.PluginEnablementEnabled, "", nil, service)
	requireCode(t, err, "plugin_enablement_limit", "v1alpha enables an installation for exactly one organization")
	_, err = writer.manager.Transition(ctx, ops.command(""), uuid.New(), ActionActivate)
	requireCode(t, err, "plugin_installation_not_found")

	// Another plugin cannot take a served UI route prefix.
	other := strings.ReplaceAll(string(raw), "dev.example.rollouts", "dev.example.other")
	otherSum := sha256.Sum256([]byte(other))
	pkg, err = writer.manager.PushPackage(ctx, ops.command(""), hex.EncodeToString(otherSum[:]), json.RawMessage(other))
	require.NoError(t, err)
	ops.revision = pkg.Revision
	otherInstallation := ops.installation(writer.manager.CreateInstallation(ctx, ops.command(""), hex.EncodeToString(otherSum[:]), "https://other.example.test", "https://ui.other.example.test"))
	fleet.run(t, otherInstallation)
	ops.installation(writer.manager.SetIdentity(ctx, ops.command(""), otherInstallation.InstallationID, userid.NewServiceUserTokenId().String(), userid.NewServiceUserTokenId().String()))
	_, err = writer.manager.Transition(ctx, ops.command(""), otherInstallation.InstallationID, ActionActivate)
	requireCode(t, err, "plugin_registration_conflict")
}

func TestAnInvalidServedInstallationDegradesOnlyItself(t *testing.T) {
	db := testDatabase(t)
	org, otherOrg := testOrganization(t, db), testOrganization(t, db)
	writer := newReplica(t, db)
	ops := newSession(t, writer.manager)
	broken := ops.install("dev.example.broken", org)
	ops.install("dev.example.healthy", otherOrg)
	// No command can store an invalid manifest; a database edit stands in
	// for state written by an older, less strict release.
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `UPDATE plugin_packages SET manifest=jsonb_set(manifest,'{version}','"not-a-version"') WHERE manifest_sha256=$1`, broken.PackageSHA256)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `UPDATE plugin_manager_state SET revision=revision+1`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.NoError(t, writer.manager.Refresh(t.Context()))
	require.Equal(t, plugins.StateNotInstalled, availability(writer.registry, org, "dev.example.broken"))
	require.Equal(t, plugins.StateAvailable, availability(writer.registry, otherOrg, "dev.example.healthy"))
}

func requireCode(t *testing.T, err error, code string, messages ...any) {
	t.Helper()
	require.Error(t, err, messages...)
	var actual string
	if bad, ok := model.IsErrBadRequest(err); ok {
		actual = bad.Code
	} else if conflict, ok := model.IsErrConflict(err); ok {
		actual = conflict.Code
	} else if missing, ok := model.IsErrNotFound(err); ok {
		actual = missing.Code
	}
	require.Equal(t, code, actual, append([]any{"%v"}, err))
}
