package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/authz"
	orchestratoriam "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"

	mockorchestratoriam "github.com/stellwerk-labs/platform-orchestrator-cp/internal/clients/orchestratoriam/mocks"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	mockmodel "github.com/stellwerk-labs/platform-orchestrator-cp/internal/model/mocks"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
)

func createModuleDef(mod func(*ModuleCreateBody)) ModuleCreateBody {
	x := ModuleCreateBody{
		Id:           "example-definition",
		Description:  ref.Ref("some example definition of mine"),
		ResourceType: "s3",
		ModuleSource: "git::https://github.com/stellwerk-labs/example-tf-module",
		ModuleInputs: map[string]interface{}{
			"thing": "${context.env_id}",
		},
		Dependencies: map[string]ModuleDependencyManifest{
			"thing": {
				Type:  "postgres",
				Class: ref.Ref("some-class"),
				Id:    ref.Ref("some-id"),
				Params: map[string]interface{}{
					"thing": "thing",
				},
			},
		},
		Coprovisioned: []ModuleCoProvisionManifest{
			{
				Type:  "postgres",
				Class: ref.Ref("some-class"),
				Id:    ref.Ref("some-id"),
				Params: map[string]interface{}{
					"thing": "thing",
				},
			},
		},
		ProviderMapping: map[string]string{
			"aws": "aws.default",
		},
	}
	if mod != nil {
		mod(&x)
	}
	return x
}

func TestValidateModuleSource_valid(t *testing.T) {
	for _, tc := range []string{
		"/some/absolute/path",
		"git::ssh://git@github.com/stellwerk-labs/example-tf-module.git?rev=v1",
		"git::https://github.com/stellwerk-labs/example-tf-module",
		"some.registry.com/namespace/name/provider",
		"some.registry.com/namespace/name/provider//some/path",
		"namespace/name/provider",
		"namespace/name/provider//some/path",
	} {
		assert.NoError(t, validateModuleSource(tc, nil), "'%s' should not error", tc)
	}
}

func TestValidateModuleSource_invalid(t *testing.T) {
	for _, tc := range []string{
		"//not a valid path",
		"github.com/stellwerk-labs/example-tf-module.git?rev=v1",
		"https://github.com/stellwerk-labs/example-tf-module",
		"too/many/slashes/in/this/path",
	} {
		assert.Error(t, validateModuleSource(tc, nil), "'%s' should error", tc)
	}
}

