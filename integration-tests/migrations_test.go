package integrationtests

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

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
		t.Context(), `SELECT to_regclass('public.' || $1)::text`, relation,
	).Scan(&name))
	return name.Valid
}

func columnExists(t *testing.T, database queryRower, table, column string) bool {
	t.Helper()
	var count int
	require.NoError(t, database.QueryRowContext(t.Context(), `
		SELECT count(*)
		FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2
	`, table, column).Scan(&count))
	return count == 1
}
