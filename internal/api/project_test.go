package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/hecho"
	"github.com/stellwerk-labs/golib/herrors"
	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/genevents"
	orchestratoriam "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	mockorchestratoriam "github.com/stellwerk-labs/platform-orchestrator-cp/internal/clients/orchestratoriam/mocks"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/events"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	mockmodel "github.com/stellwerk-labs/platform-orchestrator-cp/internal/model/mocks"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
)

const (
	orgId     = "test-org"
	projectId = "test-project"
)

func TestUpdateProject_permissions(t *testing.T) {
	projectUuid := uuid.New()
	userId := userid.NewHumanUserId()

	tests := []struct {
		name               string
		authzStatusCode    int
		authzError         error
		getProjectError    error
		expectedStatusCode int
		expectAuthCall     bool
		expectOrgFallback  bool
	}{
		{
			name:               "success - user has manage permission",
			authzStatusCode:    http.StatusNoContent,
			expectedStatusCode: http.StatusOK,
			expectAuthCall:     true,
		},
		{
			name:               "not found - project does not exist",
			getProjectError:    model.NewErrNotFound("project not found"),
			expectedStatusCode: http.StatusNotFound,
			expectAuthCall:     true,
			expectOrgFallback:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, s, cleanup := MockServer(t)
			defer cleanup()

			db := s.Database.(*mockmodel.MockDatabaser)
			mockIamClient := s.IamClient.(*mockorchestratoriam.MockClientWithResponsesInterface)

			if tc.expectAuthCall {
				db.EXPECT().GetProject(gomock.Any(), gomock.Any(), orgId, projectId, model.GetModeDefault).DoAndReturn(
					func(_ context.Context, _ model.Tx, _, _ string, _ model.GetMode) (*model.Project, error) {
						if tc.getProjectError != nil {
							return nil, tc.getProjectError
						}
						return &model.Project{
							Id:          projectId,
							Uuid:        projectUuid,
							OrgId:       orgId,
							DisplayName: "Test Project",
							Status:      model.ProjectStatusActive,
							CreatedAt:   time.Now(),
							UpdatedAt:   time.Now(),
						}, nil
					},
				).Times(1)

				if tc.getProjectError == nil {
					authResp := &orchestratoriam.InternalAuthorizeResponse{
						HTTPResponse: &http.Response{StatusCode: tc.authzStatusCode},
					}
					if tc.authzStatusCode == http.StatusForbidden {
						details := map[string]interface{}{
							"permission": "project_write",
							"resource":   "project:" + projectUuid.String(),
						}
						authResp.JSON403 = &orchestratoriam.N403Forbidden{
							Details: &details,
							Error:   "HTTP-403",
							Message: "forbidden",
						}
					}

					mockIamClient.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
						UserId: userId,
						Checks: []orchestratoriam.ResourcePermissionCheck{projectCheck(projectUuid, PermissionProjectWrite)},
					}).Return(authResp, tc.authzError).Times(1)
				} else if tc.expectOrgFallback {
					// When project is not found, fallback to org auth is still attempted
					mockIamClient.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
						UserId: userId,
						Checks: []orchestratoriam.ResourcePermissionCheck{orgCheck(orgId, PermissionProjectWrite)},
					}).Return(&orchestratoriam.InternalAuthorizeResponse{
						HTTPResponse: &http.Response{StatusCode: http.StatusNoContent},
					}, nil).Times(1)
				}
			}

			if tc.expectedStatusCode == http.StatusOK {
				db.EXPECT().UpdatedProject(gomock.Any(), gomock.Any(), orgId, projectId, gomock.Any()).DoAndReturn(
					func(_ context.Context, _ model.Tx, _, _ string, params model.UpdateProjectParams) (*model.Project, error) {
						return &model.Project{
							Id:          projectId,
							Uuid:        projectUuid,
							OrgId:       orgId,
							DisplayName: params.DisplayName.Must(),
							Status:      model.ProjectStatusActive,
							CreatedAt:   time.Now(),
							UpdatedAt:   params.UpdatedAt,
						}, nil
					},
				).Times(1)
			}

			ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, userId.String())
			r, err := s.UpdateProject(ctx, UpdateProjectRequestObject{
				OrgId:     orgId,
				ProjectId: projectId,
				Body: &ProjectUpdateBody{
					DisplayName: "Updated Project Name",
				},
			})

			switch tc.expectedStatusCode {
			case http.StatusOK:
				require.NoError(t, err)
				resp, ok := r.(UpdateProject200JSONResponse)
				require.True(t, ok, "expected UpdateProject200JSONResponse, got %T", r)
				assert.Equal(t, projectId, resp.Id)
				assert.Equal(t, "Updated Project Name", resp.DisplayName)
			case http.StatusNotFound:
				var herr *herrors.PlatformOrchestratorError
				require.ErrorAs(t, err, &herr)

				assert.Equal(t, http.StatusNotFound, herr.StatusCode)
				assert.Equal(t, "project not found", herr.Message)
			}
		})
	}
}

