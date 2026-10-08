//go:build integration

package model

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestSeveralOrganizationsPerInstallationBlockTheDowngrade(t *testing.T) {
	ctx := t.Context()
	dsn := isolatedSchema(t)
	core, err := NewDatabaser(ctx, zap.NewNop(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, core.Close()) })
	db := schemaConnection(t, dsn)
	actor := uuid.New()
	for _, org := range []string{"acme", "beta"} {
		_, err := core.CreateOrg(ctx, nil, &Org{Id: org, CreatedBy: actor, CreatedAt: time.Now().UTC(), Source: OrgSourceInternal, Plan: OrgPlanCustom})
		require.NoError(t, err)
	}
	installation, digest := uuid.New(), strings.Repeat("a", 64)
	_, err = db.ExecContext(ctx, `INSERT INTO plugin_packages (manifest_sha256, plugin_id, version, manifest, created_by) VALUES ($1, 'dev.example.rollouts', '1.0.0', '{}', $2)`, digest, actor)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO plugin_installations (installation_uuid, plugin_id, package_sha256, state, backend_url, created_by, updated_by)
		VALUES ($1, 'dev.example.rollouts', $2, 'active', 'https://rollouts.example.test', $3, $3)`, installation, digest, actor)
	require.NoError(t, err)
	enable := func(db *sql.DB, org string) error {
		_, err := db.ExecContext(ctx, `INSERT INTO plugin_enablements (plugin_id, org_id, installation_uuid, state, timezone, created_by, updated_by)
			VALUES ('dev.example.rollouts', $1, $2, 'enabled', 'UTC', $3, $3)`, org, installation, actor)
		return err
	}
	require.NoError(t, enable(db, "acme"))
	require.NoError(t, enable(db, "beta"), "one installation is enabled for several organizations")
	applied := appliedMigrations(t, db)

	// Later migrations step down first; the one that allows several
	// organizations refuses.
	require.NoError(t, goose.DownToContext(ctx, db, "migrations", 45))
	atSeveral := appliedMigrations(t, db)
	require.Equal(t, int64(45), atSeveral[len(atSeveral)-1])
	err = goose.DownToContext(ctx, db, "migrations", 44)
	require.ErrorContains(t, err, "cannot downgrade while an installation is enabled for several organizations")
	require.Equal(t, atSeveral, appliedMigrations(t, db), "the refused downgrade changes no migration")

	_, err = db.ExecContext(ctx, `DELETE FROM plugin_enablements WHERE org_id = 'beta'`)
	require.NoError(t, err)
	require.NoError(t, goose.DownToContext(ctx, db, "migrations", 44))
	require.ErrorContains(t, enable(db, "beta"), "plugin_enablements_installation_uuid_key", "below the migration an installation has one organization")
	require.NoError(t, goose.UpContext(ctx, db, "migrations"))
	require.Equal(t, applied, appliedMigrations(t, db))
	require.NoError(t, enable(db, "beta"))
}
