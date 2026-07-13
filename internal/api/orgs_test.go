package api

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/hecho"
	"github.com/stellwerk-labs/golib/herrors"
	orchestratoriam "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	mockorchestratoriam "github.com/stellwerk-labs/platform-orchestrator-cp/internal/clients/orchestratoriam/mocks"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	mockmodel "github.com/stellwerk-labs/platform-orchestrator-cp/internal/model/mocks"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
)

func TestGenerateRandomOrgNameAndSuffix(t *testing.T) {
	r := rand.New(rand.NewPCG(0, 0)) // nolint
	assert.Equal(t, "deep-premium-flamingo-4406", generateRandomOrgNameAndSuffix(r.IntN, ""))
	assert.Equal(t, "green-blazing-platypus-4812", generateRandomOrgNameAndSuffix(r.IntN, ""))
	assert.Equal(t, "prefix-round-swan-3768", generateRandomOrgNameAndSuffix(r.IntN, "prefix"))
	assert.Equal(t, "prefix-distant-crow-0231", generateRandomOrgNameAndSuffix(r.IntN, "prefix"))
	assert.Equal(t, "prefix-seamless-jackal-5773", generateRandomOrgNameAndSuffix(r.IntN, "prefix"))
}

func TestCreateInternalOrganization_random_id(t *testing.T) {
	_, s, fin := MockServer(t)
	defer fin()

	db := s.Database.(*mockmodel.MockDatabaser)
	db.EXPECT().BeginTx(gomock.Any(), gomock.Any()).Return(&mockmodel.MockTxWithCommit{}, nil)
	db.EXPECT().CreateOrg(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _ model.Tx, org *model.Org) (*model.Org, error) {
		require.Equal(t, model.OrgSourceInternal, org.Source)
		require.NotEmpty(t, org.Id)
		org.Status = model.OrgStatusActive
		return org, nil
	})
	r, err := s.CreateInternalOrganization(t.Context(), CreateInternalOrganizationRequestObject{
		&InternalOrganizationCreateBody{},
	})
	require.NoError(t, err)
	require.IsType(t, CreateInternalOrganization201JSONResponse{}, r)
	require.GreaterOrEqual(t, len(r.(CreateInternalOrganization201JSONResponse).Id), 15)
}

func TestCreateInternalOrganization_id_prefix(t *testing.T) {
	_, s, fin := MockServer(t)
	defer fin()

	db := s.Database.(*mockmodel.MockDatabaser)
	db.EXPECT().BeginTx(gomock.Any(), gomock.Any()).Return(&mockmodel.MockTxWithCommit{}, nil)
	db.EXPECT().CreateOrg(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _ model.Tx, org *model.Org) (*model.Org, error) {
		require.Equal(t, model.OrgSourceInternal, org.Source)
		require.NotEmpty(t, org.Id)
		org.Status = model.OrgStatusActive
		return org, nil
	})
	r, err := s.CreateInternalOrganization(t.Context(), CreateInternalOrganizationRequestObject{
		&InternalOrganizationCreateBody{
			IdPrefix: ref.Ref("test"),
		},
	})
	require.NoError(t, err)
	require.IsType(t, CreateInternalOrganization201JSONResponse{}, r)
	assert.Regexp(t, "^test-", r.(CreateInternalOrganization201JSONResponse).Id)
}

func TestCreateInternalOrganization(t *testing.T) {
	_, s, fin := MockServer(t)
	defer fin()

	for _, tc := range []struct {
		name     string
		conflict bool
	}{
		{name: "create internal org - success", conflict: false},
		{name: "create internal org - conflict", conflict: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := s.Database.(*mockmodel.MockDatabaser)
			db.EXPECT().BeginTx(gomock.Any(), gomock.Any()).Return(&mockmodel.MockTxWithCommit{}, nil)
			db.EXPECT().CreateOrg(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _ model.Tx, org *model.Org) (*model.Org, error) {
				require.Equal(t, model.OrgSourceInternal, org.Source)
				require.Equal(t, orgId, org.Id)
				if tc.conflict {
					return nil, model.NewErrConflict("organization already exists")
				}
				org.Status = model.OrgStatusActive
				return org, nil
			})
			r, err := s.CreateInternalOrganization(t.Context(), CreateInternalOrganizationRequestObject{
				&InternalOrganizationCreateBody{
					Id: ref.Ref(orgId),
				},
			})
			require.NoError(t, err)
			if tc.conflict {
				require.IsType(t, CreateInternalOrganization409JSONResponse{}, r)
			} else {
				require.IsType(t, CreateInternalOrganization201JSONResponse{}, r)
				require.Equal(t, orgId, r.(CreateInternalOrganization201JSONResponse).Id)
				require.Equal(t, Internal, r.(CreateInternalOrganization201JSONResponse).Source)
			}
		})
	}
}

