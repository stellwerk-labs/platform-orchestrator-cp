package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

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

func TestGetMatchingRunners(t *testing.T) {
	_, s, fin := MockServer(t)
	defer fin()

	const (
		orgId     = "org-id"
		projId    = "project-id"
		envTypeId = "env-type-id"
	)

	for _, tc := range []struct {
		name     string
		items    []model.RunnerRule
		expected []RunnerRuleWithSpecificity
	}{
		{
			name:     "no rules",
			items:    []model.RunnerRule{},
			expected: nil,
		},
		{
			name:  "org wide and project id rules",
			items: []model.RunnerRule{{}, {ProjectId: opt.Of(projId)}},
			expected: []RunnerRuleWithSpecificity{
				{Rule: model.RunnerRule{ProjectId: opt.Of(projId)}, Specificity: 2},
				{Rule: model.RunnerRule{}, Specificity: 0},
			},
		},
		{
			name:  "org wide, project id and env type rules",
			items: []model.RunnerRule{{}, {ProjectId: opt.Of(projId)}, {EnvTypeId: opt.Of(envTypeId)}},
			expected: []RunnerRuleWithSpecificity{
				{Rule: model.RunnerRule{ProjectId: opt.Of(projId)}, Specificity: 2},
				{Rule: model.RunnerRule{EnvTypeId: opt.Of(envTypeId)}, Specificity: 1},
				{Rule: model.RunnerRule{}, Specificity: 0},
			},
		},
		{
			name:  "org wide, project id, env type and full defined rules",
			items: []model.RunnerRule{{}, {ProjectId: opt.Of(projId)}, {EnvTypeId: opt.Of(envTypeId)}, {ProjectId: opt.Of(projId), EnvTypeId: opt.Of(envTypeId)}},
			expected: []RunnerRuleWithSpecificity{
				{Rule: model.RunnerRule{ProjectId: opt.Of(projId), EnvTypeId: opt.Of(envTypeId)}, Specificity: 3},
				{Rule: model.RunnerRule{ProjectId: opt.Of(projId)}, Specificity: 2},
				{Rule: model.RunnerRule{EnvTypeId: opt.Of(envTypeId)}, Specificity: 1},
				{Rule: model.RunnerRule{}, Specificity: 0},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := s.Database.(*mockmodel.MockDatabaser)
			db.EXPECT().ListRunnerRules(gomock.Any(), gomock.Any(), orgId, "", getRunnerRulesPaginationSize, model.ListRunnerRulesParams{
				EffectiveInProjectId: ref.Ref(projId),
				EffectiveInEnvTypeId: ref.Ref(envTypeId),
			}).Return(tc.items, "", nil).Times(1)

			actual, err := s.getMatchingRunners(t.Context(), nil, orgId, projId, envTypeId)
			require.NoError(t, err)
			assert.Len(t, tc.expected, len(actual))
			for idx, item := range actual {
				assert.Equal(t, tc.expected[idx].Rule.ProjectId, item.Rule.ProjectId, "index %d", idx)
				assert.Equal(t, tc.expected[idx].Rule.EnvTypeId, item.Rule.EnvTypeId, "index %d", idx)
			}
		})
	}
}

func TestDeleteEnvironment_default(t *testing.T) {
	tests := []struct {
		name        string
		envStatus   model.EnvironmentStatus
		force       bool
		expectError bool
	}{
		{
			name:      "delete when status active",
			envStatus: model.EnvironmentStatusActive,
		},
		{
			name:      "force delete when status active",
			envStatus: model.EnvironmentStatusActive,
			force:     true,
		},
		{
			name:      "force delete when status deleting",
			envStatus: model.EnvironmentStatusDeleting,
			force:     true,
		},
		{
			name:        "error: delete when status deleting",
			envStatus:   model.EnvironmentStatusDeleting,
			expectError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, s, cleanup := MockServer(t)
			defer cleanup()
			envUuid := uuid.MustParse("01234567-89ab-cdef-0123-456789abcdef")
			projectUuid := uuid.New()

			db := s.Database.(*mockmodel.MockDatabaser)
			userId := userid.NewHumanUserId()
			mockIamClient := s.IamClient.(*mockorchestratoriam.MockClientWithResponsesInterface)

			// GetProject for authorization check
			db.EXPECT().GetProject(gomock.Any(), nil, "org-id", "proj-id", model.GetModeDefault).Return(&model.Project{
				Uuid: projectUuid,
			}, nil).Times(1)

			mockIamClient.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
				UserId: userId,
				Checks: []orchestratoriam.ResourcePermissionCheck{projectCheck(projectUuid, PermissionEnvironmentWrite)},
			}).Return(&orchestratoriam.InternalAuthorizeResponse{
				HTTPResponse: &http.Response{StatusCode: http.StatusNoContent},
			}, nil).Times(1)

			db.EXPECT().GetEnvironment(gomock.Any(), nil, "org-id", "proj-id", "env-id", model.GetModeDefault).Return(&model.Environment{
				Status: tc.envStatus,
				Uuid:   envUuid,
			}, nil).Times(1)
			db.EXPECT().GetEnvironment(gomock.Any(), gomock.Not(nil), "org-id", "proj-id", "env-id", model.GetModeForUpdate).Return(&model.Environment{
				Status: tc.envStatus,
			}, nil).Times(1)
			db.EXPECT().UpdateEnvironment(gomock.Any(), gomock.Not(nil), "org-id", "proj-id", "env-id", gomock.Any()).
				Return(&model.Environment{}, nil)

			ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, userId.String())
			var params DeleteEnvironmentParams
			if tc.force {
				params = DeleteEnvironmentParams{Force: ref.Ref(true)}
			}
			r, err := s.DeleteEnvironment(ctx, DeleteEnvironmentRequestObject{OrgId: "org-id", ProjectId: "proj-id", EnvId: "env-id", Params: params})
			require.NoError(t, err)
			if tc.expectError {
				require.IsType(t, DeleteEnvironment409JSONResponse{}, r)
			} else {
				require.IsType(t, DeleteEnvironment202JSONResponse{}, r)
				rec := s.Publisher.(*hmessaging.RecordingPublisher).Messages()
				if assert.Len(t, rec, 1) {
					assert.Equal(t, "io.platform-orchestrator.environment.updated", rec[0].Subject)
					var event events.CloudEvent[genevents.EnvChangedData]
					require.NoError(t, json.Unmarshal(rec[0].Data, &event))
					assert.Equal(t, tc.force, ref.DerefOr(event.Data.Force, false))
					assert.False(t, ref.DerefOr(event.Data.DeleteRules, false), "deleteRules should be set to true in the event")
				}
			}
		})
	}
}

