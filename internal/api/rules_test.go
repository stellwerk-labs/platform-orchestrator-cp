package api

import (
	"bytes"
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

func TestRuleCreateBody_validate(t *testing.T) {
	e, s, fin := MockServer(t)
	defer fin()
	for _, tc := range []struct {
		name          string
		body          RuleCreateBody
		expectedError string
	}{
		// unauthorized means it past the validation
		{name: "valid", body: RuleCreateBody{ModuleId: "my-definition"}},
		{name: "valid with res id", body: RuleCreateBody{ModuleId: "my-definition", ResourceId: ref.Ref("workloads.my-workload.my-res")}},
		{name: "bad definition", body: RuleCreateBody{}, expectedError: ": module id must be a valid identifier of alphanumerics and hyphens"},
		{name: "bad class", body: RuleCreateBody{ModuleId: "my-definition", ResourceClass: ref.Ref("bad class")}, expectedError: ": resource class must be a valid identifier of alphanumerics and hyphens"},
		{name: "bad id", body: RuleCreateBody{ModuleId: "my-definition", ResourceId: ref.Ref("bad id")}, expectedError: ": resource id must be a valid resource id with one or more dot-separated parts of lowercase alphanumerics and hyphens"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := s.Database.(*mockmodel.MockDatabaser)
			db.EXPECT().GetModuleDefinition(gomock.Any(), gomock.Any(), "my-org", gomock.Any(), model.GetModeForUpdate).Return(nil, fmt.Errorf("here")).AnyTimes()

			userId := userid.NewHumanUserId()
			mockIamClient := s.IamClient.(*mockorchestratoriam.MockClientWithResponsesInterface)
			mockIamClient.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
				UserId: userId,
				Checks: []orchestratoriam.ResourcePermissionCheck{authz.OrgCheck("my-org", authz.PermissionModuleRuleWrite)},
			}).Return(&orchestratoriam.InternalAuthorizeResponse{
				HTTPResponse: &http.Response{StatusCode: http.StatusNoContent},
			}, nil)

			bod, _ := json.Marshal(tc.body)
			req := httptest.NewRequest(http.MethodPost, "/orgs/my-org/module-rules", bytes.NewReader(bod))
			req.Header.Set("From", userId.String())
			resp := httptest.NewRecorder()
			e.ServeHTTP(resp, req)
			b, _ := io.ReadAll(resp.Result().Body)
			if tc.expectedError == "" {
				require.Equal(t, http.StatusInternalServerError, resp.Result().StatusCode, string(b))
			} else {
				require.Contains(t, string(b), tc.expectedError)
			}
		})
	}
}
