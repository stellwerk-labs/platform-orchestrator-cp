//go:build integration

package pluginmanager

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
)

// enablements returns each organization's enablement of the installation
// without its timestamps and capability registration.
func enablements(installation Installation) map[string]model.PluginEnablement {
	result := map[string]model.PluginEnablement{}
	for _, enablement := range installation.Organizations {
		result[enablement.OrgID] = model.PluginEnablement{OrgID: enablement.OrgID, State: enablement.State, Timezone: enablement.Timezone,
			ApprovalPolicies: enablement.ApprovalPolicies, CreatedBy: enablement.CreatedBy}
	}
	return result
}

// journaling reports whether Core journals org's resource events.
func journaling(t *testing.T, db model.Databaser, org string) bool {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	var journaling bool
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT journaling FROM extension_resource_event_journal_state WHERE org_id=$1`, org).Scan(&journaling))
	return journaling
}

func TestServingAllOrganizationsEnablesEachOrganizationOnce(t *testing.T) {
	db := testDatabase(t)
	ctx := t.Context()
	acme, beta, gamma, leaving := testOrganization(t, db), testOrganization(t, db), testOrganization(t, db), testOrganization(t, db)
	_, err := db.UpdateOrg(ctx, nil, leaving, model.UpdateOrgRequest{Status: opt.Of(model.OrgStatusDeleting)})
	require.NoError(t, err)
	writer := newReplica(t, db)
	ops := newSession(t, writer.manager)
	installation := ops.install("dev.example.rollouts", acme)
	id := installation.InstallationID
	require.Equal(t, model.PluginOrganizationsSelected, installation.OrganizationMode)
	require.Equal(t, model.PluginOrganizationDefaults{Timezone: "UTC", ApprovalPolicies: map[string]string{}}, installation.OrganizationDefaults)
	ops.installation(writer.manager.PutEnablement(ctx, ops.command(""), id, beta, model.PluginEnablementDisabled, "", nil))
	approval := map[string]string{"environment": "plugin.dev.example.rollouts.approve"}
	set := func(key, mode, zone string, policies map[string]string) (Installation, error) {
		return writer.manager.SetOrganizationMode(ctx, ops.command(key), id, mode, zone, policies)
	}

	for _, invalid := range []struct {
		name, code, mode, zone string
		policies               map[string]string
	}{
		{"mode", "plugin_manager_request_invalid", "every", "", nil},
		{"timezone", "plugin_enablement_invalid", model.PluginOrganizationsAll, "Local", nil},
		{"approval", "plugin_enablement_invalid", model.PluginOrganizationsAll, "", map[string]string{"organization": "plugin.dev.example.rollouts.approve"}},
	} {
		_, err := set("", invalid.mode, invalid.zone, invalid.policies)
		requireCode(t, err, invalid.code, invalid.name)
	}

	// Mode all enables every active organization without an enablement, in
	// one command, with the defaults. The opt-out of beta and acme's own
	// settings stay.
	all := ops.installation(set("serve-all", model.PluginOrganizationsAll, "America/New_York", approval))
	require.Equal(t, model.PluginOrganizationsAll, all.OrganizationMode)
	require.Equal(t, model.PluginOrganizationDefaults{Timezone: "America/New_York", ApprovalPolicies: approval}, all.OrganizationDefaults)
	require.Empty(t, all.PendingOrganizations)
	require.Equal(t, map[string]model.PluginEnablement{
		acme:  {OrgID: acme, State: model.PluginEnablementEnabled, Timezone: "Europe/Berlin", ApprovalPolicies: map[string]string{}, CreatedBy: operator},
		beta:  {OrgID: beta, State: model.PluginEnablementDisabled, Timezone: "UTC", ApprovalPolicies: map[string]string{}, CreatedBy: operator},
		gamma: {OrgID: gamma, State: model.PluginEnablementEnabled, Timezone: "America/New_York", ApprovalPolicies: approval, CreatedBy: operator},
	}, enablements(all), "an organization that is being deleted is not enabled")
	require.Equal(t, map[string]plugins.AvailabilityState{acme: plugins.StateAvailable, beta: plugins.StateNotEnabled, gamma: plugins.StateAvailable, leaving: plugins.StateNotInstalled},
		servedOrganizations(t, writer.registry, "dev.example.rollouts", acme, beta, gamma, leaving))
	require.True(t, journaling(t, db, gamma), "gamma's journal starts with its enablement")
	events := auditEvents(t, writer.manager)
	last := events[len(events)-1]
	require.Equal(t, "plugin_installation.organization_mode", last.Action)
	require.Equal(t, all.Revision, last.Revision, "the switch is one change")
	var before, after model.PluginInstallation
	require.NoError(t, json.Unmarshal(last.Before, &before))
	require.NoError(t, json.Unmarshal(last.After, &after))
	require.Equal(t, model.PluginOrganizationsSelected, before.OrganizationMode)
	require.Len(t, before.Organizations, 2)
	require.Equal(t, model.PluginOrganizationsAll, after.OrganizationMode)
	require.Len(t, after.Organizations, 3)

	// A replay returns the stored response; the same mode and defaults again
	// change nothing.
	replayed, err := writer.manager.SetOrganizationMode(ctx, Command{Actor: operator, IdempotencyKey: "serve-all", ExpectedRevision: all.Revision - 1, Reason: "Operate serve-all"},
		id, model.PluginOrganizationsAll, "America/New_York", approval)
	require.NoError(t, err)
	require.Equal(t, all, replayed)
	unchanged := ops.installation(set("", model.PluginOrganizationsAll, "America/New_York", approval))
	require.Equal(t, all.Revision, unchanged.Revision)

	// In mode all every organization keeps an enablement: removing one is
	// refused, removing an absent one still changes nothing.
	_, err = writer.manager.RemoveEnablement(ctx, ops.command(""), id, gamma)
	requireCode(t, err, "plugin_serves_all_organizations")
	require.Equal(t, all.Revision, ops.installation(writer.manager.RemoveEnablement(ctx, ops.command(""), id, leaving)).Revision)

	// New defaults, a timezone or approval policies, apply to enablements
	// Core creates from now on only.
	zone := ops.installation(set("", model.PluginOrganizationsAll, "Europe/Vienna", approval))
	require.Equal(t, all.Revision+1, zone.Revision)
	require.Equal(t, model.PluginOrganizationDefaults{Timezone: "Europe/Vienna", ApprovalPolicies: approval}, zone.OrganizationDefaults)
	vienna := ops.installation(set("", model.PluginOrganizationsAll, "Europe/Vienna", nil))
	require.Equal(t, all.Revision+2, vienna.Revision)
	require.Equal(t, enablements(all), enablements(vienna))
	require.Equal(t, model.PluginOrganizationDefaults{Timezone: "Europe/Vienna", ApprovalPolicies: map[string]string{}}, vienna.OrganizationDefaults)

	// Back to selected keeps every enablement as it is.
	selected := ops.installation(set("", model.PluginOrganizationsSelected, "Europe/Vienna", nil))
	require.Equal(t, model.PluginOrganizationsSelected, selected.OrganizationMode)
	require.Equal(t, enablements(all), enablements(selected))
	require.Empty(t, selected.PendingOrganizations)
	removed := ops.installation(writer.manager.RemoveEnablement(ctx, ops.command(""), id, gamma))
	require.NotContains(t, enablements(removed), gamma)

	// gamma has no enablement any more, which is no opt-out: mode all
	// enables it again. beta stays disabled.
	again := ops.installation(set("", model.PluginOrganizationsAll, "Europe/Vienna", nil))
	require.Equal(t, model.PluginEnablementEnabled, enablements(again)[gamma].State)
	require.Equal(t, "Europe/Vienna", enablements(again)[gamma].Timezone)
	require.Equal(t, model.PluginEnablementDisabled, enablements(again)[beta].State)

	// Uninstall removes every enablement and returns to selected; only an
	// installation Core serves can serve every organization.
	uninstalled := ops.installation(writer.manager.Transition(ctx, ops.command(""), id, ActionUninstall))
	require.Equal(t, model.PluginOrganizationsSelected, uninstalled.OrganizationMode)
	require.Empty(t, uninstalled.Organizations)
	_, err = set("", model.PluginOrganizationsAll, "", nil)
	requireCode(t, err, "plugin_installation_state")
	ops.installation(writer.manager.Transition(ctx, ops.command(""), id, ActionReinstall))
	_, err = set("", model.PluginOrganizationsAll, "", nil)
	requireCode(t, err, "plugin_installation_state", "an installed installation is not served yet")
	ops.installation(writer.manager.Transition(ctx, ops.command(""), id, ActionActivate))
	served := ops.installation(set("", model.PluginOrganizationsAll, "", nil))
	require.Len(t, served.Organizations, 3, "the opt-out went with the uninstall")

	// Core creates enablements with the defaults later, so they must hold
	// for every package the installation is to serve.
	ops.installation(set("", model.PluginOrganizationsSelected, "", map[string]string{"project": "plugin.dev.example.rollouts.approve"}))
	raw, _ := versionedPackage(t, "dev.example.rollouts", "1.1.0", 0)
	successor, digest := edited(t, raw, func(manifest map[string]any) {
		manifest["authorization_policies"] = []any{map[string]any{"permission": "plugin.dev.example.rollouts.approve", "scopes": []string{"environment"}}}
	})
	pkg, err := writer.manager.PushPackage(ctx, ops.command(""), digest, successor)
	require.NoError(t, err)
	ops.revision = pkg.Revision
	_, err = writer.manager.Upgrade(ctx, ops.command(""), id, digest)
	requireCode(t, err, "plugin_installation_incomplete", "the successor no longer approves at project scope, as the defaults do")
	ops.installation(set("", model.PluginOrganizationsSelected, "", nil))
	require.Equal(t, digest, ops.installation(writer.manager.Upgrade(ctx, ops.command(""), id, digest)).TargetPackageSHA256)
}

// waitingForALock reports whether a transaction waits for a lock.
func waitingForALock(t *testing.T, db model.Databaser) bool {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	var count int
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT count(*) FROM pg_locks WHERE NOT granted`).Scan(&count))
	return count > 0
}

