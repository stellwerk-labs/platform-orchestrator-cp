//go:build integration

package model

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExternalMigrationsConstraintGuardIsRelationScoped(t *testing.T) {
	// Keep both migrated schemas alive. A same-named constraint on another
	// schema's table must not suppress installation of this table's constraint.
	first := externalOperationDatabase(t)
	second := externalOperationDatabase(t)
	for _, db := range []Databaser{first, second} {
		tx, err := db.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		var validated bool
		err = tx.QueryRowContext(t.Context(), `SELECT convalidated FROM pg_constraint WHERE conname='definition_versions_module_uuid_fk' AND conrelid='definition_versions'::regclass`).Scan(&validated)
		require.NoError(t, err)
		require.True(t, validated)
		require.NoError(t, tx.Rollback())
	}
}
