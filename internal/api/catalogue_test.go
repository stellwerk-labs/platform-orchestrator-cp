package api

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	mockmodel "github.com/stellwerk-labs/platform-orchestrator-cp/internal/model/mocks"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
)

func TestGenerateInternalModuleCatalogue_empty(t *testing.T) {
	_, s, fin := MockServer(t)
	defer fin()

	mdb := s.Database.(*mockmodel.MockDatabaser)
	mdb.EXPECT().GetEnvironment(gomock.Any(), gomock.Not(gomock.Nil()), "my-org", "my-project", "my-env", model.GetModeDefault).Return(&model.Environment{ProjectId: "my-project", Id: "my-env", EnvTypeId: "dev"}, nil)
	mdb.EXPECT().ListModuleRules(gomock.Any(), gomock.Not(gomock.Nil()), "my-org", "", getModuleCataloguePaginationSize, model.ListModuleRulesParams{
		EffectiveInProjectId: ref.Ref("my-project"),
		EffectiveInEnvTypeId: ref.Ref("dev"),
		EffectiveInEnvId:     ref.Ref("my-env"),
	}).
		Return(nil, "", nil)

	resp, err := s.GenerateInternalModuleCatalogue(t.Context(), GenerateInternalModuleCatalogueRequestObject{
		OrgId:     "my-org",
		ProjectId: "my-project",
		EnvId:     "my-env",
	})
	require.NoError(t, err)
	assert.Equal(t, GenerateInternalModuleCatalogue200JSONResponse(InternalModuleCatalogue{
		Modules:   make([]InternalModuleCatalogueModule, 0),
		Providers: make([]ModuleProvider, 0),
	}), resp.(GenerateInternalModuleCatalogue200JSONResponse))
}

