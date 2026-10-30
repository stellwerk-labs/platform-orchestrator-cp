//go:build integration

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/entitlements"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/pluginentitlements"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/pluginmanager"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/pluginstatus"
)

// pluginOperator holds both Plugin Manager permissions in every test IAM peer.
var pluginOperator = userid.NewHumanUserId()

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

// serve makes the plugin behind backend report what its runtime was given:
// the installation UUID and the package digest, with no plugin-owned schema.
func (p *runningPlugins) serve(t *testing.T, backend string, installation uuid.UUID, digest string) {
	t.Helper()
	parsed, err := url.Parse(backend)
	require.NoError(t, err)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reports[parsed.Host] = pluginstatus.Report{InstallationID: installation.String(), PackageSHA256: digest}
}

// testPluginManager is a Plugin Manager over db that serves registry and
// registers capabilities in iam.
func testPluginManager(t *testing.T, db model.Databaser, registry *plugins.InstallationRegistry, adapter *pluginentitlements.Adapter, iam *iamPeer) *pluginmanager.Manager {
	t.Helper()
	if adapter == nil {
		var err error
		adapter, _, err = pluginentitlements.New(db, entitlements.Development, "", nil)
		require.NoError(t, err)
	}
	manager, err := pluginmanager.New(pluginmanager.Config{Database: db, Registry: registry, Entitlements: adapter, StatusClient: &http.Client{Transport: fleet}, IAM: iam.client})
	require.NoError(t, err)
	return manager
}

// operatorSteps issues Plugin Manager commands as pluginOperator, each at the
// revision the previous one returned.
type operatorSteps struct {
	t        *testing.T
	manager  *pluginmanager.Manager
	revision int64
}

func newOperatorSteps(t *testing.T, manager *pluginmanager.Manager) *operatorSteps {
	t.Helper()
	page, err := manager.Installations(t.Context())
	require.NoError(t, err)
	return &operatorSteps{t: t, manager: manager, revision: page.Revision}
}

func (o *operatorSteps) command(reason string) pluginmanager.Command {
	return pluginmanager.Command{Actor: pluginOperator, IdempotencyKey: uuid.NewString(), ExpectedRevision: o.revision, Reason: reason}
}

func (o *operatorSteps) installation(installation pluginmanager.Installation, err error) pluginmanager.Installation {
	o.t.Helper()
	require.NoError(o.t, err)
	o.revision = installation.Revision
	return installation
}

// installPlugin pushes item's package, installs it, records its service
// identities, activates it and enables it for each of its organizations
// through the Plugin Manager. An organization whose fixture state is
// not_enabled gets a disabled enablement. It returns item with the
// Core-assigned installation UUID and the package digest.
func installPlugin(t *testing.T, db model.Databaser, item plugins.Installation) plugins.Installation {
	t.Helper()
	ctx := t.Context()
	steps := newOperatorSteps(t, testPluginManager(t, db, &plugins.InstallationRegistry{}, nil, operatorIAM(t)))
	raw, err := json.Marshal(item.Manifest())
	require.NoError(t, err)
	digest := item.ManifestSHA256
	if digest == "" {
		sum := sha256.Sum256(raw)
		digest = hex.EncodeToString(sum[:])
	}
	pkg, err := steps.manager.PushPackage(ctx, steps.command("Register the fixture package"), digest, raw)
	require.NoError(t, err)
	steps.revision = pkg.Revision
	uiOrigin := ""
	if item.UI != nil {
		uiOrigin = item.UI.Origin
	}
	installation := steps.installation(steps.manager.CreateInstallation(ctx, steps.command("Install the fixture plugin"), digest, item.Backend, uiOrigin))
	id := installation.InstallationID
	fleet.serve(t, item.Backend, id, digest)
	if item.ServiceUserID != "" {
		steps.installation(steps.manager.SetIdentity(ctx, steps.command("Bind the fixture identities"), id, item.ServiceUserID, item.AdmissionCheckerID))
	}
	steps.installation(steps.manager.Transition(ctx, steps.command("Activate the fixture plugin"), id, pluginmanager.ActionActivate))
	for org, state := range item.Organizations {
		enablement := model.PluginEnablementEnabled
		if state == plugins.StateNotEnabled {
			enablement = model.PluginEnablementDisabled
		}
		steps.installation(steps.manager.PutEnablement(ctx, steps.command("Enable the fixture organization"), id, org, enablement, item.OrganizationTimezones[org], item.OrganizationApprovalPolicies[org]))
	}
	item.InstallationID, item.ManifestSHA256 = id.String(), digest
	return item
}

// installedRegistry installs item and serves it, with its fixture states, from
// a registry snapshot.
func installedRegistry(t *testing.T, db model.Databaser, item plugins.Installation) (*plugins.InstallationRegistry, plugins.Installation) {
	t.Helper()
	item = installPlugin(t, db, item)
	registry, err := plugins.NewInstallationRegistry([]plugins.Installation{item}, false)
	require.NoError(t, err)
	return registry, item
}
