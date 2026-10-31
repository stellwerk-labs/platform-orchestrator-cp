package integrationtests

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/pgtest"
	"github.com/stretchr/testify/require"
)

func migrationDatabaseAt(t *testing.T, version int64) *sql.DB {
	t.Helper()
	// NewDatabaser registers the embedded SQL and Go migrations used by the
	// running server. The fixture then applies only through the target version.
	MustDatabaser(t)
	connection := os.Getenv("DB_CONNECTION_STRING")
	require.NotEmpty(t, connection)
	admin, err := sql.Open("postgres", connection)
	require.NoError(t, err)
	schema := "migration_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = admin.ExecContext(t.Context(), `CREATE SCHEMA `+schema)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		require.NoError(t, err)
		require.NoError(t, admin.Close())
	})
	db, err := sql.Open("postgres", pgtest.WithSearchPath(connection, schema))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	require.NoError(t, goose.UpToContext(t.Context(), db, "migrations", version))
	return db
}

func migrationSections(t *testing.T, migration string) (string, string) {
	t.Helper()
	const upMarker = "-- +goose Up"
	const downMarker = "-- +goose Down"
	parts := strings.Split(migration, downMarker)
	require.Len(t, parts, 2)
	return strings.TrimSpace(strings.TrimPrefix(parts[0], upMarker)), strings.TrimSpace(parts[1])
}

type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func relationExists(t *testing.T, database queryRower, relation string) bool {
	t.Helper()
	var name sql.NullString
	require.NoError(t, database.QueryRowContext(
		t.Context(), `SELECT to_regclass(current_schema() || '.' || $1)::text`, relation,
	).Scan(&name))
	return name.Valid
}

func columnExists(t *testing.T, database queryRower, table, column string) bool {
	t.Helper()
	var count int
	require.NoError(t, database.QueryRowContext(t.Context(), `
		SELECT count(*)
		FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = $1 AND column_name = $2
	`, table, column).Scan(&count))
	return count == 1
}
