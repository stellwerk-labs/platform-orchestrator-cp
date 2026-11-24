//go:build integration

package model

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
	"github.com/stretchr/testify/require"
)

func TestObservationTargetSearchPrecedesPaginationAndBindsCursor(t *testing.T) {
	db := externalOperationDatabase(t)
	fixture := externalGrantFixture(t, db)
	environment, err := db.GetEnvironmentByUuid(t.Context(), nil, fixture.OrgID, fixture.Intent.Environments[0].EnvironmentID, GetModeDefault)
	require.NoError(t, err)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(t.Context(), `INSERT INTO envs(org_id,org_uuid,project_id,project_uuid,id,env_type_id,env_type_uuid,display_name,created_at,updated_at,status) SELECT $1,$2,$3,$4,'search-'||n::text,$5,$6,'Ordinary '||n::text,now(),now(),'active' FROM generate_series(1,40) n`, environment.OrgId, environment.OrgUuid, environment.ProjectId, environment.ProjectUuid, environment.EnvTypeId, environment.EnvTypeUuid)
	require.NoError(t, err)
	all, _, err := QueryObservationTargetsTx(t.Context(), tx, fixture.OrgID, "dev.example.observer", ObservationTargetSelector{}, "", 100)
	require.NoError(t, err)
	require.Len(t, all, 41)
	for _, item := range all[len(all)-3:] {
		_, err = tx.ExecContext(t.Context(), `UPDATE envs SET display_name='Needle 100%_\ literal' WHERE uuid=$1 AND org_id=$2`, item.EnvironmentUUID, fixture.OrgID)
		require.NoError(t, err)
	}
	selector := ObservationTargetSelector{Search: " needle 100%_\\ ", ProjectIDs: ref.Ref([]string{environment.ProjectId})}
	first, next, err := QueryObservationTargetsTx(t.Context(), tx, fixture.OrgID, "dev.example.observer", selector, "", 2)
	require.NoError(t, err)
	require.Len(t, first, 2)
	require.NotEmpty(t, next)
	require.Equal(t, all[38].EnvironmentUUID, first[0].EnvironmentUUID)
	selector.Search = strings.TrimSpace(selector.Search)
	second, last, err := QueryObservationTargetsTx(t.Context(), tx, fixture.OrgID, "dev.example.observer", selector, next, 2)
	require.NoError(t, err)
	require.Len(t, second, 1)
	require.Empty(t, last)
	require.Equal(t, all[40].EnvironmentUUID, second[0].EnvironmentUUID)
	selector.Search = "different"
	_, _, err = QueryObservationTargetsTx(t.Context(), tx, fixture.OrgID, "dev.example.observer", selector, next, 2)
	require.ErrorContains(t, err, "exact target query")
	selector.Search = "needle"
	foreign, _, err := QueryObservationTargetsTx(t.Context(), tx, "other-org", "dev.example.observer", selector, "", 2)
	require.NoError(t, err)
	require.Empty(t, foreign)
}

func TestObservationTargetSearchIsLiteralBoundedAndUsesProjectNames(t *testing.T) {
	db := externalOperationDatabase(t)
	fixture := externalGrantFixture(t, db)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(t.Context(), `UPDATE projects SET display_name='North Customer' WHERE org_id=$1`, fixture.OrgID)
	require.NoError(t, err)
	items, _, err := QueryObservationTargetsTx(t.Context(), tx, fixture.OrgID, "dev.example.observer", ObservationTargetSelector{Search: "NORTH CUSTOMER"}, "", 2)
	require.NoError(t, err)
	require.Len(t, items, 1)
	for _, search := range []string{"%", "_", "\\", "' OR true --"} {
		items, _, err := QueryObservationTargetsTx(t.Context(), tx, fixture.OrgID, "dev.example.observer", ObservationTargetSelector{Search: search}, "", 2)
		require.NoError(t, err)
		require.Empty(t, items, search)
	}
	for _, search := range []string{strings.Repeat("a", 201), strings.Repeat("é", 101), "bad\x00input", string([]byte{0xff})} {
		_, _, err := QueryObservationTargetsTx(t.Context(), tx, fixture.OrgID, "dev.example.observer", ObservationTargetSelector{Search: search}, "", 2)
		require.Error(t, err)
	}
	items, _, err = QueryObservationTargetsTx(t.Context(), tx, fixture.OrgID, "dev.example.observer", ObservationTargetSelector{Search: "North", EnvironmentIDs: ref.Ref([]uuid.UUID{})}, "", 2)
	require.NoError(t, err)
	require.Empty(t, items)
}
