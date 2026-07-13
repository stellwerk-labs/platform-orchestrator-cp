package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/authz"
	orchestratoriam "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	mockorchestratoriam "github.com/stellwerk-labs/platform-orchestrator-cp/internal/clients/orchestratoriam/mocks"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	mockmodel "github.com/stellwerk-labs/platform-orchestrator-cp/internal/model/mocks"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
)

func TestRunnerRuleCreateBody_checkRunnerRuleBody(t *testing.T) {
	e, s, fin := MockServer(t)
	defer fin()
	for _, tc := range []struct {
		name                               string
		body                               RunnerRuleCreateBody
		expectedCreatedRunnerRuleProjectId *string
		expectedCreatedRunnerRuleEnvTypeId *string
	}{
		{name: "only runner", body: RunnerRuleCreateBody{RunnerId: "my-runner"}},
		{name: "runner - blank project id", body: RunnerRuleCreateBody{RunnerId: "my-runner", ProjectId: ref.Ref("")}},
		{name: "runner - blank project id / env type id specified", body: RunnerRuleCreateBody{RunnerId: "my-runner", ProjectId: ref.Ref(""), EnvTypeId: ref.Ref("my-env-type")}, expectedCreatedRunnerRuleEnvTypeId: ref.Ref("my-env-type")},
		{name: "runner - project id specified", body: RunnerRuleCreateBody{RunnerId: "my-runner", ProjectId: ref.Ref("my-project")}, expectedCreatedRunnerRuleProjectId: ref.Ref("my-project")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := s.Database.(*mockmodel.MockDatabaser)
			db.EXPECT().CreateRunnerRule(gomock.Any(), gomock.Any(), "my-org", gomock.Any()).DoAndReturn(
				func(ctx context.Context, tx model.Tx, orgId string, request *model.RunnerRule) (*model.RunnerRule, error) {
					if ref.DerefOr(tc.expectedCreatedRunnerRuleEnvTypeId, "") == "" {
						require.False(t, request.EnvTypeId.IsSet())
					} else {
						require.Equal(t, *tc.expectedCreatedRunnerRuleEnvTypeId, request.EnvTypeId.Must())
					}
					if ref.DerefOr(tc.expectedCreatedRunnerRuleProjectId, "") == "" {
						require.False(t, request.ProjectId.IsSet())
					} else {
						require.Equal(t, *tc.expectedCreatedRunnerRuleProjectId, request.ProjectId.Must())
					}
					return nil, fmt.Errorf("here")
				}).Times(1)
			userId := userid.NewHumanUserId()
			mockIamClient := s.IamClient.(*mockorchestratoriam.MockClientWithResponsesInterface)
			mockIamClient.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
				UserId: userId,
				Checks: []orchestratoriam.ResourcePermissionCheck{authz.CanManageOrgCheck("my-org")},
			}).Return(&orchestratoriam.InternalAuthorizeResponse{
				HTTPResponse: &http.Response{StatusCode: http.StatusNoContent},
			}, nil)

			bod, _ := json.Marshal(tc.body)
			req := httptest.NewRequest(http.MethodPost, "/orgs/my-org/runner-rules", bytes.NewReader(bod))
			req.Header.Set("From", userId.String())
			resp := httptest.NewRecorder()
			e.ServeHTTP(resp, req)
			b, _ := io.ReadAll(resp.Result().Body)
			require.Equal(t, http.StatusInternalServerError, resp.Result().StatusCode, string(b))
		})
	}
}

func TestCreateRunnerRuleInOrg_no_runner_id(t *testing.T) {
	e, s, fin := MockServer(t)
	defer fin()

	userId := userid.NewHumanUserId()
	mockIamClient := s.IamClient.(*mockorchestratoriam.MockClientWithResponsesInterface)
	mockIamClient.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
		UserId: userId,
		Checks: []orchestratoriam.ResourcePermissionCheck{authz.CanManageOrgCheck("my-org")},
	}).Return(&orchestratoriam.InternalAuthorizeResponse{
		HTTPResponse: &http.Response{StatusCode: http.StatusNoContent},
	}, nil)

	bod, _ := json.Marshal(RunnerRuleCreateBody{})
	req := httptest.NewRequest(http.MethodPost, "/orgs/my-org/runner-rules", bytes.NewReader(bod))
	req.Header.Set("From", userId.String())
	resp := httptest.NewRecorder()
	e.ServeHTTP(resp, req)
	b, _ := io.ReadAll(resp.Result().Body)
	require.Equal(t, http.StatusBadRequest, resp.Result().StatusCode, string(b))
	require.Contains(t, string(b), "runner_id is mandatory to create a runner rule")
}