func TestUpdateProject_successWithOrgFallback(t *testing.T) {
	_, s, cleanup := MockServer(t)
	defer cleanup()

	projectUuid := uuid.New()
	userId := userid.NewHumanUserId()

	db := s.Database.(*mockmodel.MockDatabaser)
	mockIamClient := s.IamClient.(*mockorchestratoriam.MockClientWithResponsesInterface)

	db.EXPECT().GetProject(gomock.Any(), gomock.Any(), orgId, projectId, model.GetModeDefault).Return(&model.Project{
		Id:          projectId,
		Uuid:        projectUuid,
		OrgId:       orgId,
		DisplayName: "Test Project",
		Status:      model.ProjectStatusActive,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}, nil).Times(1)

	// Project-level authorization fails
	projectAuthResp := &orchestratoriam.InternalAuthorizeResponse{
		HTTPResponse: &http.Response{StatusCode: http.StatusForbidden},
	}
	projectDetails := map[string]interface{}{
		"permission": "project_write",
		"resource":   "project:" + projectUuid.String(),
	}
	projectAuthResp.JSON403 = &orchestratoriam.N403Forbidden{
		Details: &projectDetails,
		Error:   "HTTP-403",
		Message: "forbidden",
	}

	mockIamClient.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
		UserId: userId,
		Checks: []orchestratoriam.ResourcePermissionCheck{projectCheck(projectUuid, PermissionProjectWrite)},
	}).Return(projectAuthResp, nil).Times(1)

	// Fallback to org-level authorization succeeds
	mockIamClient.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
		UserId: userId,
		Checks: []orchestratoriam.ResourcePermissionCheck{orgCheck(orgId, PermissionProjectWrite)},
	}).Return(&orchestratoriam.InternalAuthorizeResponse{
		HTTPResponse: &http.Response{StatusCode: http.StatusNoContent},
	}, nil).Times(1)

	db.EXPECT().UpdatedProject(gomock.Any(), gomock.Any(), orgId, projectId, gomock.Any()).DoAndReturn(
		func(_ context.Context, _ model.Tx, _, _ string, params model.UpdateProjectParams) (*model.Project, error) {
			return &model.Project{
				Id:          projectId,
				Uuid:        projectUuid,
				OrgId:       orgId,
				DisplayName: params.DisplayName.Must(),
				Status:      model.ProjectStatusActive,
				CreatedAt:   time.Now(),
				UpdatedAt:   params.UpdatedAt,
			}, nil
		},
	).Times(1)

	ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, userId.String())
	r, err := s.UpdateProject(ctx, UpdateProjectRequestObject{
		OrgId:     orgId,
		ProjectId: projectId,
		Body: &ProjectUpdateBody{
			DisplayName: "Updated Project Name",
		},
	})

	require.NoError(t, err)
	resp, ok := r.(UpdateProject200JSONResponse)
	require.True(t, ok, "expected UpdateProject200JSONResponse, got %T", r)
	assert.Equal(t, projectId, resp.Id)
	assert.Equal(t, "Updated Project Name", resp.DisplayName)
}