func TestCreateOrg_success(t *testing.T) {
	_, s, fin := MockServer(t)
	defer fin()
	userId := userid.NewHumanUserId()
	db := s.Database.(*mockmodel.MockDatabaser)
	db.EXPECT().BeginTx(gomock.Any(), gomock.Any()).Return(&mockmodel.MockTxWithCommit{}, nil)
	iam := s.IamClient.(*mockorchestratoriam.MockClientWithResponsesInterface)
	iam.EXPECT().ListUserMembershipsWithResponse(gomock.Any(), userId, &orchestratoriam.ListUserMembershipsParams{}).Return(&orchestratoriam.ListUserMembershipsResponse{
		JSON200: &orchestratoriam.UserMembershipPage{
			Items: []orchestratoriam.UserMembership{},
		},
		HTTPResponse: &http.Response{
			StatusCode: 200,
		},
	}, nil).Times(1)
	var orgId string
	ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, userId.String())
	db.EXPECT().CreateOrg(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ model.Tx, org *model.Org) (*model.Org, error) {
			require.Equal(t, model.OrgSourcePublic, org.Source)
			orgId = org.Id
			org.Status = model.OrgStatusActive
			return org, nil
		}).Times(1)
	roleId := uuid.New()
	iam.EXPECT().ListRolesWithResponse(gomock.Any(), gomock.Any(), &orchestratoriam.ListRolesParams{}).Return(&orchestratoriam.ListRolesResponse{
		HTTPResponse: &http.Response{StatusCode: http.StatusOK},
		JSON200:      &orchestratoriam.RolePage{Items: []orchestratoriam.Role{{Id: roleId, DisplayName: "Admin"}}},
	}, nil)
	iam.EXPECT().InternalCreateOrgMembershipWithResponse(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, membershipOrgId string, orgMembership orchestratoriam.InternalCreateOrgMembershipJSONRequestBody, _ ...orchestratoriam.RequestEditorFn) (*orchestratoriam.InternalCreateOrgMembershipResponse, error) {
			require.Equal(t, orgId, membershipOrgId)
			require.Equal(t, userId, orgMembership.UserId)
			return &orchestratoriam.InternalCreateOrgMembershipResponse{
				HTTPResponse: &http.Response{
					StatusCode: 201,
				},
				JSON201: &orchestratoriam.OrgMembership{
					Id:          uuid.New(),
					UserId:      userId,
					Subject:     roleId.String(),
					SubjectType: "role",
				},
			}, nil
		}).Times(1)
	r, err := s.CreateOrganization(ctx, CreateOrganizationRequestObject{})
	require.NoError(t, err)
	require.IsType(t, CreateOrganization201JSONResponse{}, r)
	require.Equal(t, orgId, r.(CreateOrganization201JSONResponse).Id)
	require.Equal(t, "custom", r.(CreateOrganization201JSONResponse).Plan)
}

func TestCreateOrg_no_user_in_header(t *testing.T) {
	_, s, fin := MockServer(t)
	defer fin()
	ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, "")
	_, err := s.CreateOrganization(ctx, CreateOrganizationRequestObject{})
	require.Error(t, err)
	require.EqualError(t, err, "code=401, message=Unauthorized")
}

func TestCreateOrg_service_user_token(t *testing.T) {
	_, s, fin := MockServer(t)
	defer fin()
	ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, userid.NewServiceUserTokenId().String())
	r, err := s.CreateOrganization(ctx, CreateOrganizationRequestObject{})
	require.NoError(t, err)
	require.IsType(t, CreateOrganization403JSONResponse{}, r)
	require.Equal(t, "HTTP-403", r.(CreateOrganization403JSONResponse).Error)
	require.Equal(t, "service users are not allowed to create organizations", r.(CreateOrganization403JSONResponse).Message)
}

