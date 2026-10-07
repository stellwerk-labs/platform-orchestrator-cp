//go:build integration

package model

import (
	"database/sql"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/moduleversions"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/pgtest"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func externalOperationDatabase(t *testing.T) Databaser {
	t.Helper()
	return externalOperationDatabaseWith(t, WithExtensionResourceJournal())
}

func externalOperationDatabaseWith(t *testing.T, options ...DatabaserOption) Databaser {
	t.Helper()
	dsn := os.Getenv("PLUGIN_TEST_DATABASE_URL")
	require.NotEmpty(t, dsn, "PLUGIN_TEST_DATABASE_URL must identify a disposable PostgreSQL instance")
	admin, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	schema := "plugin_cp_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = admin.Exec(`CREATE SCHEMA ` + schema)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		require.NoError(t, err)
		require.NoError(t, admin.Close())
	})
	db, err := NewDatabaser(t.Context(), zap.NewNop(), pgtest.WithSearchPath(dsn, schema), options...)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

func externalOperationModules(t *testing.T, db Databaser, actor uuid.UUID) (string, []ExternalModuleTarget, []CoreModuleVersion) {
	t.Helper()
	return externalOperationModulesWithTarget(t, db, actor, "1.1.0")
}

func externalOperationModulesWithTarget(t *testing.T, db Databaser, actor uuid.UUID, targetVersion string) (string, []ExternalModuleTarget, []CoreModuleVersion) {
	t.Helper()
	ctx := t.Context()
	org := "plugin-test-" + uuid.NewString()
	_, err := db.CreateOrg(ctx, nil, &Org{Id: org, CreatedBy: actor, CreatedAt: time.Now(), Source: OrgSourceInternal, Plan: OrgPlanCustom})
	require.NoError(t, err)
	journalFixtureOrganization(t, db, org)
	_, err = db.CreateResourceType(ctx, nil, &ResourceType{OrgId: opt.Of(org), Id: "generic", OutputsSchema: map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}}})
	require.NoError(t, err)
	targets := []ExternalModuleTarget{}
	previous := []CoreModuleVersion{}
	for _, slug := range []string{"alpha", "bravo"} {
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		module, err := db.CreateEmptyModule(ctx, tx, org, slug, slug, "", "generic", map[string]string{})
		require.NoError(t, err)
		publish := func(version string) *CoreModuleVersion {
			result, err := db.PublishCoreModuleVersion(ctx, tx, &ModuleDefinitionVersion{OrgId: org, DefinitionId: slug, SemanticVersion: version, ModuleSource: "inline", ModuleSourceCode: opt.Of(`output "name" { value = "example" }`), OutputSchema: map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}}, ModuleInputs: map[string]any{}, ModuleParams: map[string]ModuleParam{}, ProviderMapping: map[string]string{}, Dependencies: map[string]ModuleDefinitionDependency{}, CoProvisioned: []ModuleDefinitionCoProvision{}}, actor)
			require.NoError(t, err)
			return result
		}
		base := publish("1.0.0")
		base, err = db.TransitionCoreModuleVersion(ctx, tx, org, slug, base.UUID.String(), moduleversions.LifecycleDefault, base.ResourceVersion, actor, "Prepare baseline", nil)
		require.NoError(t, err)
		candidate := publish(targetVersion)
		targets = append(targets, ExternalModuleTarget{ModuleUUID: module.UUID, VersionUUID: candidate.UUID, ExpectedDefaultUUID: &base.UUID, ExpectedResourceVersion: candidate.ResourceVersion})
		previous = append(previous, *base)
		require.NoError(t, tx.Commit())
	}
	return org, targets, previous
}