func TestUpdateProject_forbidden(t *testing.T) {
	_, s, cleanup := MockServer(t)
	defer cleanup()

	projectUuid := uuid.New()
	userId := userid.NewHumanUserId()

	db := s.Database.(*mockmodel.MockDatabaser)
	mockIamClient := s.IamClient.(*mockorchestratoriam.MockClientWithResponsesInterface)

	db.EXPECT().GetProject(gomock.Any(), gomock.Any(), orgId, projectId, model.GetModeDefault).Return(&model.Project{
		Id:          projectId,
		Uuid:        projectUuid,
		OrgId:       orgId,
		DisplayName: "Test Project",
		Status:      model.ProjectStatusActive,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}, nil).Times(1)

	// Project-level authorization fails
	projectAuthResp := &orchestratoriam.InternalAuthorizeResponse{
		HTTPResponse: &http.Response{StatusCode: http.StatusForbidden},
	}
	projectDetails := map[string]interface{}{
		"permission": "project_write",
		"resource":   "project:" + projectUuid.String(),
	}
	projectAuthResp.JSON403 = &orchestratoriam.N403Forbidden{
		Details: &projectDetails,
		Error:   "HTTP-403",
		Message: "forbidden",
	}

	mockIamClient.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
		UserId: userId,
		Checks: []orchestratoriam.ResourcePermissionCheck{projectCheck(projectUuid, PermissionProjectWrite)},
	}).Return(projectAuthResp, nil).Times(1)

	// Fallback to org-level authorization also fails
	orgAuthResp := &orchestratoriam.InternalAuthorizeResponse{
		HTTPResponse: &http.Response{StatusCode: http.StatusForbidden},
	}
	orgDetails := map[string]interface{}{
		"permission": "project_write",
		"resource":   "org:" + orgId,
	}
	orgAuthResp.JSON403 = &orchestratoriam.N403Forbidden{
		Details: &orgDetails,
		Error:   "HTTP-403",
		Message: "forbidden",
	}

	mockIamClient.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
		UserId: userId,
		Checks: []orchestratoriam.ResourcePermissionCheck{orgCheck(orgId, PermissionProjectWrite)},
	}).Return(orgAuthResp, nil).Times(1)

	ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, userId.String())
	r, err := s.UpdateProject(ctx, UpdateProjectRequestObject{
		OrgId:     orgId,
		ProjectId: projectId,
		Body: &ProjectUpdateBody{
			DisplayName: "Updated Project Name",
		},
	})

	require.Error(t, err)
	herr, ok := err.(*herrors.PlatformOrchestratorError)
	require.True(t, ok, "expected *herrors.PlatformOrchestratorError, got %T", err)
	assert.Equal(t, http.StatusForbidden, herr.StatusCode)
	assert.Contains(t, herr.Details, "permission")
	assert.Contains(t, herr.Details, "resource")
	assert.Nil(t, r)
}

func TestUpdateProject_emptyBody(t *testing.T) {

	projectUuid := uuid.New()
	userId := userid.NewHumanUserId()

	_, s, cleanup := MockServer(t)
	defer cleanup()

	db := s.Database.(*mockmodel.MockDatabaser)
	mockIamClient := s.IamClient.(*mockorchestratoriam.MockClientWithResponsesInterface)

	db.EXPECT().GetProject(gomock.Any(), gomock.Any(), orgId, projectId, model.GetModeDefault).Return(&model.Project{
		Id:          projectId,
		Uuid:        projectUuid,
		OrgId:       orgId,
		DisplayName: "Test Project",
		Status:      model.ProjectStatusActive,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}, nil).Times(2)

	mockIamClient.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
		UserId: userId,
		Checks: []orchestratoriam.ResourcePermissionCheck{projectCheck(projectUuid, PermissionProjectWrite)},
	}).Return(&orchestratoriam.InternalAuthorizeResponse{
		HTTPResponse: &http.Response{StatusCode: http.StatusNoContent},
	}, nil).Times(1)

	ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, userId.String())
	r, err := s.UpdateProject(ctx, UpdateProjectRequestObject{
		OrgId:     orgId,
		ProjectId: projectId,
		Body:      nil,
	})

	require.NoError(t, err)
	resp, ok := r.(UpdateProject200JSONResponse)
	require.True(t, ok, "expected UpdateProject200JSONResponse, got %T", r)
	assert.Equal(t, projectId, resp.Id)
	assert.Equal(t, "Test Project", resp.DisplayName)
}

