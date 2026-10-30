//go:build integration

package model

import (
	"testing"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"
)

func TestResourceTypeVersionMigrationPreservesContractsAndIdentity(t *testing.T) {
	dsn := isolatedSchema(t)
	requireStarts(t, dsn)
	db := schemaConnection(t, dsn)
	ctx := t.Context()
	require.NoError(t, goose.DownToContext(ctx, db, "migrations", 48))
	// Same slug in different scopes must not collapse into one contract.
	_, err := db.ExecContext(ctx, `INSERT INTO orgs (id, created_at, created_by, source, plan)
		VALUES ('contracts', now(), $1, 'internal', 'custom')`, uuid.New())
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO resource_types
		(org_id, id, description, output_schema, module_contract, created_at)
		VALUES (NULL, 'shared', 'Built-in', '{"type":"object"}', NULL, '2025-01-02'),
		('contracts', 'shared', 'Custom', '{"type":"object","properties":{"value":{"type":"string"}}}',
		'{"type":"object"}', '2025-02-03')`)
	require.NoError(t, err)
	require.NoError(t, goose.UpContext(ctx, db, "migrations"))
	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM resource_types r
		JOIN resource_type_versions v ON v.resource_type_uuid = r.uuid AND v.version = 1
		WHERE r.output_schema = v.output_schema AND r.module_contract IS NOT DISTINCT FROM v.module_contract
		AND r.created_at = v.created_at AND v.created_by IS NULL`).Scan(&count))
	require.Equal(t, 2, count)
	var before, after string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT string_agg(uuid::text, ',' ORDER BY uuid) FROM resource_type_versions`).Scan(&before))
	requireStarts(t, dsn)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT string_agg(uuid::text, ',' ORDER BY uuid) FROM resource_type_versions`).Scan(&after))
	require.Equal(t, before, after, "restart must not recreate contract identities")
	require.ErrorContains(t, goose.DownToContext(ctx, db, "migrations", 48), "cannot downgrade while Resource Type version snapshots exist")
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM resource_type_versions`).Scan(&count))
	require.Equal(t, 2, count, "refused downgrade is atomic")
}

func TestResourceTypeVersionsAreInitializedAndRetained(t *testing.T) {
	dsn := isolatedSchema(t)
	requireStarts(t, dsn)
	db := schemaConnection(t, dsn)
	ctx := t.Context()
	var resourceID uuid.UUID
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO resource_types
		(id, description, output_schema, created_at) VALUES ('example', 'Example', '{}', now()) RETURNING uuid`).Scan(&resourceID))
	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM resource_type_versions WHERE resource_type_uuid=$1 AND version=1`, resourceID).Scan(&count))
	require.Equal(t, 1, count)
	for _, statement := range []string{
		`UPDATE resource_type_versions SET output_schema='{"type":"object"}' WHERE resource_type_uuid=$1`,
		`DELETE FROM resource_type_versions WHERE resource_type_uuid=$1`,
		`INSERT INTO resource_type_versions (resource_type_uuid, version, output_schema, created_at) VALUES ($1, 1, '{}', now())`,
		`INSERT INTO resource_type_versions (resource_type_uuid, version, output_schema, created_at) VALUES ($1, 0, '{}', now())`,
	} {
		_, err := db.ExecContext(ctx, statement, resourceID)
		require.Error(t, err)
	}
	_, err := db.ExecContext(ctx, `DELETE FROM resource_types WHERE uuid=$1`, resourceID)
	require.NoError(t, err, "retain the existing unused catalogue deletion path")
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM resource_type_versions`).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO resource_types
		(id, description, output_schema, created_at) VALUES ('example', 'Example', '{}', now()) RETURNING uuid`).Scan(&resourceID))
	_, err = db.ExecContext(ctx, `INSERT INTO resource_type_versions (resource_type_uuid, version, output_schema, created_at) VALUES ($1, 2, '{"type":"object"}', now())`, resourceID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `DELETE FROM resource_types WHERE uuid=$1`, resourceID)
	require.ErrorContains(t, err, "Resource Type versions are immutable and retained")
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM resource_type_versions WHERE resource_type_uuid=$1`, resourceID).Scan(&count))
	require.Equal(t, 2, count, "refused parent deletion must retain both contracts")
}