func TestASwitchToAllWaitsForAnOrganizationBeingCreated(t *testing.T) {
	db := testDatabase(t)
	ctx := t.Context()
	writer := newReplica(t, db)
	ops := newSession(t, writer.manager)
	id := ops.install("dev.example.rollouts", testOrganization(t, db)).InstallationID

	// A creation transaction found no installation serving all
	// organizations and has not committed when an operator switches to all.
	creation, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = creation.Rollback() }()
	org, err := db.CreateOrg(ctx, creation, &model.Org{Id: "late-" + uuid.NewString()[:8], CreatedBy: operator, CreatedAt: time.Now(), Source: model.OrgSourceInternal, Plan: model.OrgPlanCustom})
	require.NoError(t, err)
	enabled, err := writer.manager.EnableNewOrganization(ctx, creation, *org)
	require.NoError(t, err)
	require.Empty(t, enabled)
	command := ops.command("")
	done := make(chan error, 1)
	var switched Installation
	go func() {
		var err error
		switched, err = writer.manager.SetOrganizationMode(ctx, command, id, model.PluginOrganizationsAll, "", nil)
		done <- err
	}()
	require.Eventually(t, func() bool { return waitingForALock(t, db) || len(done) > 0 }, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, creation.Commit())
	require.NoError(t, <-done)
	require.Contains(t, enablements(switched), org.Id, "the switch waited for the creation and enabled its organization")
	require.Empty(t, switched.PendingOrganizations)
}

