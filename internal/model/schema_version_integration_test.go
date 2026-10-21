//go:build integration

package model

import (
	"database/sql"
	"fmt"
	"slices"
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestNewDatabaserRefusesASchemaNewerThanItsMigrations(t *testing.T) {
	dsn := isolatedSchema(t)
	db := schemaConnection(t, dsn)
	requireStarts(t, dsn)
	applied := appliedMigrations(t, db)
	require.NotEmpty(t, applied, "a fresh database migrates")
	require.Contains(t, schemaTables(t, db), "orgs")
	latest := applied[len(applied)-1]
	require.Greater(t, latest, int64(len(applied)), "version numbers have unused gaps, so the latest version exceeds the number of migrations")
	newer := fmt.Sprintf("database schema version %d is newer than this binary's latest migration %d", latest+1, latest)

	t.Run("a database this binary migrated starts again", func(t *testing.T) {
		requireStarts(t, dsn)
		require.Equal(t, applied, appliedMigrations(t, db))
	})

	t.Run("a database a newer release migrated is refused", func(t *testing.T) {
		dsn := isolatedSchema(t)
		db := schemaConnection(t, dsn)
		requireStarts(t, dsn)
		recordAppliedMigration(t, db, latest+1)

		_, err := NewDatabaser(t.Context(), zap.NewNop(), dsn)
		require.EqualError(t, err, newer)
		require.Equal(t, append(slices.Clone(applied), latest+1), appliedMigrations(t, db), "a refused start changes nothing")

		_, err = db.ExecContext(t.Context(), `DELETE FROM goose_db_version WHERE version_id = $1`, latest+1)
		require.NoError(t, err)
		requireStarts(t, dsn)
	})

	t.Run("the refusal comes before any migration", func(t *testing.T) {
		dsn := isolatedSchema(t)
		db := schemaConnection(t, dsn)
		createMigrationVersionTable(t, db)
		recordAppliedMigration(t, db, latest+1)

		_, err := NewDatabaser(t.Context(), zap.NewNop(), dsn)
		require.EqualError(t, err, newer)
		require.Equal(t, []string{"goose_db_version"}, schemaTables(t, db))
		require.Equal(t, []int64{latest + 1}, appliedMigrations(t, db))
	})

	t.Run("a version table without applied migrations migrates", func(t *testing.T) {
		dsn := isolatedSchema(t)
		db := schemaConnection(t, dsn)
		createMigrationVersionTable(t, db)
		requireStarts(t, dsn)
		require.Equal(t, applied, appliedMigrations(t, db))
	})
}

func requireStarts(t *testing.T, dsn string) {
	t.Helper()
	db, err := NewDatabaser(t.Context(), zap.NewNop(), dsn)
	require.NoError(t, err)
	require.NoError(t, db.Close())
}

// schemaConnection opens a connection whose search_path is the test's schema.
func schemaConnection(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

// appliedMigrations lists the versions in goose's table, without the version 0
// row goose writes when it creates the table.
func appliedMigrations(t *testing.T, db *sql.DB) []int64 {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT version_id FROM goose_db_version WHERE is_applied AND version_id > 0 ORDER BY version_id`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var versions []int64
	for rows.Next() {
		var version int64
		require.NoError(t, rows.Scan(&version))
		versions = append(versions, version)
	}
	require.NoError(t, rows.Err())
	return versions
}

func recordAppliedMigration(t *testing.T, db *sql.DB, version int64) {
	t.Helper()
	_, err := db.ExecContext(t.Context(), `INSERT INTO goose_db_version (version_id, is_applied) VALUES ($1, true)`, version)
	require.NoError(t, err)
}

// createMigrationVersionTable creates goose's version table as goose does
// before its first migration: with the version 0 row only.
func createMigrationVersionTable(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := goose.EnsureDBVersionContext(t.Context(), db)
	require.NoError(t, err)
}

func schemaTables(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema() ORDER BY table_name`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var tables []string
	for rows.Next() {
		var table string
		require.NoError(t, rows.Scan(&table))
		tables = append(tables, table)
	}
	require.NoError(t, rows.Err())
	return tables
}