func TestGenerateInternalModuleCatalogue_full(t *testing.T) {
	_, s, fin := MockServer(t)
	defer fin()

	mdb := s.Database.(*mockmodel.MockDatabaser)
	mdb.EXPECT().GetEnvironment(gomock.Any(), gomock.Not(gomock.Nil()), "my-org", "my-project", "my-env", model.GetModeDefault).Return(&model.Environment{ProjectId: "my-project", Id: "my-env", EnvTypeId: "dev"}, nil)
	mdb.EXPECT().ListModuleRules(gomock.Any(), gomock.Not(gomock.Nil()), "my-org", "", getModuleCataloguePaginationSize, model.ListModuleRulesParams{
		EffectiveInProjectId: ref.Ref("my-project"),
		EffectiveInEnvTypeId: ref.Ref("dev"),
		EffectiveInEnvId:     ref.Ref("my-env"),
	}).
		Return([]model.DefinitionRule{{Id: uuid.NewMD5(uuid.Nil, []byte{0}), DefinitionId: "def-0", ResourceClass: "default", ResourceType: "my-type", ProjectId: opt.Of("my-project"), EnvId: opt.Of("my-env")}}, "next-page", nil)
	mdb.EXPECT().ListModuleRules(gomock.Any(), gomock.Not(gomock.Nil()), "my-org", "next-page", getModuleCataloguePaginationSize, model.ListModuleRulesParams{
		EffectiveInProjectId: ref.Ref("my-project"),
		EffectiveInEnvTypeId: ref.Ref("dev"),
		EffectiveInEnvId:     ref.Ref("my-env"),
	}).
		Return([]model.DefinitionRule{
			{Id: uuid.NewMD5(uuid.Nil, []byte{1}), DefinitionId: "def-1", ResourceType: "my-type", ResourceClass: "default"},
			{Id: uuid.NewMD5(uuid.Nil, []byte{2}), DefinitionId: "def-1", ResourceType: "my-type", ResourceClass: "non-default", ResourceId: opt.Of("my-res")},
			{Id: uuid.NewMD5(uuid.Nil, []byte{4}), DefinitionId: "def-2", ResourceType: "my-type", ResourceClass: "default", ProjectId: opt.Of("my-project")},
			{Id: uuid.NewMD5(uuid.Nil, []byte{5}), DefinitionId: "def-3", ResourceType: "my-type", ResourceClass: "default", ProjectId: opt.Of("my-project"), EnvTypeId: opt.Of("dev")},
		}, "", nil)

	mdb.EXPECT().ListModuleDefinitions(gomock.Any(), gomock.Not(gomock.Nil()), "my-org", "", getModuleCataloguePaginationSize, model.ListModuleDefinitionsParams{
		ByDefinitionIds: []string{"def-0", "def-1"},
	}).Return([]model.ModuleDefinitionVersion{
		{
			DefinitionId: "def-0", ResourceType: "my-type", ModuleSource: "/modules/my-s3", ProviderMapping: map[string]string{"aws": "aws.my-prov"},
		},
		{
			DefinitionId: "def-1", ResourceType: "my-type", ModuleSource: "/modules/my-s3", ProviderMapping: map[string]string{"aws": "google.my-google"},
			ModuleParams: map[string]model.ModuleParam{"animal": {Type: "string", IsOptional: true, Description: "animal description"}},
			Dependencies: map[string]model.ModuleDefinitionDependency{
				"thing": {Type: "x", Class: opt.Of("default"), Id: opt.Of("y"), Params: map[string]interface{}{"thing": "thing"}},
			},
			CoProvisioned: []model.ModuleDefinitionCoProvision{
				{Type: "y", Class: opt.Of("default"), Id: opt.Of("y"), Params: map[string]interface{}{"thing": "thing"}},
			},
		},
	}, "", nil)

	mdb.EXPECT().BulkGetModuleDefinitionVersions(gomock.Any(), gomock.Not(gomock.Nil()), "my-org", []string{"my-def", "def-0"}, []string{"some-version", "old-version"}).
		Return([]model.ModuleDefinitionVersion{
			{DefinitionId: "my-def", VersionId: "some-version"},
			{DefinitionId: "def-0", VersionId: "old-version"},
		}, nil)

	mdb.EXPECT().ListModuleProviders(gomock.Any(), gomock.Not(gomock.Nil()), "my-org", "", getModuleCataloguePaginationSize, model.ListModuleProvidersParams{
		ByProviderIds: []string{"aws.my-prov", "aws.other-prov", "google.my-google"},
	}).Return([]model.ModuleProvider{
		{ProviderType: "aws", Id: "my-prov"},
		{ProviderType: "aws", Id: "other-prov"},
		{ProviderType: "google", Id: "my-google"},
	}, "", nil)

	resp, err := s.GenerateInternalModuleCatalogue(t.Context(), GenerateInternalModuleCatalogueRequestObject{
		OrgId:     "my-org",
		ProjectId: "my-project",
		EnvId:     "my-env",
		Body: &InternalModuleCatalogueGenerateBody{
			PinnedModuleVersions: []string{
				"my-def@some-version",
				"def-0@old-version",
				"def-1@",
			},
			PinnedProviders: []string{
				"aws.other-prov",
			},
		},
	})
	require.NoError(t, err)
	require.IsType(t, GenerateInternalModuleCatalogue200JSONResponse{}, resp, resp)
	assert.Equal(t, GenerateInternalModuleCatalogue200JSONResponse(InternalModuleCatalogue{
		Modules: []InternalModuleCatalogueModule{
			{
				Id: "def-0", ResourceType: "my-type", ModuleSource: "/modules/my-s3", ProviderMapping: map[string]string{"aws": "aws.my-prov"},
				ModuleParams:  map[string]ModuleParamItem{},
				Dependencies:  map[string]ModuleDependencyManifest{},
				Coprovisioned: []ModuleCoProvisionManifest{},
				Rules: []InternalModuleCatalogueModuleRule{
					{
						RuleId:        uuid.NewMD5(uuid.Nil, []byte{0}),
						ResourceClass: "default",
						ProjectId:     ref.Ref("my-project"),
						EnvId:         ref.Ref("my-env"),
					},
				},
			},
			{
				Id: "def-1", ResourceType: "my-type", ModuleSource: "/modules/my-s3", ProviderMapping: map[string]string{"aws": "google.my-google"},
				ModuleParams: map[string]ModuleParamItem{
					"animal": {Type: String, IsOptional: true},
				},
				Dependencies: map[string]ModuleDependencyManifest{
					"thing": {Type: "x", Class: ref.Ref("default"), Id: ref.Ref("y"), Params: map[string]interface{}{"thing": "thing"}},
				},
				Coprovisioned: []ModuleCoProvisionManifest{
					{Type: "y", Class: ref.Ref("default"), Id: ref.Ref("y"), Params: map[string]interface{}{"thing": "thing"}},
				},
				Rules: []InternalModuleCatalogueModuleRule{
					{
						RuleId:        uuid.NewMD5(uuid.Nil, []byte{2}),
						ResourceClass: "non-default",
						ResourceId:    ref.Ref("my-res"),
					},
				},
			},
			{Id: "my-def", VersionId: "some-version", Dependencies: map[string]ModuleDependencyManifest{}, Coprovisioned: []ModuleCoProvisionManifest{}, ModuleParams: map[string]ModuleParamItem{}},
			{
				Id: "def-0", VersionId: "old-version", Dependencies: map[string]ModuleDependencyManifest{}, Coprovisioned: []ModuleCoProvisionManifest{}, ModuleParams: map[string]ModuleParamItem{},
			},
		},
		Providers: []ModuleProvider{
			{ProviderType: "aws", Id: "my-prov"},
			{ProviderType: "aws", Id: "other-prov"},
			{ProviderType: "google", Id: "my-google"},
		},
	}), resp.(GenerateInternalModuleCatalogue200JSONResponse))
}