func TestCreateOrg_user_is_already_in_some_orgs(t *testing.T) {
	_, s, fin := MockServer(t)
	defer fin()
	userid := userid.NewHumanUserId()
	ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, userid.String())
	iam := s.IamClient.(*mockorchestratoriam.MockClientWithResponsesInterface)
	orgId := uuid.New()
	iam.EXPECT().ListUserMembershipsWithResponse(gomock.Any(), userid, &orchestratoriam.ListUserMembershipsParams{}).Return(&orchestratoriam.ListUserMembershipsResponse{
		JSON200: &orchestratoriam.UserMembershipPage{
			Items: []orchestratoriam.UserMembership{
				{OrgId: orgId.String()},
			},
		},
		HTTPResponse: &http.Response{
			StatusCode: 200,
		},
	}, nil).Times(1)
	r, err := s.CreateOrganization(ctx, CreateOrganizationRequestObject{})
	require.NoError(t, err)
	require.IsType(t, CreateOrganization409JSONResponse{}, r)
	require.Equal(t, "HTTP-409", r.(CreateOrganization409JSONResponse).Error)
	require.Equal(t, fmt.Sprintf("user %s is already a member of organizations [%s]", userid.String(), orgId), r.(CreateOrganization409JSONResponse).Message)
}

func TestCreateOrg_user_does_not_exist(t *testing.T) {
	_, s, fin := MockServer(t)
	defer fin()
	userId := userid.NewHumanUserId()
	db := s.Database.(*mockmodel.MockDatabaser)
	db.EXPECT().BeginTx(gomock.Any(), gomock.Any()).Return(&mockmodel.MockTxWithCommit{}, nil)
	iam := s.IamClient.(*mockorchestratoriam.MockClientWithResponsesInterface)
	iam.EXPECT().ListUserMembershipsWithResponse(gomock.Any(), userId, &orchestratoriam.ListUserMembershipsParams{}).Return(&orchestratoriam.ListUserMembershipsResponse{
		JSON200: &orchestratoriam.UserMembershipPage{
			Items: []orchestratoriam.UserMembership{},
		},
		HTTPResponse: &http.Response{
			StatusCode: 200,
		},
	}, nil).Times(1)
	var orgId string
	ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, userId.String())
	db.EXPECT().CreateOrg(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ model.Tx, org *model.Org) (*model.Org, error) {
			require.Equal(t, model.OrgSourcePublic, org.Source)
			orgId = org.Id
			org.Status = model.OrgStatusActive
			return org, nil
		}).Times(1)
	roleId := uuid.New()
	iam.EXPECT().ListRolesWithResponse(gomock.Any(), gomock.Any(), &orchestratoriam.ListRolesParams{}).Return(&orchestratoriam.ListRolesResponse{
		HTTPResponse: &http.Response{StatusCode: http.StatusOK},
		JSON200:      &orchestratoriam.RolePage{Items: []orchestratoriam.Role{{Id: roleId, DisplayName: "Admin"}}},
	}, nil)
	iam.EXPECT().InternalCreateOrgMembershipWithResponse(gomock.Any(), gomock.Any(), gomock.Any()).Return(&orchestratoriam.InternalCreateOrgMembershipResponse{
		HTTPResponse: &http.Response{
			StatusCode: http.StatusConflict,
		},
		JSON409: &orchestratoriam.N409Conflict{
			Message: fmt.Sprintf("user %s does not exist", userId.String()),
		},
	}, nil).Times(1)
	db.EXPECT().DeleteOrg(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _ model.Tx, orgToDelete string) error {
		require.Equal(t, orgId, orgToDelete)
		return nil
	}).Times(1)
	_, err := s.CreateOrganization(ctx, CreateOrganizationRequestObject{})
	require.Error(t, err)
	var humerr *herrors.PlatformOrchestratorError
	require.ErrorAs(t, err, &humerr)
	require.Contains(t, humerr.Message, "failed to add user")
	require.Contains(t, humerr.Message, fmt.Sprintf("user %s does not exist", userId.String()))
}