func TestExternalModuleOperationAtomicityRecoveryAndFencing(t *testing.T) {
	db := externalOperationDatabase(t)
	actor := uuid.New()
	org, targets, previous := externalOperationModules(t, db, actor)
	command := ExternalModuleCommand{OrgID: org, PluginID: "dev.example.change-manager", OperationID: uuid.New(), ActorID: actor, RequestID: "prepare-1", Action: "reserve", Reason: "Freeze reviewed module set", Targets: targets}
	var group sync.WaitGroup
	results := make(chan *ExternalModuleReceipt, 2)
	failures := make(chan error, 2)
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			receipt, err := ApplyExternalModuleOperation(t.Context(), db, command)
			results <- receipt
			failures <- err
		}()
	}
	group.Wait()
	close(results)
	close(failures)
	for err := range failures {
		require.NoError(t, err)
	}
	for receipt := range results {
		require.Equal(t, int64(1), receipt.Fence)
		require.Equal(t, "active", receipt.State)
	}
	// A lost plugin acknowledgement is recovered by replay without a second Core mutation.
	replay, err := ApplyExternalModuleOperation(t.Context(), db, command)
	require.NoError(t, err)
	require.Equal(t, int64(1), replay.Fence)
	otherActor := command
	otherActor.ActorID = uuid.New()
	_, err = ApplyExternalModuleOperation(t.Context(), db, otherActor)
	require.Error(t, err)
	competitor := command
	competitor.OperationID = uuid.New()
	competitor.RequestID = "other"
	_, err = ApplyExternalModuleOperation(t.Context(), db, competitor)
	require.Error(t, err)
	for _, target := range targets {
		reservations, err := db.ListModuleOperationReservations(t.Context(), nil, org, target.ModuleUUID, false)
		require.NoError(t, err)
		require.Len(t, reservations, 1)
	}
	// Fail after one potential promotion: the entire set, outbox and receipt must roll back.
	promotion := command
	promotion.Action = "promote"
	promotion.RequestID = "promote-1"
	promotion.ExpectedFence = 1
	promotion.Targets = append([]ExternalModuleTarget{}, targets...)
	last := 1
	if strings.Compare(targets[0].ModuleUUID.String(), targets[1].ModuleUUID.String()) > 0 {
		last = 0
	}
	promotion.Targets[last].ExpectedResourceVersion++
	_, err = ApplyExternalModuleOperation(t.Context(), db, promotion)
	require.Error(t, err)
	for i, target := range targets {
		module, err := db.GetModuleCatalogue(t.Context(), nil, org, target.ModuleUUID.String(), GetModeDefault)
		require.NoError(t, err)
		require.Equal(t, previous[i].UUID, *module.CurrentDefaultVersionUUID)
	}
	promotion.Targets = targets
	receipt, err := ApplyExternalModuleOperation(t.Context(), db, promotion)
	require.NoError(t, err)
	require.Equal(t, "promote", receipt.Action)
	// Exact predecessor restoration is another atomic Core transaction.
	restore := promotion
	restore.RequestID = "restore-1"
	restore.Action = "restore"
	restore.Targets = append([]ExternalModuleTarget{}, targets...)
	for i, target := range targets {
		old, err := db.GetCoreModuleVersion(t.Context(), nil, org, target.ModuleUUID.String(), previous[i].UUID.String(), GetModeDefault)
		require.NoError(t, err)
		restore.Targets[i].VersionUUID = old.UUID
		restore.Targets[i].ExpectedResourceVersion = old.ResourceVersion
		restore.Targets[i].ExpectedDefaultUUID = &targets[i].VersionUUID
	}
	_, err = ApplyExternalModuleOperation(t.Context(), db, restore)
	require.NoError(t, err)
	release := command
	release.Action = "release"
	release.RequestID = "release-1"
	release.ExpectedFence = 1
	_, err = ApplyExternalModuleOperation(t.Context(), db, release)
	require.NoError(t, err)
	command.RequestID = "prepare-2"
	command.ExpectedFence = 1
	receipt, err = ApplyExternalModuleOperation(t.Context(), db, command)
	require.NoError(t, err)
	require.Equal(t, int64(2), receipt.Fence)
	release.RequestID = "stale-release"
	_, err = ApplyExternalModuleOperation(t.Context(), db, release)
	require.Error(t, err)
}