func TestUpdateProject_checkProjectAuthorization_flow(t *testing.T) {

	projectUuid := uuid.New()
	userId := userid.NewHumanUserId()

	t.Run("authorization check retrieves project UUID", func(t *testing.T) {
		_, s, cleanup := MockServer(t)
		defer cleanup()

		db := s.Database.(*mockmodel.MockDatabaser)
		mockIamClient := s.IamClient.(*mockorchestratoriam.MockClientWithResponsesInterface)

		db.EXPECT().GetProject(gomock.Any(), gomock.Any(), orgId, projectId, model.GetModeDefault).DoAndReturn(
			func(_ context.Context, _ model.Tx, orgId, projectId string, _ model.GetMode) (*model.Project, error) {
				return &model.Project{
					Id:          projectId,
					Uuid:        projectUuid,
					OrgId:       orgId,
					DisplayName: "Test Project",
					Status:      model.ProjectStatusActive,
					CreatedAt:   time.Now(),
					UpdatedAt:   time.Now(),
				}, nil
			},
		).Times(1)

		mockIamClient.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
			UserId: userId,
			Checks: []orchestratoriam.ResourcePermissionCheck{projectCheck(projectUuid, PermissionProjectWrite)},
		}).Return(&orchestratoriam.InternalAuthorizeResponse{
			HTTPResponse: &http.Response{StatusCode: http.StatusNoContent},
		}, nil).Times(1)

		db.EXPECT().UpdatedProject(gomock.Any(), gomock.Any(), orgId, projectId, gomock.Any()).DoAndReturn(
			func(_ context.Context, _ model.Tx, _, _ string, params model.UpdateProjectParams) (*model.Project, error) {
				assert.Equal(t, opt.Of("Updated Name"), params.DisplayName)
				return &model.Project{
					Id:          projectId,
					Uuid:        projectUuid,
					OrgId:       orgId,
					DisplayName: "Updated Name",
					Status:      model.ProjectStatusActive,
					CreatedAt:   time.Now(),
					UpdatedAt:   params.UpdatedAt,
				}, nil
			},
		).Times(1)

		ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, userId.String())
		r, err := s.UpdateProject(ctx, UpdateProjectRequestObject{
			OrgId:     orgId,
			ProjectId: projectId,
			Body: &ProjectUpdateBody{
				DisplayName: "Updated Name",
			},
		})

		require.NoError(t, err)
		resp, ok := r.(UpdateProject200JSONResponse)
		require.True(t, ok)
		assert.Equal(t, "Updated Name", resp.DisplayName)
	})
}

