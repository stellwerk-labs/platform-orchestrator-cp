//go:build integration

package model

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
	"github.com/stretchr/testify/require"
)

func TestObservationTargetsUseCanonicalFiltersAndScopedPagination(t *testing.T) {
	db := externalOperationDatabase(t)
	fixture := externalGrantFixture(t, db)
	ctx := t.Context()
	target := fixture.Intent.Environments[0]
	original, err := db.GetEnvironmentByUuid(ctx, nil, fixture.OrgID, target.EnvironmentID, GetModeDefault)
	require.NoError(t, err)
	prodType, err := db.CreateEnvironmentType(ctx, nil, &EnvType{OrgId: fixture.OrgID, OrgUuid: original.OrgUuid, Id: "delivery", DisplayName: "Production by canonical flag", IsProduction: true, CreatedAt: time.Now()})
	require.NoError(t, err)
	production := []uuid.UUID{}
	for _, name := range []string{"a", "b", "c"} {
		environment, err := db.CreateEnvironment(ctx, nil, &Environment{OrgId: fixture.OrgID, OrgUuid: original.OrgUuid, ProjectId: original.ProjectId, ProjectUuid: original.ProjectUuid, Id: name, EnvTypeId: prodType.Id, EnvTypeUuid: prodType.Uuid, CreatedAt: time.Now(), UpdatedAt: time.Now(), Status: EnvironmentStatusActive})
		require.NoError(t, err)
		production = append(production, environment.Uuid)
	}
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	query := func(selector ObservationTargetSelector, page string, limit int) ([]ObservationTargetMetadata, string) {
		t.Helper()
		items, next, err := QueryObservationTargetsTx(ctx, tx, fixture.OrgID, "dev.example.observer", selector, page, limit)
		require.NoError(t, err)
		return items, next
	}
	first, next := query(ObservationTargetSelector{}, "", 2)
	require.Len(t, first, 2)
	require.NotEmpty(t, next)
	second, last := query(ObservationTargetSelector{}, next, 2)
	require.Len(t, second, 2)
	require.Empty(t, last)
	seen := map[uuid.UUID]bool{}
	for _, item := range append(first, second...) {
		require.False(t, seen[item.EnvironmentUUID])
		seen[item.EnvironmentUUID] = true
		require.Equal(t, original.ProjectUuid, item.ProjectUUID)
	}
	items, _ := query(ObservationTargetSelector{Production: ref.Ref(true)}, "", 200)
	require.Len(t, items, 3)
	for _, item := range items {
		require.Equal(t, "delivery", item.EnvironmentType)
		require.True(t, item.Production)
	}
	items, _ = query(ObservationTargetSelector{EnvironmentTypes: ref.Ref([]string{"development"})}, "", 200)
	require.Len(t, items, 1)
	require.False(t, items[0].Production)
	items, _ = query(ObservationTargetSelector{EnvironmentIDs: ref.Ref([]uuid.UUID{})}, "", 200)
	require.Empty(t, items)
	items, _ = query(ObservationTargetSelector{EnvironmentIDs: &production, ExcludeEnvironmentIDs: production[:1]}, "", 200)
	require.Len(t, items, 2)
	items, _ = query(ObservationTargetSelector{ProjectIDs: ref.Ref([]string{"foreign-project"})}, "", 200)
	require.Empty(t, items)
	_, _, err = QueryObservationTargetsTx(ctx, tx, fixture.OrgID, "dev.example.other", ObservationTargetSelector{}, next, 2)
	require.ErrorContains(t, err, "exact target query")
	_, _, err = QueryObservationTargetsTx(ctx, tx, fixture.OrgID, "dev.example.observer", ObservationTargetSelector{Production: ref.Ref(true)}, next, 2)
	require.ErrorContains(t, err, "exact target query")
	items, _, err = QueryObservationTargetsTx(ctx, tx, "other-org", "dev.example.observer", ObservationTargetSelector{}, "", 200)
	require.NoError(t, err)
	require.Empty(t, items)
}

func TestObservationTargetsPageBeyondLegacyInventoryBound(t *testing.T) {
	db := externalOperationDatabase(t)
	fixture := externalGrantFixture(t, db)
	ctx := t.Context()
	environment, err := db.GetEnvironmentByUuid(ctx, nil, fixture.OrgID, fixture.Intent.Environments[0].EnvironmentID, GetModeDefault)
	require.NoError(t, err)
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `INSERT INTO envs(org_id,org_uuid,project_id,project_uuid,id,env_type_id,env_type_uuid,display_name,created_at,updated_at,status) SELECT $1,$2,$3,$4,'bulk-'||n::text,$5,$6,'Bulk '||n::text,now(),now(),'active' FROM generate_series(1,10001) n`, environment.OrgId, environment.OrgUuid, environment.ProjectId, environment.ProjectUuid, environment.EnvTypeId, environment.EnvTypeUuid)
	require.NoError(t, err)
	total := 0
	pages := 0
	page := ""
	for {
		items, next, err := QueryObservationTargetsTx(ctx, tx, fixture.OrgID, "dev.example.observer", ObservationTargetSelector{}, page, 200)
		require.NoError(t, err)
		require.LessOrEqual(t, len(items), 200)
		total += len(items)
		pages++
		if next == "" {
			break
		}
		page = next
	}
	require.Equal(t, 10002, total)
	require.Equal(t, 51, pages)
}