func TestDeleteEnvironment_withDeleteRules(t *testing.T) {
	_, s, cleanup := MockServer(t)
	defer cleanup()
	envUuid := uuid.MustParse("01234567-89ab-cdef-0123-456789abcdef")
	projectUuid := uuid.New()

	db := s.Database.(*mockmodel.MockDatabaser)
	userId := userid.NewHumanUserId()
	mockIamClient := s.IamClient.(*mockorchestratoriam.MockClientWithResponsesInterface)

	// GetProject for authorization check
	db.EXPECT().GetProject(gomock.Any(), nil, "org-id", "proj-id", model.GetModeDefault).Return(&model.Project{
		Uuid: projectUuid,
	}, nil).Times(1)

	mockIamClient.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
		UserId: userId,
		Checks: []orchestratoriam.ResourcePermissionCheck{projectCheck(projectUuid, PermissionEnvironmentWrite)},
	}).Return(&orchestratoriam.InternalAuthorizeResponse{
		HTTPResponse: &http.Response{StatusCode: http.StatusNoContent},
	}, nil).Times(1)

	db.EXPECT().GetEnvironment(gomock.Any(), nil, "org-id", "proj-id", "env-id", model.GetModeDefault).Return(&model.Environment{
		Status: model.EnvironmentStatusActive,
		Uuid:   envUuid,
	}, nil).Times(1)
	db.EXPECT().GetEnvironment(gomock.Any(), gomock.Not(nil), "org-id", "proj-id", "env-id", model.GetModeForUpdate).Return(&model.Environment{
		Status: model.EnvironmentStatusActive,
	}, nil)
	db.EXPECT().BulkDeleteModuleRuleDefinitions(gomock.Any(), gomock.Not(nil), "org-id", model.DeleteModuleRulesParams{
		ByProjectId: ref.Ref("proj-id"),
		ByEnvId:     ref.Ref("env-id"),
	}).Return([]string{"rule-id-1", "rule-id-2"}, nil)
	db.EXPECT().UpdateEnvironment(gomock.Any(), gomock.Not(nil), "org-id", "proj-id", "env-id", gomock.Any()).
		Return(&model.Environment{}, nil)

	ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, userId.String())
	params := DeleteEnvironmentParams{DeleteRules: ref.Ref(true)}
	r, err := s.DeleteEnvironment(ctx, DeleteEnvironmentRequestObject{OrgId: "org-id", ProjectId: "proj-id", EnvId: "env-id", Params: params})
	require.NoError(t, err)
	require.IsType(t, DeleteEnvironment202JSONResponse{}, r)

	rec := s.Publisher.(*hmessaging.RecordingPublisher).Messages()
	if assert.Len(t, rec, 1) {
		assert.Equal(t, "io.platform-orchestrator.environment.updated", rec[0].Subject)
		var event events.CloudEvent[genevents.EnvChangedData]
		require.NoError(t, json.Unmarshal(rec[0].Data, &event))
		assert.True(t, ref.DerefOr(event.Data.DeleteRules, false), "deleteRules should be set to true in the event")
	}
}