func TestCreateProject_success(t *testing.T) {
	tests := []struct {
		name                string
		projectID           string
		displayName         *string
		expectedDisplayName string
	}{
		{
			name:                "success with explicit display name",
			projectID:           "new-project",
			displayName:         ref.Ref("My New Project"),
			expectedDisplayName: "My New Project",
		},
		{
			name:                "success with display name defaulting to ID",
			projectID:           "another-project",
			displayName:         nil,
			expectedDisplayName: "another-project",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, s, cleanup := MockServer(t)
			defer cleanup()

			userID := userid.NewHumanUserId()
			orgUuid := uuid.New()
			projectUuid := uuid.New()

			db := s.Database.(*mockmodel.MockDatabaser)
			mockIamClient := s.IamClient.(*mockorchestratoriam.MockClientWithResponsesInterface)

			mockIamClient.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
				UserId: userID,
				Checks: []orchestratoriam.ResourcePermissionCheck{orgCheck(orgId, PermissionProjectWrite)},
			}).Return(&orchestratoriam.InternalAuthorizeResponse{
				HTTPResponse: &http.Response{StatusCode: http.StatusNoContent},
			}, nil).Times(1)

			db.EXPECT().GetOrg(gomock.Any(), gomock.Not(nil), orgId).Return(&model.Org{
				Id:   orgId,
				Uuid: orgUuid,
			}, nil).Times(1)

			db.EXPECT().CreateProject(gomock.Any(), gomock.Not(nil), gomock.Any()).DoAndReturn(
				func(_ context.Context, _ model.Tx, p *model.Project) (*model.Project, error) {
					assert.Equal(t, orgId, p.OrgId)
					assert.Equal(t, orgUuid, p.OrgUuid)
					assert.Equal(t, tc.projectID, p.Id)
					assert.Equal(t, tc.expectedDisplayName, p.DisplayName)
					assert.Equal(t, model.ProjectStatusActive, p.Status)
					assert.False(t, p.CreatedAt.IsZero())
					assert.False(t, p.UpdatedAt.IsZero())

					p.Uuid = projectUuid
					return p, nil
				},
			).Times(1)

			ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, userID.String())
			r, err := s.CreateProject(ctx, CreateProjectRequestObject{
				OrgId: orgId,
				Body: &ProjectCreateBody{
					Id:          tc.projectID,
					DisplayName: tc.displayName,
				},
			})

			require.NoError(t, err)
			resp, ok := r.(CreateProject201JSONResponse)
			require.True(t, ok, "expected CreateProject201JSONResponse, got %T", r)
			assert.Equal(t, tc.projectID, resp.Id)
			assert.Equal(t, projectUuid, resp.Uuid)
			assert.Equal(t, tc.expectedDisplayName, resp.DisplayName)
			assert.Equal(t, ProjectStatusActive, resp.Status)
			assert.False(t, resp.CreatedAt.IsZero())
			assert.False(t, resp.UpdatedAt.IsZero())

			rec := s.Publisher.(*hmessaging.RecordingPublisher).Messages()
			if assert.Len(t, rec, 1) {
				assert.Equal(t, string(genevents.IoPlatformOrchestratorProjectCreated), rec[0].Subject)
				var event events.CloudEvent[genevents.ProjectChangedData]
				require.NoError(t, json.Unmarshal(rec[0].Data, &event))
				assert.Equal(t, orgId, event.Data.OrgId)
				assert.Equal(t, orgUuid, event.Data.OrgUuid)
				assert.Equal(t, tc.projectID, event.Data.ProjectId)
				assert.Equal(t, projectUuid, event.Data.ProjectUuid)
				assert.Equal(t, ref.Ref(string(model.ProjectStatusActive)), event.Data.Status)
			}
		})
	}
}

func TestCreateProject_forbidden(t *testing.T) {
	_, s, cleanup := MockServer(t)
	defer cleanup()

	userID := userid.NewHumanUserId()

	mockIamClient := s.IamClient.(*mockorchestratoriam.MockClientWithResponsesInterface)

	authResp := &orchestratoriam.InternalAuthorizeResponse{
		HTTPResponse: &http.Response{StatusCode: http.StatusForbidden},
	}
	details := map[string]interface{}{
		"permission": "project_write",
		"resource":   "org:" + orgId,
	}
	authResp.JSON403 = &orchestratoriam.N403Forbidden{
		Details: &details,
		Error:   "HTTP-403",
		Message: "forbidden",
	}

	mockIamClient.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
		UserId: userID,
		Checks: []orchestratoriam.ResourcePermissionCheck{orgCheck(orgId, PermissionProjectWrite)},
	}).Return(authResp, nil).Times(1)

	ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, userID.String())
	r, err := s.CreateProject(ctx, CreateProjectRequestObject{
		OrgId: orgId,
		Body: &ProjectCreateBody{
			Id:          "new-project",
			DisplayName: ref.Ref("New Project"),
		},
	})

	require.Error(t, err)
	herr, ok := err.(*herrors.PlatformOrchestratorError)
	require.True(t, ok, "expected *herrors.PlatformOrchestratorError, got %T", err)
	assert.Equal(t, http.StatusForbidden, herr.StatusCode)
	assert.Contains(t, herr.Details, "permission")
	assert.Contains(t, herr.Details, "resource")
	assert.Nil(t, r)
}

