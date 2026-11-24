//go:build integration

package model

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/pressly/goose/v3"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestRunnerUUIDMigrationPreservesLegacyRowsAndWriters(t *testing.T) {
	dsn := isolatedSchema(t)
	requireStarts(t, dsn)
	db := schemaConnection(t, dsn)
	ctx := t.Context()
	require.NoError(t, goose.DownToContext(ctx, db, "migrations", 61))
	_, err := db.ExecContext(ctx, `INSERT INTO orgs (id, created_at, created_by, source, plan) VALUES ('runners', now(), $1, 'internal', 'custom')`, uuid.New())
	require.NoError(t, err)
	const legacyInsert = `INSERT INTO runners (org_id,id,created_at,updated_at,type,description,configuration,config_secret,state_type,state_configuration)
	 VALUES ('runners',$1,'2025-01-02','2025-01-03','kubernetes-agent','retained description','{"type":"kubernetes-agent","key":"retained-public-key","max_parallel_jobs":2}',
	 '{"path":"retained/secret","version":3}','s3','{"type":"s3","bucket":"retained-bucket"}')`
	for _, id := range []string{"alpha", "bravo", "charlie"} {
		_, err = db.ExecContext(ctx, legacyInsert, id)
		require.NoError(t, err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO runner_rules (org_id,runner_id,created_at) VALUES ('runners','alpha','2025-01-04')`)
	require.NoError(t, err)
	var before, after json.RawMessage
	require.NoError(t, db.QueryRowContext(ctx, `SELECT jsonb_agg(to_jsonb(r) ORDER BY id) FROM runners r`).Scan(&before))
	started := time.Now()
	require.NoError(t, goose.UpContext(ctx, db, "migrations"))
	t.Logf("populated Runner UUID migration: %s", time.Since(started))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT jsonb_agg(to_jsonb(r)-'uuid' ORDER BY id) FROM runners r`).Scan(&after))
	require.JSONEq(t, string(before), string(after))
	var count, distinct, rules int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(uuid),count(DISTINCT uuid) FROM runners`).Scan(&count, &distinct))
	require.Equal(t, 3, count)
	require.Equal(t, count, distinct)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM runner_rules WHERE runner_id='alpha'`).Scan(&rules))
	require.Equal(t, 1, rules)
	_, err = db.ExecContext(ctx, legacyInsert, "delta")
	require.NoError(t, err, "an old writer still receives a database-issued UUID")
	var identity uuid.UUID
	require.NoError(t, db.QueryRowContext(ctx, `SELECT uuid FROM runners WHERE id='alpha'`).Scan(&identity))
	require.NotEqual(t, uuid.Nil, identity)
	_, err = db.ExecContext(ctx, `UPDATE runners SET uuid=$1 WHERE id='bravo'`, identity)
	var duplicate *pq.Error
	require.ErrorAs(t, err, &duplicate)
	require.Equal(t, "runners_uuid_key", duplicate.Constraint)
	_, err = db.ExecContext(ctx, `UPDATE runners SET description='old writer update' WHERE org_id='runners' AND id='alpha'`)
	require.NoError(t, err)
	requireStarts(t, dsn)
	var reopened uuid.UUID
	require.NoError(t, db.QueryRowContext(ctx, `SELECT uuid FROM runners WHERE id='alpha'`).Scan(&reopened))
	require.Equal(t, identity, reopened, "old updates and startup preserve registration identity")
}

func TestRunnerUUIDCRUDUsesServerIdentityAndPreservesSlugPagination(t *testing.T) {
	dsn := isolatedSchema(t)
	db, err := NewDatabaser(t.Context(), zap.NewNop(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.CreateOrg(t.Context(), nil, &Org{Id: "runners", CreatedBy: uuid.New(), CreatedAt: time.Now(), Source: OrgSourceInternal, Plan: OrgPlanCustom})
	require.NoError(t, err)
	create := func(id string) *Runner {
		t.Helper()
		tx, err := db.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		forged := uuid.New()
		created, err := db.CreateRunner(t.Context(), tx, &Runner{
			OrgId: "runners", Id: id, Uuid: forged, RunnerType: "serverless-ecs",
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
			RunnerConfiguration:       map[string]any{"type": "serverless-ecs", "max_parallel_jobs": float64(2)},
			StateStorageType:          "s3",
			StateStorageConfiguration: map[string]any{"type": "s3", "bucket": "retained"},
			RunnerConfigurationSecret: &RunnerConfigurationSecret{Path: "retained/secret", Version: 3},
		})
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
		require.NotEqual(t, uuid.Nil, created.Uuid)
		require.NotEqual(t, forged, created.Uuid, "Create must return the actual database default, not a caller's UUID")
		return created
	}
	alpha, bravo := create("alpha"), create("bravo")
	require.NotEqual(t, alpha.Uuid, bravo.Uuid)
	first, token, err := db.ListRunners(t.Context(), nil, "runners", "", 1)
	require.NoError(t, err)
	require.Len(t, first, 1)
	require.Equal(t, "alpha", token)
	require.Equal(t, alpha.Uuid, first[0].Uuid)
	second, token, err := db.ListRunners(t.Context(), nil, "runners", token, 1)
	require.NoError(t, err)
	require.Len(t, second, 1)
	require.Empty(t, token)
	require.Equal(t, bravo.Uuid, second[0].Uuid)
	updated, err := db.UpdateRunner(t.Context(), nil, "runners", "alpha", &RunnerPatch{Description: opt.Of("updated"), UpdatedAt: time.Now()})
	require.NoError(t, err)
	require.Equal(t, alpha.Uuid, updated.Uuid)
	require.Equal(t, alpha.RunnerType, updated.RunnerType)
	require.Equal(t, alpha.RunnerConfiguration, updated.RunnerConfiguration)
	require.Equal(t, alpha.StateStorageConfiguration, updated.StateStorageConfiguration)
	require.Equal(t, alpha.RunnerConfigurationSecret, updated.RunnerConfigurationSecret)
	reopened, err := NewDatabaser(t.Context(), zap.NewNop(), dsn)
	require.NoError(t, err)
	defer func() { require.NoError(t, reopened.Close()) }()
	read, err := reopened.GetRunner(t.Context(), nil, "runners", "alpha", GetModeDefault)
	require.NoError(t, err)
	require.Equal(t, updated, read)
	require.NoError(t, db.DeleteRunner(t.Context(), nil, "runners", "alpha"))
	recreated := create("alpha")
	require.NotEqual(t, alpha.Uuid, recreated.Uuid)
}