func TestDeleteEnvironment_successWithOrgFallback(t *testing.T) {
	_, s, cleanup := MockServer(t)
	defer cleanup()
	envUuid := uuid.MustParse("01234567-89ab-cdef-0123-456789abcdef")
	projectUuid := uuid.New()

	db := s.Database.(*mockmodel.MockDatabaser)
	userId := userid.NewHumanUserId()
	mockIamClient := s.IamClient.(*mockorchestratoriam.MockClientWithResponsesInterface)

	// GetProject for authorization check
	db.EXPECT().GetProject(gomock.Any(), nil, "org-id", "proj-id", model.GetModeDefault).Return(&model.Project{
		Uuid: projectUuid,
	}, nil).Times(1)

	// Project-level authorization fails
	projectAuthResp := &orchestratoriam.InternalAuthorizeResponse{
		HTTPResponse: &http.Response{StatusCode: http.StatusForbidden},
	}
	projectDetails := map[string]interface{}{
		"permission": "environment_write",
		"resource":   "project:" + projectUuid.String(),
	}
	projectAuthResp.JSON403 = &orchestratoriam.N403Forbidden{
		Details: &projectDetails,
		Error:   "HTTP-403",
		Message: "forbidden",
	}

	mockIamClient.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
		UserId: userId,
		Checks: []orchestratoriam.ResourcePermissionCheck{projectCheck(projectUuid, PermissionEnvironmentWrite)},
	}).Return(projectAuthResp, nil).Times(1)

	// Fallback to org-level authorization succeeds
	mockIamClient.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
		UserId: userId,
		Checks: []orchestratoriam.ResourcePermissionCheck{orgCheck("org-id", PermissionEnvironmentWrite)},
	}).Return(&orchestratoriam.InternalAuthorizeResponse{
		HTTPResponse: &http.Response{StatusCode: http.StatusNoContent},
	}, nil).Times(1)

	db.EXPECT().GetEnvironment(gomock.Any(), nil, "org-id", "proj-id", "env-id", model.GetModeDefault).Return(&model.Environment{
		Status: model.EnvironmentStatusActive,
		Uuid:   envUuid,
	}, nil).Times(1)
	db.EXPECT().GetEnvironment(gomock.Any(), gomock.Not(nil), "org-id", "proj-id", "env-id", model.GetModeForUpdate).Return(&model.Environment{
		Status: model.EnvironmentStatusActive,
	}, nil).Times(1)
	db.EXPECT().UpdateEnvironment(gomock.Any(), gomock.Not(nil), "org-id", "proj-id", "env-id", gomock.Any()).
		Return(&model.Environment{}, nil)

	ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, userId.String())
	r, err := s.DeleteEnvironment(ctx, DeleteEnvironmentRequestObject{OrgId: "org-id", ProjectId: "proj-id", EnvId: "env-id"})
	require.NoError(t, err)
	require.IsType(t, DeleteEnvironment202JSONResponse{}, r)
}

func TestDeleteEnvironment_forbidden(t *testing.T) {
	_, s, cleanup := MockServer(t)
	defer cleanup()
	projectUuid := uuid.New()

	db := s.Database.(*mockmodel.MockDatabaser)
	userId := userid.NewHumanUserId()
	mockIamClient := s.IamClient.(*mockorchestratoriam.MockClientWithResponsesInterface)

	// GetProject for authorization check
	db.EXPECT().GetProject(gomock.Any(), nil, "org-id", "proj-id", model.GetModeDefault).Return(&model.Project{
		Uuid: projectUuid,
	}, nil).Times(1)

	// Project-level authorization fails
	projectAuthResp := &orchestratoriam.InternalAuthorizeResponse{
		HTTPResponse: &http.Response{StatusCode: http.StatusForbidden},
	}
	projectDetails := map[string]interface{}{
		"permission": "environment_write",
		"resource":   "project:" + projectUuid.String(),
	}
	projectAuthResp.JSON403 = &orchestratoriam.N403Forbidden{
		Details: &projectDetails,
		Error:   "HTTP-403",
		Message: "forbidden",
	}

	mockIamClient.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
		UserId: userId,
		Checks: []orchestratoriam.ResourcePermissionCheck{projectCheck(projectUuid, PermissionEnvironmentWrite)},
	}).Return(projectAuthResp, nil).Times(1)

	// Fallback to org-level authorization also fails
	orgAuthResp := &orchestratoriam.InternalAuthorizeResponse{
		HTTPResponse: &http.Response{StatusCode: http.StatusForbidden},
	}
	orgDetails := map[string]interface{}{
		"permission": "environment_write",
		"resource":   "org:org-id",
	}
	orgAuthResp.JSON403 = &orchestratoriam.N403Forbidden{
		Details: &orgDetails,
		Error:   "HTTP-403",
		Message: "forbidden",
	}

	mockIamClient.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
		UserId: userId,
		Checks: []orchestratoriam.ResourcePermissionCheck{orgCheck("org-id", PermissionEnvironmentWrite)},
	}).Return(orgAuthResp, nil).Times(1)

	ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, userId.String())
	r, err := s.DeleteEnvironment(ctx, DeleteEnvironmentRequestObject{OrgId: "org-id", ProjectId: "proj-id", EnvId: "env-id"})
	require.Error(t, err)
	herr, ok := err.(*herrors.PlatformOrchestratorError)
	require.True(t, ok, "expected *herrors.PlatformOrchestratorError, got %T", err)
	assert.Equal(t, http.StatusForbidden, herr.StatusCode)
	assert.Nil(t, r)
}

func TestInternalForceDeleteEnvironment(t *testing.T) {
	_, s, cleanup := MockServer(t)
	defer cleanup()

	db := s.Database.(*mockmodel.MockDatabaser)
	db.EXPECT().GetEnvironment(gomock.Any(), gomock.Not(nil), "org-id", "proj-id", "env-id", model.GetModeForUpdate).Return(&model.Environment{
		Status: model.EnvironmentStatusActive,
	}, nil)
	db.EXPECT().DeleteEnvironment(gomock.Any(), gomock.Not(nil), "org-id", "proj-id", "env-id").
		Return(nil)

	ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, userid.InternalSystemUuid.String())
	r, err := s.InternalForceDeleteEnvironment(ctx, InternalForceDeleteEnvironmentRequestObject{OrgId: "org-id", ProjectId: "proj-id", EnvId: "env-id"})
	require.NoError(t, err)
	require.IsType(t, InternalForceDeleteEnvironment204Response{}, r)

	rec := s.Publisher.(*hmessaging.RecordingPublisher).Messages()
	if assert.Len(t, rec, 1) {
		assert.Equal(t, "io.platform-orchestrator.environment.deleted", rec[0].Subject)
	}
}

