//go:build integration

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/hecho"
	orchestratoriam "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestModuleCatalogueSummaryHonorsVersionReadPermission(t *testing.T) {
	db := pluginTestDatabase(t)
	grant := seedPluginOperation(t, db, "catalogue-summary")
	actor := uuid.New()
	for _, status := range []int{http.StatusNoContent, http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			// Exercise the real authorization client and handler over HTTP, with the
			// catalogue loaded from PostgreSQL rather than a database substitute.
			iam := httptest.NewServer(catalogueSummaryAuthorization(t, actor, status))
			defer iam.Close()
			client, err := orchestratoriam.NewClientWithResponses(iam.URL)
			require.NoError(t, err)
			server := &Server{Database: db, IamClient: client, Logger: zap.NewNop()}
			ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, actor.String())
			result, err := server.ListModuleCatalogueEntries(ctx, ListModuleCatalogueEntriesRequestObject{OrgId: grant.OrgID})
			if status == http.StatusInternalServerError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			encoded, err := json.Marshal(result)
			require.NoError(t, err)
			var entries []map[string]any
			require.NoError(t, json.Unmarshal(encoded, &entries))
			require.NotEmpty(t, entries)
			for _, entry := range entries {
				verified, present := entry["default_verified"]
				require.Equal(t, status == http.StatusNoContent, present)
				if present {
					require.Equal(t, false, verified)
				}
				value, present := entry["has_proposed"]
				require.Equal(t, status == http.StatusNoContent, present)
				if present {
					require.IsType(t, false, value)
				}
			}
		})
	}
}

func catalogueSummaryAuthorization(t *testing.T, actor uuid.UUID, status int) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		var body orchestratoriam.InternalAuthorizeBody
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) || !assert.Len(t, body.Checks, 1) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.Equal(t, actor, body.UserId)
		if body.Checks[0].Permission == PermissionModuleCoreRead {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		assert.Equal(t, PermissionModuleVersionRead, body.Checks[0].Permission)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusForbidden {
			_, _ = w.Write([]byte(`{"details":{}}`))
		}
	}
}
