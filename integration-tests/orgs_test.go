package integrationtests

import (
	"crypto/rand"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	orchestratoriam "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"

	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/genclient"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
)

func TestInternalOrgs(t *testing.T) {
	t.Parallel()

	client := MustInternalServerClient(t)

	id := MustCreateOrg(t, MustInternalServerClient(t)).Id

	t.Run("org does not exist before creation", func(t *testing.T) {
		res, err := client.GetInternalOrganizationWithResponse(t.Context(), "unknown-org")
		require.NoError(t, err)
		assert.Equal(t, http.StatusNotFound, res.StatusCode())
	})

	t.Run("create org", func(t *testing.T) {
		t.Log("creating org", id)
		org := MustCreateOrg(t, MustInternalServerClient(t))
		assert.Equal(t, userid.InternalSystemUuid, org.CreatedBy)
		assert.Equal(t, "custom", org.Plan)

		t.Run("creating another org gives different uuid", func(t *testing.T) {
			res2, err := client.CreateInternalOrganizationWithResponse(t.Context(), genclient.CreateInternalOrganizationJSONRequestBody{Id: ref.Ref("org-" + strings.ToLower(rand.Text()))})
			if assert.NoError(t, err) && assert.Equal(t, http.StatusCreated, res2.StatusCode(), string(res2.Body)) {
				assert.NotEqual(t, org.Id, res2.JSON201.Id)
				assert.NotEqual(t, org.Uuid, res2.JSON201.Uuid)
				assert.GreaterOrEqual(t, res2.JSON201.CreatedAt, org.CreatedAt)
			}
		})

		t.Run("can get org", func(t *testing.T) {
			r, err := client.GetOrganizationWithResponse(t.Context(), org.Id)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, r.StatusCode(), string(r.Body))
		})
	})

	t.Run("get a conflict if you create the org again", func(t *testing.T) {
		res, err := client.CreateInternalOrganizationWithResponse(t.Context(), genclient.CreateInternalOrganizationJSONRequestBody{Id: ref.Ref(id)})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusConflict, res.StatusCode(), string(res.Body)) {
			assert.Contains(t, res.JSON409.Message, "organization already exists")
		}
	})

	t.Run("paginates correctly and contains the created org", func(t *testing.T) {
		for range 10 {
			res, err := client.CreateInternalOrganizationWithResponse(t.Context(), genclient.CreateInternalOrganizationJSONRequestBody{IdPrefix: ref.Ref("org")})
			require.NoError(t, err)
			require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
		}
		first, nextToken, seen := true, "", make([]string, 0)
		for first || nextToken != "" {
			first = false
			t.Log("fetching page with token", nextToken)
			res, err := client.ListInternalOrganizationsWithResponse(t.Context(), &genclient.ListInternalOrganizationsParams{PerPage: ref.Ref(5), Page: ref.RefStringEmptyNil(nextToken)})
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body))
			assert.LessOrEqual(t, len(res.JSON200.Items), 5)
			for _, item := range res.JSON200.Items {
				assert.NotContains(t, seen, item.Id)
				seen = append(seen, item.Id)
			}
			nextToken = ref.DerefOr(res.JSON200.NextPageToken, "")
		}
		assert.Contains(t, seen, id)
	})

	t.Run("cannot call internal orgs api on public endpoint", func(t *testing.T) {
		c := MustServerClient(t)
		r, err := c.ListInternalOrganizationsWithResponse(t.Context(), &genclient.ListInternalOrganizationsParams{})
		require.NoError(t, err)
		assert.Equal(t, http.StatusNotFound, r.StatusCode())
	})
}

func TestOrgs(t *testing.T) {
	var orgId string
	iamClient := MustIamClient(t)
	tut := MustGenerateTestUserToken(t)
	var userId uuid.UUID
	var client genclient.ClientWithResponsesInterface
	t.Run("a registered user with no organization can create a new org", func(t *testing.T) {
		assert.EventuallyWithT(t, func(c *assert.CollectT) {
			r, err := iamClient.RegisterUserWithResponse(t.Context(), &orchestratoriam.RegisterUserParams{}, orchestratoriam.RegisterUserJSONRequestBody{
				Provider:      "testuser",
				ProviderToken: tut,
			})
			require.NoError(c, err)
			require.Equal(c, http.StatusAccepted, r.StatusCode(), string(r.Body))
			userId = r.JSON202.Id
			client = MustServerClientWithId(t, userId.String())
			res, err := client.CreateOrganizationWithResponse(t.Context())
			if assert.NoError(c, err) && assert.Equal(c, http.StatusCreated, res.StatusCode(), string(res.Body)) {
				orgId = res.JSON201.Id
			}
		}, 30*time.Second, 2*time.Second)
	})

	t.Run("the user is now a member of the created organization", func(t *testing.T) {
		res, err := iamClient.ListOrgMembershipsWithResponse(t.Context(), orgId, nil)
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.Len(t, res.JSON200.Items, 1)
			assert.Equal(t, userId, res.JSON200.Items[0].UserId)
		}
	})

	t.Run("the same user can't create no more organization now", func(t *testing.T) {
		res, err := client.CreateOrganizationWithResponse(t.Context())
		if assert.NoError(t, err) && assert.Equal(t, http.StatusConflict, res.StatusCode(), string(res.Body)) {
			assert.Equal(t, fmt.Sprintf("user %s is already a member of organizations [%s]", userId.String(), orgId), res.JSON409.Message)
		}
	})

	t.Run("a service user cannot create a new org", func(t *testing.T) {
		client = MustServerClientWithId(t, userid.NewServiceUserTokenId().String())
		res, err := client.CreateOrganizationWithResponse(t.Context())
		if assert.NoError(t, err) && assert.Equal(t, http.StatusForbidden, res.StatusCode(), string(res.Body)) {
			assert.Equal(t, "service users are not allowed to create organizations", res.JSON403.Message)
		}
	})
}