func TestInternalUpdateEnvironment(t *testing.T) {
	_, s, cleanup := MockServer(t)
	defer cleanup()

	db := s.Database.(*mockmodel.MockDatabaser)
	db.EXPECT().UpdateEnvironment(gomock.Any(), gomock.Not(nil), "org-id", "proj-id", "env-id", gomock.Any()).
		Return(&model.Environment{}, nil)

	r, err := s.InternalUpdateEnvironment(t.Context(), InternalUpdateEnvironmentRequestObject{OrgId: "org-id", ProjectId: "proj-id", EnvId: "env-id", Body: &InternalUpdateEnvironmentJSONRequestBody{
		Status: ref.Ref(EnvironmentStatusDeleting), StatusMessage: ref.Ref("we're deleting now"),
	}})
	require.NoError(t, err)
	require.IsType(t, InternalUpdateEnvironment200JSONResponse{}, r)

	rec := s.Publisher.(*hmessaging.RecordingPublisher).Messages()
	if assert.Len(t, rec, 1) {
		assert.Equal(t, "io.platform-orchestrator.environment.updated", rec[0].Subject)
	}
}

func TestCreateEnvironment_success(t *testing.T) {
	_, s, cleanup := MockServer(t)
	defer cleanup()

	userID := userid.NewHumanUserId()
	projectUuid := uuid.New()
	orgUuid := uuid.New()
	envTypeUuid := uuid.New()
	envUuid := uuid.New()

	db := s.Database.(*mockmodel.MockDatabaser)
	mockIamClient := s.IamClient.(*mockorchestratoriam.MockClientWithResponsesInterface)

	// First GetProject call for authorization check (no transaction)
	db.EXPECT().GetProject(gomock.Any(), nil, "org-id", "test-project", model.GetModeDefault).Return(&model.Project{
		Id:      "test-project",
		Uuid:    projectUuid,
		OrgId:   "org-id",
		OrgUuid: orgUuid,
		Status:  model.ProjectStatusActive,
	}, nil).Times(1)

	// Second GetProject call inside CreateEnvironment (with transaction)
	db.EXPECT().GetProject(gomock.Any(), gomock.Not(nil), "org-id", "test-project", model.GetModeDefault).Return(&model.Project{
		Id:      "test-project",
		Uuid:    projectUuid,
		OrgId:   "org-id",
		OrgUuid: orgUuid,
		Status:  model.ProjectStatusActive,
	}, nil).Times(1)

	mockIamClient.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
		UserId: userID,
		Checks: []orchestratoriam.ResourcePermissionCheck{projectCheck(projectUuid, PermissionEnvironmentWrite)},
	}).Return(&orchestratoriam.InternalAuthorizeResponse{
		HTTPResponse: &http.Response{StatusCode: http.StatusNoContent},
	}, nil).Times(1)

	db.EXPECT().GetEnvironmentType(gomock.Any(), gomock.Not(nil), "org-id", "dev", model.GetModeDefault).Return(&model.EnvType{
		Id:      "dev",
		Uuid:    envTypeUuid,
		OrgId:   "org-id",
		OrgUuid: orgUuid,
	}, nil).Times(1)

	db.EXPECT().CreateEnvironment(gomock.Any(), gomock.Not(nil), gomock.Any()).DoAndReturn(
		func(_ context.Context, _ model.Tx, e *model.Environment) (*model.Environment, error) {
			assert.Equal(t, "org-id", e.OrgId)
			assert.Equal(t, orgUuid, e.OrgUuid)
			assert.Equal(t, "test-project", e.ProjectId)
			assert.Equal(t, projectUuid, e.ProjectUuid)
			assert.Equal(t, "test-env", e.Id)
			assert.Equal(t, "dev", e.EnvTypeId)
			assert.Equal(t, envTypeUuid, e.EnvTypeUuid)
			assert.Equal(t, model.EnvironmentStatusActive, e.Status)
			assert.False(t, e.CreatedAt.IsZero())
			assert.False(t, e.UpdatedAt.IsZero())

			e.Uuid = envUuid
			return e, nil
		},
	).Times(1)

	ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, userID.String())
	r, err := s.CreateEnvironment(ctx, CreateEnvironmentRequestObject{
		OrgId:     "org-id",
		ProjectId: "test-project",
		Body: &EnvironmentCreateBody{
			Id:        "test-env",
			EnvTypeId: "dev",
		},
	})

	require.NoError(t, err)
	resp, ok := r.(CreateEnvironment201JSONResponse)
	require.True(t, ok, "expected CreateEnvironment201JSONResponse, got %T", r)
	assert.Equal(t, "test-env", resp.Id)
	assert.Equal(t, envUuid, resp.Uuid)
	assert.Equal(t, "test-project", resp.ProjectId)
	assert.Equal(t, "dev", resp.EnvTypeId)
	assert.Equal(t, EnvironmentStatusActive, resp.Status)
	assert.False(t, resp.CreatedAt.IsZero())
	assert.False(t, resp.UpdatedAt.IsZero())

	rec := s.Publisher.(*hmessaging.RecordingPublisher).Messages()
	if assert.Len(t, rec, 1) {
		assert.Equal(t, string(genevents.IoPlatformOrchestratorEnvironmentCreated), rec[0].Subject)
		var event events.CloudEvent[genevents.EnvChangedData]
		require.NoError(t, json.Unmarshal(rec[0].Data, &event))
		assert.Equal(t, "org-id", event.Data.OrgId)
		assert.Equal(t, orgUuid, event.Data.OrgUuid)
		assert.Equal(t, "test-project", event.Data.ProjectId)
		assert.Equal(t, projectUuid, event.Data.ProjectUuid)
		assert.Equal(t, "test-env", event.Data.EnvId)
		assert.Equal(t, envUuid, event.Data.EnvUuid)
		assert.Equal(t, "dev", event.Data.EnvTypeId)
		assert.Equal(t, envTypeUuid, event.Data.EnvTypeUuid)
		assert.Equal(t, ref.Ref(string(model.EnvironmentStatusActive)), event.Data.Status)
	}
}

