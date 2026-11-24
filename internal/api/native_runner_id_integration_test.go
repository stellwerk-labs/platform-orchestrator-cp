//go:build integration

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"
)

func TestNativeRunnerIDMatchesRuntimeBoundsWithoutChangingLegacy(t *testing.T) {
	db := pluginTestDatabase(t)
	org := "native-identifiers"
	_, err := db.CreateOrg(t.Context(), nil, &model.Org{Id: org, CreatedBy: userid.InternalSystemUuid, CreatedAt: time.Now(), Source: model.OrgSourceInternal, Plan: model.OrgPlanCustom})
	require.NoError(t, err)
	router := finalDeletionRouter(t, db)
	native := nativeAgentTestConfiguration(t)
	legacy := map[string]any{"type": "kubernetes-agent", "key": native["key"], "job": map[string]any{"namespace": "owned", "service_account": "owned"}}
	for _, test := range []struct {
		id     string
		config map[string]any
		status int
	}{
		{"np", native, http.StatusCreated},
		{strings.Repeat("n", 128), native, http.StatusCreated},
		{"native_pool", native, http.StatusCreated},
		{strings.Repeat("l", 129), legacy, http.StatusCreated},
		{strings.Repeat("n", 129), native, http.StatusBadRequest},
	} {
		body, err := json.Marshal(map[string]any{"id": test.id, "runner_configuration": test.config, "state_storage_configuration": map[string]any{"type": "s3", "bucket": "owned"}})
		require.NoError(t, err)
		request := httptest.NewRequest(http.MethodPost, "/orgs/"+org+"/runners", bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("From", userid.InternalSystemUuid.String())
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		require.Equal(t, test.status, response.Code, response.Body.String())
		stored, err := db.GetRunner(t.Context(), nil, org, test.id, model.GetModeDefault)
		if test.status == http.StatusBadRequest {
			_, missing := model.IsErrNotFound(err)
			require.True(t, missing, "overlong Native registration must persist no Runner")
			require.NotContains(t, response.Body.String(), test.id)
		} else {
			require.NoError(t, err)
			require.Equal(t, test.id, stored.Id)
		}
	}
}
