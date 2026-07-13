package integrationtests

import (
	"context"
	"net/http"
	"testing"

	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/genclient"
)

func TestAuthorization(t *testing.T) {

	client := MustServerClient(t)

	orgId := MustCreateOrg(t, MustInternalServerClient(t)).Id

	t.Run("unknown human user has no access to org", func(t *testing.T) {
		r, err := client.ListProjectsWithResponse(t.Context(), orgId, &genclient.ListProjectsParams{}, func(ctx context.Context, req *http.Request) error {
			req.Header.Set("From", userid.NewHumanUserId().String())
			return nil
		})
		require.NoError(t, err)
		assert.Equal(t, http.StatusForbidden, r.StatusCode())
	})

	t.Run("unknown service user has no access to org", func(t *testing.T) {
		r, err := client.ListProjectsWithResponse(t.Context(), orgId, &genclient.ListProjectsParams{}, func(ctx context.Context, req *http.Request) error {
			req.Header.Set("From", userid.NewServiceUserTokenId().String())
			return nil
		})
		require.NoError(t, err)
		assert.Equal(t, http.StatusForbidden, r.StatusCode())
	})

	t.Run("system user does have access", func(t *testing.T) {
		r, err := client.ListProjectsWithResponse(t.Context(), orgId, &genclient.ListProjectsParams{}, func(ctx context.Context, req *http.Request) error {
			req.Header.Set("From", userid.InternalSystemUuid.String())
			return nil
		})
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, r.StatusCode())
	})

}