func TestCreateEnvironment_forbidden(t *testing.T) {
	_, s, cleanup := MockServer(t)
	defer cleanup()

	userID := userid.NewHumanUserId()
	projectUuid := uuid.New()

	db := s.Database.(*mockmodel.MockDatabaser)
	mockIamClient := s.IamClient.(*mockorchestratoriam.MockClientWithResponsesInterface)

	// GetProject call for authorization check (no transaction)
	db.EXPECT().GetProject(gomock.Any(), nil, "org-id", "test-project", model.GetModeDefault).Return(&model.Project{
		Id:     "test-project",
		Uuid:   projectUuid,
		OrgId:  "org-id",
		Status: model.ProjectStatusActive,
	}, nil).Times(1)

	projectAuthResp := &orchestratoriam.InternalAuthorizeResponse{
		HTTPResponse: &http.Response{StatusCode: http.StatusForbidden},
	}
	projectDetails := map[string]interface{}{
		"permission": "environment_write",
		"resource":   "project:" + projectUuid.String(),
	}
	projectAuthResp.JSON403 = &orchestratoriam.N403Forbidden{
		Details: &projectDetails,
		Error:   "HTTP-403",
		Message: "forbidden",
	}

	mockIamClient.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
		UserId: userID,
		Checks: []orchestratoriam.ResourcePermissionCheck{projectCheck(projectUuid, PermissionEnvironmentWrite)},
	}).Return(projectAuthResp, nil).Times(1)

	// Fallback to org-level authorization also fails
	orgAuthResp := &orchestratoriam.InternalAuthorizeResponse{
		HTTPResponse: &http.Response{StatusCode: http.StatusForbidden},
	}
	orgDetails := map[string]interface{}{
		"permission": "environment_write",
		"resource":   "org:org-id",
	}
	orgAuthResp.JSON403 = &orchestratoriam.N403Forbidden{
		Details: &orgDetails,
		Error:   "HTTP-403",
		Message: "forbidden",
	}

	mockIamClient.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
		UserId: userID,
		Checks: []orchestratoriam.ResourcePermissionCheck{orgCheck("org-id", PermissionEnvironmentWrite)},
	}).Return(orgAuthResp, nil).Times(1)

	ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, userID.String())
	r, err := s.CreateEnvironment(ctx, CreateEnvironmentRequestObject{
		OrgId:     "org-id",
		ProjectId: "test-project",
		Body: &EnvironmentCreateBody{
			Id:        "test-env",
			EnvTypeId: "dev",
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

func TestCreateEnvironment_successWithOrgFallback(t *testing.T) {
	_, s, cleanup := MockServer(t)
	defer cleanup()

	userID := userid.NewHumanUserId()
	projectUuid := uuid.New()
	orgUuid := uuid.New()
	envTypeUuid := uuid.New()
	envUuid := uuid.New()

	db := s.Database.(*mockmodel.MockDatabaser)
	mockIamClient := s.IamClient.(*mockorchestratoriam.MockClientWithResponsesInterface)

	// First GetProject call for authorization check (no transaction)
	db.EXPECT().GetProject(gomock.Any(), nil, "org-id", "test-project", model.GetModeDefault).Return(&model.Project{
		Id:      "test-project",
		Uuid:    projectUuid,
		OrgId:   "org-id",
		OrgUuid: orgUuid,
		Status:  model.ProjectStatusActive,
	}, nil).Times(1)

	// Project-level authorization fails
	projectAuthResp := &orchestratoriam.InternalAuthorizeResponse{
		HTTPResponse: &http.Response{StatusCode: http.StatusForbidden},
	}
	projectDetails := map[string]interface{}{
		"permission": "environment_write",
		"resource":   "project:" + projectUuid.String(),
	}
	projectAuthResp.JSON403 = &orchestratoriam.N403Forbidden{
		Details: &projectDetails,
		Error:   "HTTP-403",
		Message: "forbidden",
	}

	mockIamClient.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
		UserId: userID,
		Checks: []orchestratoriam.ResourcePermissionCheck{projectCheck(projectUuid, PermissionEnvironmentWrite)},
	}).Return(projectAuthResp, nil).Times(1)

	// Fallback to org-level authorization succeeds
	mockIamClient.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
		UserId: userID,
		Checks: []orchestratoriam.ResourcePermissionCheck{orgCheck("org-id", PermissionEnvironmentWrite)},
	}).Return(&orchestratoriam.InternalAuthorizeResponse{
		HTTPResponse: &http.Response{StatusCode: http.StatusNoContent},
	}, nil).Times(1)

	// Second GetProject call inside CreateEnvironment (with transaction)
	db.EXPECT().GetProject(gomock.Any(), gomock.Not(nil), "org-id", "test-project", model.GetModeDefault).Return(&model.Project{
		Id:      "test-project",
		Uuid:    projectUuid,
		OrgId:   "org-id",
		OrgUuid: orgUuid,
		Status:  model.ProjectStatusActive,
	}, nil).Times(1)

	db.EXPECT().GetEnvironmentType(gomock.Any(), gomock.Not(nil), "org-id", "dev", model.GetModeDefault).Return(&model.EnvType{
		Id:      "dev",
		Uuid:    envTypeUuid,
		OrgId:   "org-id",
		OrgUuid: orgUuid,
	}, nil).Times(1)

	db.EXPECT().CreateEnvironment(gomock.Any(), gomock.Not(nil), gomock.Any()).DoAndReturn(
		func(_ context.Context, _ model.Tx, e *model.Environment) (*model.Environment, error) {
			e.Uuid = envUuid
			return e, nil
		},
	).Times(1)

	ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, userID.String())
	r, err := s.CreateEnvironment(ctx, CreateEnvironmentRequestObject{
		OrgId:     "org-id",
		ProjectId: "test-project",
		Body: &EnvironmentCreateBody{
			Id:        "test-env",
			EnvTypeId: "dev",
		},
	})

	require.NoError(t, err)
	resp, ok := r.(CreateEnvironment201JSONResponse)
	require.True(t, ok, "expected CreateEnvironment201JSONResponse, got %T", r)
	assert.Equal(t, "test-env", resp.Id)
	assert.Equal(t, envUuid, resp.Uuid)
}