func TestOrganizationsCreatedWhileSwitchingToAllAreEnabledExactlyOnce(t *testing.T) {
	db := testDatabase(t)
	ctx := t.Context()
	acme := testOrganization(t, db)
	writer := newReplica(t, db)
	ops := newSession(t, writer.manager)
	id := ops.install("dev.example.rollouts", acme).InstallationID

	// Two operators decide on the same revision: one switch commits, the
	// other is refused and changes nothing.
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, zone := range []string{"Europe/Paris", "Asia/Tokyo"} {
		command := ops.command("")
		go func() {
			<-start
			_, err := writer.manager.SetOrganizationMode(ctx, command, id, model.PluginOrganizationsAll, zone, nil)
			results <- err
		}()
	}
	close(start)
	var refused int
	for range 2 {
		if err := <-results; err != nil {
			requireCode(t, err, "plugin_manager_revision_conflict")
			refused++
		}
	}
	require.Equal(t, 1, refused)
	current, err := writer.manager.Installation(ctx, id)
	require.NoError(t, err)
	require.Equal(t, ops.revision+1, current.Revision)
	ops.revision = current.Revision
	ops.installation(writer.manager.SetOrganizationMode(ctx, ops.command(""), id, model.PluginOrganizationsSelected, "", nil))

	// Organizations created while an operator switches to mode all: the
	// Plugin Manager lock orders each creation before or after the switch,
	// so the switch or the creation enables it, never both and never none.
	const created = 16
	start = make(chan struct{})
	var wg sync.WaitGroup
	failures := make(chan error, created+1)
	for i := range created {
		wg.Go(func() {
			<-start
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				failures <- err
				return
			}
			defer func() { _ = tx.Rollback() }()
			org, err := db.CreateOrg(ctx, tx, &model.Org{Id: fmt.Sprintf("race-%02d-%s", i, uuid.NewString()[:8]), CreatedBy: operator, CreatedAt: time.Now(), Source: model.OrgSourceInternal, Plan: model.OrgPlanCustom})
			if err == nil {
				_, err = writer.manager.EnableNewOrganization(ctx, tx, *org)
			}
			if err == nil {
				err = tx.Commit()
			}
			if err != nil {
				failures <- err
			}
		})
	}
	command := ops.command("")
	wg.Go(func() {
		<-start
		if _, err := writer.manager.SetOrganizationMode(ctx, command, id, model.PluginOrganizationsAll, "Europe/Madrid", nil); err != nil {
			failures <- err
		}
	})
	close(start)
	wg.Wait()
	close(failures)
	for err := range failures {
		require.NoError(t, err)
	}

	installation, err := writer.manager.Installation(ctx, id)
	require.NoError(t, err)
	require.Empty(t, installation.PendingOrganizations)
	require.Len(t, installation.Organizations, created+1)
	byCore := 0
	for _, enablement := range installation.Organizations {
		require.Equal(t, model.PluginEnablementEnabled, enablement.State, enablement.OrgID)
		if enablement.OrgID == acme {
			continue
		}
		require.Equal(t, "Europe/Madrid", enablement.Timezone, enablement.OrgID)
		if enablement.CreatedBy == userid.InternalSystemUuid {
			byCore++
		}
	}
	events := auditEvents(t, writer.manager)
	require.Equal(t, installation.Revision, int64(len(events)), "one audit event per revision")
	automatic := 0
	for _, event := range events {
		if event.Actor == userid.InternalSystemUuid {
			require.Equal(t, "plugin_enablement.put", event.Action)
			require.NotEmpty(t, event.OrgID)
			automatic++
		}
	}
	require.Equal(t, byCore, automatic, "every enablement Core created on its own is one audited change")
	t.Logf("%d organizations enabled by their creation, %d by the switch", byCore, created-byCore)
}
