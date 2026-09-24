package integrationtests

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"
)

func TestResourceTypeArchiveSerializesWithNewModuleBinding(t *testing.T) {
	database := MustDatabaser(t)
	sqlDatabase := lifecycleSQLDatabase(t)
	orgID := MustCreateOrg(t, MustInternalServerClient(t)).Id
	resourceType, err := database.CreateResourceType(t.Context(), nil, &model.ResourceType{
		OrgId: opt.Of(orgID), Id: "rt-race-" + randToken(t), OutputsSchema: map[string]interface{}{},
	})
	require.NoError(t, err)
	createAppName := "rt-race-create-" + randToken(t)
	archiveAppName := "rt-race-archive-" + randToken(t)
	lockTx, err := database.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = lockTx.Rollback() }()
	_, err = lockTx.ExecContext(t.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "resource-type:"+resourceType.Id)
	require.NoError(t, err)

	type createOutcome struct {
		module *model.ModuleCatalogue
		err    error
	}
	createDone := make(chan createOutcome, 1)
	go func() {
		tx, err := database.BeginTx(t.Context(), nil)
		if err != nil {
			createDone <- createOutcome{err: err}
			return
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(t.Context(), `SELECT set_config('application_name', $1, true)`, createAppName); err != nil {
			createDone <- createOutcome{err: err}
			return
		}
		module, err := database.CreateEmptyModule(t.Context(), tx, orgID, "bound-before-archive", "", "", resourceType.Id, map[string]string{})
		if err == nil {
			err = tx.Commit()
		}
		createDone <- createOutcome{module: module, err: err}
	}()
	requireAdvisoryLockWait(t, sqlDatabase, createAppName)

	type archiveOutcome struct {
		resourceType *model.ResourceType
		err          error
	}
	archiveDone := make(chan archiveOutcome, 1)
	go func() {
		tx, err := database.BeginTx(t.Context(), nil)
		if err != nil {
			archiveDone <- archiveOutcome{err: err}
			return
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(t.Context(), `SELECT set_config('application_name', $1, true)`, archiveAppName); err != nil {
			archiveDone <- archiveOutcome{err: err}
			return
		}
		archived, err := database.SetResourceTypeCatalogueStatus(t.Context(), tx, orgID, resourceType.Id, "archived",
			userid.InternalSystemUuid, "Close new bindings", resourceType.ResourceVersion)
		if err == nil {
			err = tx.Commit()
		}
		archiveDone <- archiveOutcome{resourceType: archived, err: err}
	}()
	requireAdvisoryLockWait(t, sqlDatabase, archiveAppName)

	require.NoError(t, lockTx.Commit())
	create := <-createDone
	require.NoError(t, create.err)
	require.NotNil(t, create.module)
	archive := <-archiveDone
	require.NoError(t, archive.err)
	require.Equal(t, "archived", archive.resourceType.CatalogueStatus)

	tx, err := database.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = database.CreateEmptyModule(t.Context(), tx, orgID, "bound-after-archive", "", "", resourceType.Id, map[string]string{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "archived resource types reject new module bindings")
}

func requireAdvisoryLockWait(t *testing.T, database sqlQueryRower, applicationName string) {
	t.Helper()
	require.Eventually(t, func() bool {
		var waiting bool
		err := database.QueryRowContext(t.Context(), `SELECT EXISTS (
			SELECT 1
			FROM pg_stat_activity
			WHERE application_name = $1
			  AND wait_event_type = 'Lock'
			  AND query LIKE '%pg_advisory_xact_lock(hashtextextended%'
		)`, applicationName).Scan(&waiting)
		return err == nil && waiting
	}, 2*time.Second, 25*time.Millisecond, "transaction %s did not reach the Resource Type advisory lock wait queue", applicationName)
}

type sqlQueryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}