func TestCreateEnvironment_projectNotFound(t *testing.T) {
	_, s, cleanup := MockServer(t)
	defer cleanup()

	userId := userid.NewHumanUserId()
	db := s.Database.(*mockmodel.MockDatabaser)
	mockIamClient := s.IamClient.(*mockorchestratoriam.MockClientWithResponsesInterface)

	db.EXPECT().GetProject(gomock.Any(), nil, "org-id", "non-existent-project", model.GetModeDefault).
		Return(nil, model.NewErrNotFound("project not found"))

	// Fallback to org-level authorization is attempted even for not-found errors
	mockIamClient.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
		UserId: userId,
		Checks: []orchestratoriam.ResourcePermissionCheck{orgCheck("org-id", PermissionEnvironmentWrite)},
	}).Return(&orchestratoriam.InternalAuthorizeResponse{
		HTTPResponse: &http.Response{StatusCode: http.StatusNoContent},
	}, nil).Times(1)

	ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, userId.String())
	_, err := s.CreateEnvironment(ctx, CreateEnvironmentRequestObject{
		OrgId:     "org-id",
		ProjectId: "non-existent-project",
		Body: &EnvironmentCreateBody{
			Id:        "test-env",
			EnvTypeId: "dev",
		},
	})

	var herr *herrors.PlatformOrchestratorError
	require.ErrorAs(t, err, &herr)

	assert.Equal(t, http.StatusNotFound, herr.StatusCode)
	assert.Equal(t, "project not found", herr.Message)
}

func TestUpdateEnvironment_successWithOrgFallback(t *testing.T) {
	_, s, cleanup := MockServer(t)
	defer cleanup()

	userID := userid.NewHumanUserId()
	projectUuid := uuid.New()
	envUuid := uuid.New()

	db := s.Database.(*mockmodel.MockDatabaser)
	mockIamClient := s.IamClient.(*mockorchestratoriam.MockClientWithResponsesInterface)

	// GetEnvironment for authorization check
	db.EXPECT().GetEnvironment(gomock.Any(), nil, "org-id", "proj-id", "env-id", model.GetModeDefault).Return(&model.Environment{
		ProjectUuid: projectUuid,
		Uuid:        envUuid,
		DisplayName: "Old Name",
	}, nil).Times(1)

	// Env-level authorization fails
	envAuthResp := &orchestratoriam.InternalAuthorizeResponse{
		HTTPResponse: &http.Response{StatusCode: http.StatusForbidden},
	}
	envDetails := map[string]interface{}{
		"permission": "environment_write",
		"resource":   "environment:" + envUuid.String(),
	}
	envAuthResp.JSON403 = &orchestratoriam.N403Forbidden{
		Details: &envDetails,
		Error:   "HTTP-403",
		Message: "forbidden",
	}

	mockIamClient.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
		UserId: userID,
		Checks: []orchestratoriam.ResourcePermissionCheck{environmentCheck(envUuid, PermissionEnvironmentWrite)},
	}).Return(envAuthResp, nil).Times(1)

	// Fallback to org-level authorization succeeds
	mockIamClient.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
		UserId: userID,
		Checks: []orchestratoriam.ResourcePermissionCheck{orgCheck("org-id", PermissionEnvironmentWrite)},
	}).Return(&orchestratoriam.InternalAuthorizeResponse{
		HTTPResponse: &http.Response{StatusCode: http.StatusNoContent},
	}, nil).Times(1)

	db.EXPECT().UpdateEnvironment(gomock.Any(), nil, "org-id", "proj-id", "env-id", gomock.Any()).DoAndReturn(
		func(_ context.Context, _ model.Tx, _, _, _ string, patch *model.EnvironmentPatch) (*model.Environment, error) {
			return &model.Environment{
				ProjectUuid: projectUuid,
				Uuid:        envUuid,
				DisplayName: patch.DisplayName.Must(),
			}, nil
		},
	).Times(1)

	ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, userID.String())
	r, err := s.UpdateEnvironment(ctx, UpdateEnvironmentRequestObject{
		OrgId:     "org-id",
		ProjectId: "proj-id",
		EnvId:     "env-id",
		Body: &EnvironmentUpdateBody{
			DisplayName: "New Name",
		},
	})

	require.NoError(t, err)
	resp, ok := r.(UpdateEnvironment200JSONResponse)
	require.True(t, ok, "expected UpdateEnvironment200JSONResponse, got %T", r)
	assert.Equal(t, "New Name", resp.DisplayName)
}