func TestCreateProject_orgNotFound(t *testing.T) {
	_, s, cleanup := MockServer(t)
	defer cleanup()

	userID := userid.NewHumanUserId()

	db := s.Database.(*mockmodel.MockDatabaser)
	mockIamClient := s.IamClient.(*mockorchestratoriam.MockClientWithResponsesInterface)

	mockIamClient.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
		UserId: userID,
		Checks: []orchestratoriam.ResourcePermissionCheck{orgCheck(orgId, PermissionProjectWrite)},
	}).Return(&orchestratoriam.InternalAuthorizeResponse{
		HTTPResponse: &http.Response{StatusCode: http.StatusNoContent},
	}, nil).Times(1)

	db.EXPECT().GetOrg(gomock.Any(), gomock.Not(nil), orgId).Return(
		nil, model.NewErrNotFound("organization not found"),
	).Times(1)

	ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, userID.String())
	r, err := s.CreateProject(ctx, CreateProjectRequestObject{
		OrgId: orgId,
		Body: &ProjectCreateBody{
			Id:          "new-project",
			DisplayName: ref.Ref("New Project"),
		},
	})

	require.NoError(t, err)
	resp, ok := r.(CreateProject409JSONResponse)
	require.True(t, ok, "expected CreateProject409JSONResponse, got %T", r)
	assert.Equal(t, "HTTP-409", resp.Error)
	assert.Equal(t, "organization not found", resp.Message)
}

func TestCreateProject_projectConflict(t *testing.T) {
	_, s, cleanup := MockServer(t)
	defer cleanup()

	userID := userid.NewHumanUserId()
	orgUuid := uuid.New()

	db := s.Database.(*mockmodel.MockDatabaser)
	mockIamClient := s.IamClient.(*mockorchestratoriam.MockClientWithResponsesInterface)

	mockIamClient.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
		UserId: userID,
		Checks: []orchestratoriam.ResourcePermissionCheck{orgCheck(orgId, PermissionProjectWrite)},
	}).Return(&orchestratoriam.InternalAuthorizeResponse{
		HTTPResponse: &http.Response{StatusCode: http.StatusNoContent},
	}, nil).Times(1)

	db.EXPECT().GetOrg(gomock.Any(), gomock.Not(nil), orgId).Return(&model.Org{
		Id:   orgId,
		Uuid: orgUuid,
	}, nil).Times(1)

	db.EXPECT().CreateProject(gomock.Any(), gomock.Not(nil), gomock.Any()).Return(
		nil, model.NewErrConflict("project with this ID already exists"),
	).Times(1)

	ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, userID.String())
	r, err := s.CreateProject(ctx, CreateProjectRequestObject{
		OrgId: orgId,
		Body: &ProjectCreateBody{
			Id:          "existing-project",
			DisplayName: ref.Ref("Existing Project"),
		},
	})

	require.NoError(t, err)
	resp, ok := r.(CreateProject409JSONResponse)
	require.True(t, ok, "expected CreateProject409JSONResponse, got %T", r)
	assert.Equal(t, "HTTP-409", resp.Error)
	assert.Equal(t, "project with this ID already exists", resp.Message)
}

func TestDeleteProject_success(t *testing.T) {
	_, s, cleanup := MockServer(t)
	defer cleanup()

	userID := userid.NewHumanUserId()
	projectUuid := uuid.New()
	orgUuid := uuid.New()

	db := s.Database.(*mockmodel.MockDatabaser)
	mockIamClient := s.IamClient.(*mockorchestratoriam.MockClientWithResponsesInterface)

	mockIamClient.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
		UserId: userID,
		Checks: []orchestratoriam.ResourcePermissionCheck{orgCheck(orgId, PermissionProjectWrite)},
	}).Return(&orchestratoriam.InternalAuthorizeResponse{
		HTTPResponse: &http.Response{StatusCode: http.StatusNoContent},
	}, nil).Times(1)

	db.EXPECT().GetProject(gomock.Any(), gomock.Not(nil), orgId, projectId, model.GetModeForUpdate).Return(&model.Project{
		Id:      projectId,
		Uuid:    projectUuid,
		OrgId:   orgId,
		OrgUuid: orgUuid,
		Status:  model.ProjectStatusActive,
	}, nil).Times(1)

	db.EXPECT().DeleteProject(gomock.Any(), gomock.Not(nil), orgId, projectId).Return(nil).Times(1)

	ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, userID.String())
	r, err := s.DeleteProject(ctx, DeleteProjectRequestObject{
		OrgId:     orgId,
		ProjectId: projectId,
	})

	require.NoError(t, err)
	_, ok := r.(DeleteProject204Response)
	require.True(t, ok, "expected DeleteProject204Response, got %T", r)

	rec := s.Publisher.(*hmessaging.RecordingPublisher).Messages()
	if assert.Len(t, rec, 1) {
		assert.Equal(t, string(genevents.IoPlatformOrchestratorProjectDeleted), rec[0].Subject)
		var event events.CloudEvent[genevents.ProjectChangedData]
		require.NoError(t, json.Unmarshal(rec[0].Data, &event))
		assert.Equal(t, orgId, event.Data.OrgId)
		assert.Equal(t, orgUuid, event.Data.OrgUuid)
		assert.Equal(t, projectId, event.Data.ProjectId)
		assert.Equal(t, projectUuid, event.Data.ProjectUuid)
	}
}

