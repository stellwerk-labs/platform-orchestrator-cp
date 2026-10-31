package model

import (
	"bytes"
	"encoding/json"
	"github.com/google/uuid"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDraftTestAdmissionPreservesHistoricalIntentEncoding(t *testing.T) {
	// This is the recorded pre-Draft canonical intent shape, including collection
	// defaults. Omitted test selection must not alter existing receipt digests.
	const historical = `{"org_id":"example","admission_id":"11111111-1111-4111-8111-111111111111","project_uuid":"22222222-2222-4222-8222-222222222222","environment_uuid":"33333333-3333-4333-8333-333333333333","actor_id":"44444444-4444-4444-8444-444444444444","request_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","mode":"deploy","module_versions":{},"carry_forward_module_versions":{},"confirmed_restricted_version_uuids":[]}`
	var value DeploymentAdmission
	require.NoError(t, json.Unmarshal([]byte(historical), &value))
	encoded, err := json.Marshal(canonicalDeploymentAdmission(value))
	require.NoError(t, err)
	require.True(t, bytes.Equal([]byte(historical), encoded), "legacy canonical intent must retain byte-identical encoding")
	value.DraftTestSnapshots = map[string]uuid.UUID{}
	encoded, err = json.Marshal(canonicalDeploymentAdmission(value))
	require.NoError(t, err)
	require.True(t, bytes.Equal([]byte(historical), encoded), "legacy canonical intent must retain byte-identical encoding")
}
