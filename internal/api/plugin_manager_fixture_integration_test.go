//go:build integration

package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/entitlements"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/pluginentitlements"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/pluginmanager"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
)

// pluginOperator holds plugin_manager_write in every test Plugin Manager.
var pluginOperator = userid.NewHumanUserId()

// testPluginManager is a Plugin Manager over db that serves registry.
// pluginOperator writes; readers only read.
func testPluginManager(t *testing.T, db model.Databaser, registry *plugins.InstallationRegistry, adapter *pluginentitlements.Adapter, readers ...uuid.UUID) *pluginmanager.Manager {
	t.Helper()
	if adapter == nil {
		var err error
		adapter, _, err = pluginentitlements.New(db, entitlements.Development, "", nil)
		require.NoError(t, err)
	}
	manager, err := pluginmanager.New(pluginmanager.Config{Database: db, Registry: registry, Entitlements: adapter, Readers: readers, Writers: []uuid.UUID{pluginOperator}})
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
	steps := newOperatorSteps(t, testPluginManager(t, db, &plugins.InstallationRegistry{}, nil))
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
	if item.ServiceUserID != "" {
		steps.installation(steps.manager.SetIdentity(ctx, steps.command("Bind the fixture identities"), id, item.ServiceUserID, item.AdmissionCheckerID))
	}
	steps.installation(steps.manager.Transition(ctx, steps.command("Activate the fixture plugin"), id, pluginmanager.ActionActivate))
	for org, state := range item.Organizations {
		enablement := model.PluginEnablementEnabled
		if state == plugins.StateNotEnabled {
			enablement = model.PluginEnablementDisabled
		}
		steps.installation(steps.manager.PutEnablement(ctx, steps.command("Enable the fixture organization"), id, org, enablement, item.OrganizationTimezones[org], item.DeploymentApprovalPolicies))
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
