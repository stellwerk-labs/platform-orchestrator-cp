package api

import (
	"crypto/sha256"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	dpapi "github.com/stellwerk-labs/platform-orchestrator-dp/shared/v3/genclient"
	"github.com/stretchr/testify/require"
)

func TestPluginDeploymentLineageLegacyEncoding(t *testing.T) {
	// Frozen pre-lineage field ordering/omission is part of signed body identity.
	const legacy = `{"env_id":"development","environment_uuid":"11111111-1111-4111-8111-111111111111","fence":3,"mode":"rollback","operation_ref":"22222222-2222-4222-8222-222222222222","project_id":"project","project_uuid":"33333333-3333-4333-8333-333333333333","rollback":{"forward_deployment_id":"44444444-4444-4444-8444-444444444444"}}`
	var admission dpapi.ExternalDeploymentAdmissionBody
	require.NoError(t, json.Unmarshal([]byte(legacy), &admission))
	encoded, err := json.Marshal(admission)
	require.NoError(t, err)
	require.Equal(t, sha256.Sum256([]byte(legacy)), sha256.Sum256(encoded), "legacy signed request digest must not change")
	const original = `{"environment_uuid":"11111111-1111-4111-8111-111111111111","grant_fence":3,"mode":"rollback","request_id":"original-dispatch","rollback":{"forward_deployment_id":"44444444-4444-4444-8444-444444444444"}}`
	var request PluginDeploymentSubmitRequest
	require.NoError(t, json.Unmarshal([]byte(original), &request))
	encoded, err = json.Marshal(request)
	require.NoError(t, err)
	require.Equal(t, sha256.Sum256([]byte(original)), sha256.Sum256(encoded), "legacy retained request digest must not change")
	require.NoError(t, validatePluginDeploymentSubmit(request))
	zero, other := uuid.Nil, uuid.New()
	request.FirstForwardDeploymentId = &zero
	require.Error(t, validatePluginDeploymentSubmit(request))
	request.FirstForwardDeploymentId = &other
	require.NoError(t, validatePluginDeploymentSubmit(request))
	request.Rollback.RetryOfRollbackDeploymentId = &zero
	require.Error(t, validatePluginDeploymentSubmit(request))
	request.Rollback.RetryOfRollbackDeploymentId = &request.Rollback.ForwardDeploymentId
	require.Error(t, validatePluginDeploymentSubmit(request))
	request.Rollback.RetryOfRollbackDeploymentId = &other
	require.NoError(t, validatePluginDeploymentSubmit(request))
}

func TestPluginRollbackAnchorValidation(t *testing.T) {
	first, base, other, zero := uuid.New(), uuid.New(), uuid.New(), uuid.Nil
	legacy := dpapi.ExternalDeploymentStatusResponse{}
	require.NoError(t, validatePluginRollbackAnchor(nil, legacy))
	require.Error(t, validatePluginRollbackAnchor(&first, legacy))
	anchored := dpapi.ExternalDeploymentStatusResponse{FirstForwardDeploymentId: &first, RollbackBaseDeploymentId: &base}
	require.NoError(t, validatePluginRollbackAnchor(nil, anchored))
	require.NoError(t, validatePluginRollbackAnchor(&first, anchored))
	require.Error(t, validatePluginRollbackAnchor(&other, anchored))
	for _, invalid := range []dpapi.ExternalDeploymentStatusResponse{
		{FirstForwardDeploymentId: &first},
		{RollbackBaseDeploymentId: &base},
		{FirstForwardDeploymentId: &zero, RollbackBaseDeploymentId: &base},
		{FirstForwardDeploymentId: &first, RollbackBaseDeploymentId: &zero},
	} {
		require.Error(t, validatePluginRollbackAnchor(nil, invalid))
	}
}
