//go:build integration

package model

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/moduleversions"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
	"github.com/stretchr/testify/require"
)

func TestResourceTypeContractBindingSurvivesNewVersions(t *testing.T) {
	db := externalOperationDatabaseWith(t)
	actor := uuid.New()
	org, targets, previous := externalOperationModules(t, db, actor)
	ctx := t.Context()
	core := db.(*databaser)
	var v1, v2 uuid.UUID
	require.NoError(t, core.QueryRowContext(ctx, `SELECT v.uuid FROM resource_type_versions v
		JOIN resource_types r ON r.uuid=v.resource_type_uuid WHERE r.org_id=$1 AND r.id='generic' AND v.version=1`, org).Scan(&v1))
	require.Equal(t, v1, previous[0].ResourceTypeVersionUUID)
	require.Equal(t, int64(1), previous[0].ResourceTypeVersion)
	versionTx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = versionTx.Rollback() }()
	published, err := db.PublishResourceTypeVersion(ctx, versionTx, org, "generic", ResourceTypeVersionPublication{Version: 2, ExpectedResourceVersion: 1, OutputSchema: map[string]any{"type": "object", "properties": map[string]any{"endpoint": map[string]any{"type": "string"}}}, ModuleContract: nil, Actor: actor, Reason: "Introduce endpoint contract"})
	require.NoError(t, err)
	v2 = published.UUID
	require.Equal(t, &actor, published.CreatedBy)
	require.NoError(t, versionTx.Commit())
	legacyProjection, err := db.GetResourceType(ctx, nil, &org, "generic")
	require.NoError(t, err)
	require.Equal(t, int64(2), legacyProjection.ResourceVersion)
	require.Contains(t, legacyProjection.OutputsSchema["properties"], "name", "old clients keep version 1")

	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	current, err := db.GetCoreModuleVersion(ctx, tx, org, "alpha", targets[0].VersionUUID.String(), GetModeDefault)
	require.NoError(t, err)
	_, err = db.TransitionCoreModuleVersion(ctx, tx, org, "alpha", current.UUID.String(), moduleversions.LifecycleDeprecated,
		current.ResourceVersion, actor, "Prepare next candidate", nil)
	require.NoError(t, err)
	request := func(version int64, output string) *ModuleDefinitionVersion {
		return &ModuleDefinitionVersion{OrgId: org, DefinitionId: "alpha", SemanticVersion: "1.2.0",
			ResourceTypeVersion: version, ModuleSource: "inline", ModuleSourceCode: opt.Of(`output "endpoint" { value = "test" }`),
			OutputSchema: map[string]any{"type": "object", "properties": map[string]any{output: map[string]any{"type": "string"}}}}
	}
	_, err = db.PublishCoreModuleVersion(ctx, tx, request(2, "name"), actor)
	require.ErrorContains(t, err, "module_output_contract_mismatch")
	_, err = db.PublishCoreModuleVersion(ctx, tx, request(0, "endpoint"), actor)
	require.ErrorContains(t, err, "module_output_contract_mismatch", "omission must not select the new version")
	candidate, err := db.PublishCoreModuleVersion(ctx, tx, request(2, "endpoint"), actor)
	require.NoError(t, err)
	require.Equal(t, v2, candidate.ResourceTypeVersionUUID)
	require.Equal(t, int64(2), candidate.ResourceTypeVersion)
	require.NoError(t, tx.Commit())
	definition, err := db.GetModuleDefinitionVersion(ctx, nil, org, "alpha", candidate.UUID.String())
	require.NoError(t, err)
	require.Equal(t, v2, definition.ResourceTypeVersionUUID)
	old, err := db.GetCoreModuleVersion(ctx, nil, org, "alpha", previous[0].UUID.String(), GetModeDefault)
	require.NoError(t, err)
	require.Equal(t, v1, old.ResourceTypeVersionUUID)
	_, err = core.ExecContext(ctx, `UPDATE definition_versions SET resource_type_version_uuid=$1 WHERE uuid=$2`, v1, candidate.UUID)
	require.ErrorContains(t, err, "contract is immutable")
	_, err = core.ExecContext(ctx, `UPDATE definitions SET resource_type_uuid=(SELECT uuid FROM resource_types WHERE org_id=$1 AND id='generic') WHERE org_id=$2 AND id='alpha'`, "foreign-org", org)
	require.Error(t, err, "a binding cannot be replaced or removed")
}
