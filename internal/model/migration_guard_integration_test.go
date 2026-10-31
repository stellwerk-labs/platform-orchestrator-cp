//go:build integration

package model

import (
	"fmt"
	"io/fs"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// exerciseSQLMigrationDown tests an individual historical guard against real
// data. A full-chain downgrade can stop at a newer guard before reaching it.
// The Goose journal stays untouched; successful round trips reinstall the same
// migration inside one transaction before committing.
func exerciseSQLMigrationDown(t *testing.T, db Databaser, version int, roundTrip bool) error {
	t.Helper()
	paths, err := fs.Glob(embedMigrations, fmt.Sprintf("migrations/%06d_*.sql", version))
	require.NoError(t, err)
	require.Len(t, paths, 1)
	source, err := embedMigrations.ReadFile(paths[0])
	require.NoError(t, err)
	up, down, found := strings.Cut(string(source), "-- +goose Down")
	require.True(t, found, "migration must contain a Down section")
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(t.Context(), down); err != nil {
		return err
	}
	if !roundTrip {
		return nil
	}
	if _, err = tx.ExecContext(t.Context(), up); err != nil {
		return err
	}
	return tx.Commit()
}