func TestDefinitionsValidation_create(t *testing.T) {
	var inlineTF = `variable "foo" {
  type = string
}

output "bar" {
  value = var.foo
}
`

	e, s, fin := MockServer(t)
	defer fin()
	for _, tc := range []struct {
		name          string
		body          CreateModuleJSONRequestBody
		expectedError string
	}{
		// unauthorized means it past the validation
		{name: "valid", body: createModuleDef(nil)},
		{name: "valid with module source code", body: createModuleDef(func(m *ModuleCreateBody) { m.ModuleSource, m.ModuleSourceCode = "inline", ref.Ref(inlineTF) })},
		{name: "bad id", body: createModuleDef(func(m *ModuleCreateBody) { m.Id = "BAD Value" }), expectedError: ": module id must be a valid identifier of alphanumerics and hyphens"},
		{name: "missing resource type", body: createModuleDef(func(m *ModuleCreateBody) { m.ResourceType = "" }), expectedError: `\"/resource_type\": minimum string length is 2"`},
		{name: "empty module source", body: createModuleDef(func(m *ModuleCreateBody) { m.ModuleSource = "" }), expectedError: `\"/module_source\": minimum string length is 2"`},
		{name: "both module source and source code defined", body: createModuleDef(func(m *ModuleCreateBody) { m.ModuleSourceCode = ref.Ref(inlineTF) }), expectedError: `module source code must not be defined when module source is not 'inline'`},
		{name: "bad module source", body: createModuleDef(func(m *ModuleCreateBody) { m.ModuleSource = "£@%£@" }), expectedError: `module source must either be a module registry reference, a git:: repository, or an absolute path available in the runner`},
		{name: "bad module source code", body: createModuleDef(func(m *ModuleCreateBody) { m.ModuleSource, m.ModuleSourceCode = "inline", ref.Ref("{}") }), expectedError: `module source code must have valid hcl syntax`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := s.Database.(*mockmodel.MockDatabaser)
			db.EXPECT().CreateModuleDefinition(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, fmt.Errorf("here")).AnyTimes()
			db.EXPECT().BulkGetResourceTypes(gomock.Any(), gomock.Any(), "my-org", []string{"postgres", "s3"}).Return([]model.ResourceType{{Id: "postgres"}, {Id: "s3"}}, nil).Times(1)
			bod, _ := json.Marshal(tc.body)
			userId := userid.NewHumanUserId()
			mockIamClient := s.IamClient.(*mockorchestratoriam.MockClientWithResponsesInterface)
			mockIamClient.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
				UserId: userId,
				Checks: []orchestratoriam.ResourcePermissionCheck{authz.OrgCheck("my-org", authz.PermissionModuleWrite)},
			}).Return(&orchestratoriam.InternalAuthorizeResponse{
				HTTPResponse: &http.Response{StatusCode: http.StatusNoContent},
			}, nil)

			req := httptest.NewRequest(http.MethodPost, "/orgs/my-org/modules", bytes.NewReader(bod))
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

func updateModuleDef(mod func(body *ModuleUpdateBody)) ModuleUpdateBody {
	x := ModuleUpdateBody{
		Description:  ref.Ref("some other example definition of mine"),
		ModuleSource: ref.Ref("git::https://github.com/stellwerk-labs/example-tf-module"),
		ModuleInputs: &map[string]interface{}{
			"thing": "${context.env_id}",
		},
		Dependencies: &map[string]ModuleDependencyManifest{
			"thing": {
				Type:  "postgres",
				Class: ref.Ref("some-class"),
				Id:    ref.Ref("some-id"),
			},
		},
		ProviderMapping: &map[string]string{
			"aws": "aws.default",
		},
	}
	if mod != nil {
		mod(&x)
	}
	return x
}

func TestDefinitionsValidation_update(t *testing.T) {
	var inlineTF = `variable "foo" {
  type = string
}
`
	e, s, fin := MockServer(t)
	defer fin()
	for _, tc := range []struct {
		name          string
		body          UpdateModuleJSONRequestBody
		existing      model.ModuleDefinitionVersion
		expectedError string
	}{
		// unauthorized means it past the validation
		{
			name:     "valid",
			body:     updateModuleDef(nil),
			existing: model.ModuleDefinitionVersion{ModuleSource: "git::https://github.com/stellwerk-labs/old-tf-module"},
		},
		{
			name:          "missing module source",
			body:          updateModuleDef(func(m *ModuleUpdateBody) { m.ModuleSource = ref.Ref("a") }),
			existing:      model.ModuleDefinitionVersion{ModuleSource: "git::https://github.com/stellwerk-labs/old-tf-module"},
			expectedError: `\"/module_source\": minimum string length is 2"`,
		},
		{
			name:          "bad module source",
			body:          updateModuleDef(func(m *ModuleUpdateBody) { m.ModuleSource = ref.Ref("£@%£@") }),
			existing:      model.ModuleDefinitionVersion{ModuleSource: "git::https://github.com/stellwerk-labs/old-tf-module", ModuleSourceCode: opt.Empty[string]()},
			expectedError: `module source must either be a module registry reference, a git:: repository, or an absolute path available in the runner`,
		},
		{
			name:          "module source already defined, can't have both",
			body:          updateModuleDef(func(m *ModuleUpdateBody) { m.ModuleSourceCode = ref.Ref(inlineTF) }),
			existing:      model.ModuleDefinitionVersion{ModuleSource: "git::https://github.com/stellwerk-labs/old-tf-module"},
			expectedError: `module source code must not be defined when module source is not 'inline'`,
		},
		{
			name:     "valid with module source code",
			body:     updateModuleDef(func(m *ModuleUpdateBody) { m.ModuleSource, m.ModuleSourceCode = ref.Ref("inline"), ref.Ref(inlineTF) }),
			existing: model.ModuleDefinitionVersion{ModuleSource: "inline", ModuleSourceCode: opt.Of(strings.ReplaceAll(inlineTF, "foo", "bar"))},
		},
		{
			name:     "switch from source code to source",
			body:     updateModuleDef(nil),
			existing: model.ModuleDefinitionVersion{ModuleSourceCode: opt.Of(inlineTF)},
		},
		{
			name: "switch from source to source code",
			body: updateModuleDef(func(body *ModuleUpdateBody) {
				body.ModuleSourceCode, body.ModuleSource = ref.Ref(inlineTF), ref.Ref("inline")
			}),
			existing: model.ModuleDefinitionVersion{ModuleSource: "git::https://github.com/stellwerk-labs/old-tf-module"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := s.Database.(*mockmodel.MockDatabaser)
			db.EXPECT().GetModuleDefinition(gomock.Any(), gomock.Any(), "my-org", "some-def", model.GetModeForUpdate).Return(&tc.existing, nil).AnyTimes()
			db.EXPECT().BulkGetResourceTypes(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, errors.New("error")).AnyTimes()

			bod, _ := json.Marshal(tc.body)
			userId := userid.NewHumanUserId()
			mockIamClient := s.IamClient.(*mockorchestratoriam.MockClientWithResponsesInterface)
			mockIamClient.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
				UserId: userId,
				Checks: []orchestratoriam.ResourcePermissionCheck{authz.OrgCheck("my-org", authz.PermissionModuleWrite)},
			}).Return(&orchestratoriam.InternalAuthorizeResponse{
				HTTPResponse: &http.Response{StatusCode: http.StatusNoContent},
			}, nil)

			req := httptest.NewRequest(http.MethodPatch, "/orgs/my-org/modules/some-def", bytes.NewReader(bod))
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

func TestValidateModuleInputsAndParamInputs(t *testing.T) {
	for _, tc := range []struct {
		name          string
		inputKeys     []string
		paramKeys     []string
		expectedError string
	}{
		{"no error with empty input and param keys", []string{}, []string{}, ""},
		{"no error with non-overlapping keys", []string{"aa"}, []string{"bb"}, ""},
		{"no error with upper case keys", []string{"AA"}, []string{"BB"}, ""},
		{"error with overlapping keys", []string{"aa"}, []string{"aa"}, "module inputs and module parameters cannot have the same key: aa"},
		{"error with invalid input key", []string{"foo bar"}, []string{"foo-bar"}, "module inputs: 'foo bar' is not a valid identifier"},
		{"error with invalid param key", []string{"foo-bar"}, []string{"foo bar"}, "module parameters: 'foo bar' is not a valid identifier"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateModuleInputsAndParamInputs(slices.Values(tc.inputKeys), slices.Values(tc.paramKeys)); tc.expectedError == "" {
				require.NoError(t, err)
			} else {
				require.EqualError(t, err, tc.expectedError)
			}
		})
	}
}