func TestUpdateEnvironment_forbidden(t *testing.T) {
	_, s, cleanup := MockServer(t)
	defer cleanup()

	userID := userid.NewHumanUserId()
	projectUuid := uuid.New()
	envUuid := uuid.New()

	db := s.Database.(*mockmodel.MockDatabaser)
	mockIamClient := s.IamClient.(*mockorchestratoriam.MockClientWithResponsesInterface)

	// GetEnvironment for authorization check
	db.EXPECT().GetEnvironment(gomock.Any(), nil, "org-id", "proj-id", "env-id", model.GetModeDefault).Return(&model.Environment{
		ProjectUuid: projectUuid,
		Uuid:        envUuid,
		DisplayName: "Old Name",
	}, nil).Times(1)

	// Env-level authorization fails
	envAuthResp := &orchestratoriam.InternalAuthorizeResponse{
		HTTPResponse: &http.Response{StatusCode: http.StatusForbidden},
	}
	envDetails := map[string]interface{}{
		"permission": "environment_write",
		"resource":   "environment:" + envUuid.String(),
	}
	envAuthResp.JSON403 = &orchestratoriam.N403Forbidden{
		Details: &envDetails,
		Error:   "HTTP-403",
		Message: "forbidden",
	}

	mockIamClient.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
		UserId: userID,
		Checks: []orchestratoriam.ResourcePermissionCheck{environmentCheck(envUuid, PermissionEnvironmentWrite)},
	}).Return(envAuthResp, nil).Times(1)

	// Fallback to org-level authorization also fails
	orgAuthResp := &orchestratoriam.InternalAuthorizeResponse{
		HTTPResponse: &http.Response{StatusCode: http.StatusForbidden},
	}
	orgDetails := map[string]interface{}{
		"permission": "environment_write",
		"resource":   "org:org-id",
	}
	orgAuthResp.JSON403 = &orchestratoriam.N403Forbidden{
		Details: &orgDetails,
		Error:   "HTTP-403",
		Message: "forbidden",
	}

	mockIamClient.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
		UserId: userID,
		Checks: []orchestratoriam.ResourcePermissionCheck{orgCheck("org-id", PermissionEnvironmentWrite)},
	}).Return(orgAuthResp, nil).Times(1)

	ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, userID.String())
	r, err := s.UpdateEnvironment(ctx, UpdateEnvironmentRequestObject{
		OrgId:     "org-id",
		ProjectId: "proj-id",
		EnvId:     "env-id",
		Body: &EnvironmentUpdateBody{
			DisplayName: "New Name",
		},
	})

	require.Error(t, err)
	herr, ok := err.(*herrors.PlatformOrchestratorError)
	require.True(t, ok, "expected *herrors.PlatformOrchestratorError, got %T", err)
	assert.Equal(t, http.StatusForbidden, herr.StatusCode)
	assert.Nil(t, r)
}