func TestDeleteProject_withDeleteRules(t *testing.T) {
	_, s, cleanup := MockServer(t)
	defer cleanup()

	userID := userid.NewHumanUserId()
	projectUuid := uuid.New()
	orgUuid := uuid.New()

	db := s.Database.(*mockmodel.MockDatabaser)
	mockIamClient := s.IamClient.(*mockorchestratoriam.MockClientWithResponsesInterface)

	mockIamClient.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
		UserId: userID,
		Checks: []orchestratoriam.ResourcePermissionCheck{orgCheck(orgId, PermissionProjectWrite)},
	}).Return(&orchestratoriam.InternalAuthorizeResponse{
		HTTPResponse: &http.Response{StatusCode: http.StatusNoContent},
	}, nil).Times(1)

	db.EXPECT().GetProject(gomock.Any(), gomock.Not(nil), orgId, projectId, model.GetModeForUpdate).Return(&model.Project{
		Id:      projectId,
		Uuid:    projectUuid,
		OrgId:   orgId,
		OrgUuid: orgUuid,
		Status:  model.ProjectStatusActive,
	}, nil).Times(1)

	projectIDPtr := projectId
	db.EXPECT().BulkDeleteModuleRuleDefinitions(gomock.Any(), gomock.Not(nil), orgId, model.DeleteModuleRulesParams{
		ByProjectId: &projectIDPtr,
	}).Return([]string{"rule-id-1", "rule-id-2"}, nil).Times(1)

	db.EXPECT().BulkDeleteRunnerRules(gomock.Any(), gomock.Not(nil), orgId, model.DeleteRunnerRulesParams{
		ByProjectId: &projectIDPtr,
	}).Return([]string{"runner-rule-id-1"}, nil).Times(1)

	db.EXPECT().DeleteProject(gomock.Any(), gomock.Not(nil), orgId, projectId).Return(nil).Times(1)

	ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, userID.String())
	deleteRules := true
	r, err := s.DeleteProject(ctx, DeleteProjectRequestObject{
		OrgId:     orgId,
		ProjectId: projectId,
		Params: DeleteProjectParams{
			DeleteRules: &deleteRules,
		},
	})

	require.NoError(t, err)
	_, ok := r.(DeleteProject204Response)
	require.True(t, ok, "expected DeleteProject204Response, got %T", r)

	rec := s.Publisher.(*hmessaging.RecordingPublisher).Messages()
	if assert.Len(t, rec, 1) {
		assert.Equal(t, string(genevents.IoPlatformOrchestratorProjectDeleted), rec[0].Subject)
	}
}

