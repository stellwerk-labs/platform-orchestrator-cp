//go:build integration

package model

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"
)

func TestNativeRunnerRetentionProtectsDisabledRegistrationAndOrg(t *testing.T) {
	db := externalOperationDatabaseWith(t)
	ctx := t.Context()
	_, err := db.CreateOrg(ctx, nil, &Org{Id: "native-retention", CreatedBy: uuid.New(), CreatedAt: time.Now(), Source: OrgSourceInternal, Plan: OrgPlanCustom})
	require.NoError(t, err)
	sqlDB := db.(*databaser).DB
	_, err = sqlDB.ExecContext(ctx, `INSERT INTO runners (org_id,id,created_at,updated_at,type,configuration,state_type,state_configuration)
	 VALUES ('native-retention','pool',now(),now(),'native-agent','{"type":"native-agent","key":"retained-public-key","max_parallel_jobs":2,"native_execution":{}}','s3','{"type":"s3","bucket":"retained"}')`)
	require.NoError(t, err)
	var rule uuid.UUID
	require.NoError(t, sqlDB.QueryRowContext(ctx, `INSERT INTO runner_rules (org_id,runner_id,created_at) VALUES ('native-retention','pool',now()) RETURNING id`).Scan(&rule))
	original, err := db.GetRunner(ctx, nil, "native-retention", "pool", GetModeDefault)
	require.NoError(t, err)
	err = db.DeleteRunner(ctx, nil, "native-retention", "pool")
	_, conflict := IsErrConflict(err)
	require.True(t, conflict)
	require.ErrorContains(t, err, "retain execution authority")
	_, err = sqlDB.ExecContext(ctx, `DELETE FROM runners WHERE org_id='native-retention' AND id='pool'`)
	var retained *pq.Error
	require.ErrorAs(t, err, &retained)
	require.Equal(t, "23514", string(retained.Code))
	require.Equal(t, "runners_native_retention", retained.Constraint)
	delete(original.RunnerConfiguration, "native_execution")
	updated, err := db.UpdateRunner(ctx, nil, "native-retention", "pool", &RunnerPatch{UpdatedAt: time.Now(), RunnerConfiguration: &original.RunnerConfiguration})
	require.NoError(t, err)
	require.Equal(t, original.Uuid, updated.Uuid)
	err = db.DeleteRunner(ctx, nil, "native-retention", "pool")
	_, conflict = IsErrConflict(err)
	require.True(t, conflict, "disabled native rows still retain authority")
	_, err = sqlDB.ExecContext(ctx, `INSERT INTO runners SELECT org_id,id,created_at,updated_at,description,type,configuration,config_secret,state_type,state_configuration,uuid FROM runners WHERE org_id='native-retention' AND id='pool'`)
	require.Error(t, err, "retention prevents same-slug registration replacement")

	require.NoError(t, goose.DownToContext(ctx, sqlDB, "migrations", 63))
	versions := appliedMigrations(t, sqlDB)
	require.ErrorContains(t, goose.DownContext(ctx, sqlDB, "migrations"), "cannot discard native Runner registration retention")
	require.Equal(t, versions, appliedMigrations(t, sqlDB))
	_, err = sqlDB.ExecContext(ctx, `DELETE FROM runners WHERE org_id='native-retention' AND id='pool'`)
	require.ErrorAs(t, err, &retained)
	require.Equal(t, "runners_native_retention", retained.Constraint, "a refused downgrade preserves the active guard")

	_, err = sqlDB.ExecContext(ctx, `DELETE FROM orgs WHERE id='native-retention'`)
	var referenced *pq.Error
	require.ErrorAs(t, err, &referenced)
	require.Equal(t, "runners_org_id_fkey", referenced.Constraint)
	_, err = sqlDB.ExecContext(ctx, `INSERT INTO extension_resource_event_journal_state (org_id,journaling) VALUES ('native-retention',false)`)
	require.NoError(t, err)
	require.Error(t, db.DeleteOrg(ctx, nil, "native-retention"))
	var journal int
	require.NoError(t, sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM extension_resource_event_journal_state WHERE org_id='native-retention'`).Scan(&journal))
	require.Equal(t, 1, journal, "failed Org cleanup rolls back empty-journal removal")
	read, err := db.GetRunner(ctx, nil, "native-retention", "pool", GetModeDefault)
	require.NoError(t, err)
	require.Equal(t, updated, read)
	_, err = db.GetRunnerRule(ctx, nil, "native-retention", rule.String())
	require.NoError(t, err, "neither DELETE path cascades away the retained rule")
}

func TestNativeRunnerRetentionMigrationPreservesLegacyDeletion(t *testing.T) {
	db := externalOperationDatabaseWith(t)
	ctx := t.Context()
	sqlDB := db.(*databaser).DB
	_, err := db.CreateOrg(ctx, nil, &Org{Id: "legacy-retention", CreatedBy: uuid.New(), CreatedAt: time.Now(), Source: OrgSourceInternal, Plan: OrgPlanCustom})
	require.NoError(t, err)
	_, err = sqlDB.ExecContext(ctx, `INSERT INTO runners (org_id,id,created_at,updated_at,type,configuration,state_type,state_configuration)
	 VALUES ('legacy-retention','pool',now(),now(),'kubernetes-agent','{}','s3','{}')`)
	require.NoError(t, err)
	_, err = sqlDB.ExecContext(ctx, `INSERT INTO runner_rules (org_id,runner_id,created_at) VALUES ('legacy-retention','pool',now())`)
	require.NoError(t, err)
	before, err := db.GetRunner(ctx, nil, "legacy-retention", "pool", GetModeDefault)
	require.NoError(t, err)
	require.NoError(t, goose.DownToContext(ctx, sqlDB, "migrations", 62))
	require.NoError(t, goose.UpContext(ctx, sqlDB, "migrations"))
	after, err := db.GetRunner(ctx, nil, "legacy-retention", "pool", GetModeDefault)
	require.NoError(t, err)
	require.Equal(t, before, after, "legacy-only down/up changes no registration")
	require.NoError(t, db.DeleteRunner(ctx, nil, "legacy-retention", "pool"))
	var rules int
	require.NoError(t, sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM runner_rules WHERE org_id='legacy-retention'`).Scan(&rules))
	require.Zero(t, rules)
	err = db.DeleteRunner(ctx, nil, "legacy-retention", "pool")
	_, missing := IsErrNotFound(err)
	require.True(t, missing)
}