func TestListInternalEnvironmentsByProjectUuid_success(t *testing.T) {
	_, s, cleanup := MockServer(t)
	defer cleanup()

	projectUuid := uuid.New()
	orgUuid := uuid.New()
	envTypeUuid := uuid.New()
	env1Uuid := uuid.New()
	env2Uuid := uuid.New()

	db := s.Database.(*mockmodel.MockDatabaser)

	expectedEnvs := []model.Environment{
		{
			Id:          "env-1",
			Uuid:        env1Uuid,
			OrgId:       "org-id",
			OrgUuid:     orgUuid,
			ProjectId:   "proj-id",
			ProjectUuid: projectUuid,
			EnvTypeId:   "dev",
			EnvTypeUuid: envTypeUuid,
			DisplayName: "Development",
			Status:      model.EnvironmentStatusActive,
		},
		{
			Id:          "env-2",
			Uuid:        env2Uuid,
			OrgId:       "org-id",
			OrgUuid:     orgUuid,
			ProjectId:   "proj-id",
			ProjectUuid: projectUuid,
			EnvTypeId:   "staging",
			EnvTypeUuid: envTypeUuid,
			DisplayName: "Staging",
			Status:      model.EnvironmentStatusActive,
		},
	}

	db.EXPECT().ListEnvironmentsByProjectUuid(
		gomock.Any(), nil, "org-id", projectUuid, "", 100,
	).Return(expectedEnvs, "", nil).Times(1)

	r, err := s.ListInternalEnvironmentsByProjectUuid(t.Context(), ListInternalEnvironmentsByProjectUuidRequestObject{
		OrgId:       "org-id",
		ProjectUuid: projectUuid,
	})

	require.NoError(t, err)
	resp, ok := r.(ListInternalEnvironmentsByProjectUuid200JSONResponse)
	require.True(t, ok, "expected ListInternalEnvironmentsByProjectUuid200JSONResponse, got %T", r)
	require.Len(t, resp.Items, 2)
	assert.Equal(t, "env-1", resp.Items[0].Id)
	assert.Equal(t, env1Uuid, resp.Items[0].Uuid)
	assert.Equal(t, "proj-id", resp.Items[0].ProjectId)
	assert.Equal(t, projectUuid, *resp.Items[0].ProjectUuid)
	assert.Equal(t, "env-2", resp.Items[1].Id)
	assert.Equal(t, env2Uuid, resp.Items[1].Uuid)
	assert.Nil(t, resp.NextPageToken)
}

func TestListInternalEnvironmentsByProjectUuid_emptyList(t *testing.T) {
	_, s, cleanup := MockServer(t)
	defer cleanup()

	projectUuid := uuid.New()

	db := s.Database.(*mockmodel.MockDatabaser)

	db.EXPECT().ListEnvironmentsByProjectUuid(
		gomock.Any(), nil, "org-id", projectUuid, "", 100,
	).Return([]model.Environment{}, "", nil).Times(1)

	r, err := s.ListInternalEnvironmentsByProjectUuid(t.Context(), ListInternalEnvironmentsByProjectUuidRequestObject{
		OrgId:       "org-id",
		ProjectUuid: projectUuid,
	})

	require.NoError(t, err)
	resp, ok := r.(ListInternalEnvironmentsByProjectUuid200JSONResponse)
	require.True(t, ok, "expected ListInternalEnvironmentsByProjectUuid200JSONResponse, got %T", r)
	assert.Empty(t, resp.Items)
	assert.Nil(t, resp.NextPageToken)
}

func TestListInternalEnvironmentsByProjectUuid_notFound(t *testing.T) {
	_, s, cleanup := MockServer(t)
	defer cleanup()

	projectUuid := uuid.New()

	db := s.Database.(*mockmodel.MockDatabaser)

	db.EXPECT().ListEnvironmentsByProjectUuid(
		gomock.Any(), nil, "org-id", projectUuid, "", 100,
	).Return(nil, "", model.NewErrNotFound("project not found")).Times(1)

	r, err := s.ListInternalEnvironmentsByProjectUuid(t.Context(), ListInternalEnvironmentsByProjectUuidRequestObject{
		OrgId:       "org-id",
		ProjectUuid: projectUuid,
	})

	require.NoError(t, err)
	resp, ok := r.(ListInternalEnvironmentsByProjectUuid404JSONResponse)
	require.True(t, ok, "expected ListInternalEnvironmentsByProjectUuid404JSONResponse, got %T", r)
	assert.Equal(t, "project not found", resp.Message)
}

func TestListInternalEnvironmentsByProjectUuid_withPagination(t *testing.T) {
	_, s, cleanup := MockServer(t)
	defer cleanup()

	projectUuid := uuid.New()
	orgUuid := uuid.New()
	envTypeUuid := uuid.New()
	envUuid := uuid.New()

	db := s.Database.(*mockmodel.MockDatabaser)

	expectedEnvs := []model.Environment{
		{
			Id:          "env-1",
			Uuid:        envUuid,
			OrgId:       "org-id",
			OrgUuid:     orgUuid,
			ProjectId:   "proj-id",
			ProjectUuid: projectUuid,
			EnvTypeId:   "dev",
			EnvTypeUuid: envTypeUuid,
			DisplayName: "Development",
			Status:      model.EnvironmentStatusActive,
		},
	}

	db.EXPECT().ListEnvironmentsByProjectUuid(
		gomock.Any(), nil, "org-id", projectUuid, "page-token", 50,
	).Return(expectedEnvs, "next-page-token", nil).Times(1)

	r, err := s.ListInternalEnvironmentsByProjectUuid(t.Context(), ListInternalEnvironmentsByProjectUuidRequestObject{
		OrgId:       "org-id",
		ProjectUuid: projectUuid,
		Params: ListInternalEnvironmentsByProjectUuidParams{
			Page:    ref.Ref("page-token"),
			PerPage: ref.Ref(50),
		},
	})

	require.NoError(t, err)
	resp, ok := r.(ListInternalEnvironmentsByProjectUuid200JSONResponse)
	require.True(t, ok, "expected ListInternalEnvironmentsByProjectUuid200JSONResponse, got %T", r)
	require.Len(t, resp.Items, 1)
	assert.Equal(t, "env-1", resp.Items[0].Id)
	assert.NotNil(t, resp.NextPageToken)
	assert.Equal(t, "next-page-token", *resp.NextPageToken)
}