func TestDeleteProject_forbidden(t *testing.T) {
	_, s, cleanup := MockServer(t)
	defer cleanup()

	userID := userid.NewHumanUserId()

	mockIamClient := s.IamClient.(*mockorchestratoriam.MockClientWithResponsesInterface)

	authResp := &orchestratoriam.InternalAuthorizeResponse{
		HTTPResponse: &http.Response{StatusCode: http.StatusForbidden},
	}
	details := map[string]interface{}{
		"permission": "project_write",
		"resource":   "org:" + orgId,
	}
	authResp.JSON403 = &orchestratoriam.N403Forbidden{
		Details: &details,
		Error:   "HTTP-403",
		Message: "forbidden",
	}

	mockIamClient.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
		UserId: userID,
		Checks: []orchestratoriam.ResourcePermissionCheck{orgCheck(orgId, PermissionProjectWrite)},
	}).Return(authResp, nil).Times(1)

	ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, userID.String())
	r, err := s.DeleteProject(ctx, DeleteProjectRequestObject{
		OrgId:     orgId,
		ProjectId: projectId,
	})

	require.Error(t, err)
	herr, ok := err.(*herrors.PlatformOrchestratorError)
	require.True(t, ok, "expected *herrors.PlatformOrchestratorError, got %T", err)
	assert.Equal(t, http.StatusForbidden, herr.StatusCode)
	assert.Contains(t, herr.Details, "permission")
	assert.Contains(t, herr.Details, "resource")
	assert.Nil(t, r)
}

func TestDeleteProject_notFound(t *testing.T) {
	_, s, cleanup := MockServer(t)
	defer cleanup()

	userID := userid.NewHumanUserId()

	db := s.Database.(*mockmodel.MockDatabaser)
	mockIamClient := s.IamClient.(*mockorchestratoriam.MockClientWithResponsesInterface)

	mockIamClient.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
		UserId: userID,
		Checks: []orchestratoriam.ResourcePermissionCheck{orgCheck(orgId, PermissionProjectWrite)},
	}).Return(&orchestratoriam.InternalAuthorizeResponse{
		HTTPResponse: &http.Response{StatusCode: http.StatusNoContent},
	}, nil).Times(1)

	db.EXPECT().GetProject(gomock.Any(), gomock.Not(nil), orgId, "non-existent-project", model.GetModeForUpdate).Return(
		nil, model.NewErrNotFound("project not found"),
	).Times(1)

	ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, userID.String())
	r, err := s.DeleteProject(ctx, DeleteProjectRequestObject{
		OrgId:     orgId,
		ProjectId: "non-existent-project",
	})

	require.NoError(t, err)
	resp, ok := r.(DeleteProject404JSONResponse)
	require.True(t, ok, "expected DeleteProject404JSONResponse, got %T", r)
	assert.Equal(t, "HTTP-404", resp.Error)
	assert.Equal(t, "project not found", resp.Message)
}

func TestDeleteProject_conflict(t *testing.T) {
	_, s, cleanup := MockServer(t)
	defer cleanup()

	userID := userid.NewHumanUserId()
	projectUuid := uuid.New()
	orgUuid := uuid.New()

	db := s.Database.(*mockmodel.MockDatabaser)
	mockIamClient := s.IamClient.(*mockorchestratoriam.MockClientWithResponsesInterface)

	mockIamClient.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
		UserId: userID,
		Checks: []orchestratoriam.ResourcePermissionCheck{orgCheck(orgId, PermissionProjectWrite)},
	}).Return(&orchestratoriam.InternalAuthorizeResponse{
		HTTPResponse: &http.Response{StatusCode: http.StatusNoContent},
	}, nil).Times(1)

	db.EXPECT().GetProject(gomock.Any(), gomock.Not(nil), orgId, projectId, model.GetModeForUpdate).Return(&model.Project{
		Id:      projectId,
		Uuid:    projectUuid,
		OrgId:   orgId,
		OrgUuid: orgUuid,
		Status:  model.ProjectStatusActive,
	}, nil).Times(1)

	db.EXPECT().DeleteProject(gomock.Any(), gomock.Not(nil), orgId, projectId).Return(
		model.NewErrConflict("project has active environments"),
	).Times(1)

	ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, userID.String())
	r, err := s.DeleteProject(ctx, DeleteProjectRequestObject{
		OrgId:     orgId,
		ProjectId: projectId,
	})

	require.NoError(t, err)
	resp, ok := r.(DeleteProject409JSONResponse)
	require.True(t, ok, "expected DeleteProject409JSONResponse, got %T", r)
	assert.Equal(t, "HTTP-409", resp.Error)
	assert.Equal(t, "project has active environments", resp.Message)
}
