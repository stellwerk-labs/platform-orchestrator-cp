package api

import (
	"context"
	"testing"

	"github.com/stellwerk-labs/golib/hecho"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	mockmodel "github.com/stellwerk-labs/platform-orchestrator-cp/internal/model/mocks"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
)

func TestListAvailableResourceTypes_empty(t *testing.T) {
	_, s, fin := MockServer(t)
	defer fin()
	s.Database.(*mockmodel.MockDatabaser).EXPECT().ListAvailableResourceTypes(
		gomock.Any(), gomock.Nil(), "org-id", "proj-id", "env-id", "", 100,
		model.ListAvailableResourceTypeParams{},
	).Return(nil, "", nil)
	ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, userid.InternalSystemUuid.String())
	r, err := s.ListAvailableResourceTypes(ctx, ListAvailableResourceTypesRequestObject{
		OrgId: "org-id", ProjectId: "proj-id", EnvId: "env-id",
	})
	require.NoError(t, err)
	assert.Equal(t, ListAvailableResourceTypes200JSONResponse{Items: make([]AvailableResourceType, 0)}, r)
}

func TestListAvailableResourceTypes_one_paginated(t *testing.T) {
	_, s, fin := MockServer(t)
	defer fin()
	s.Database.(*mockmodel.MockDatabaser).EXPECT().ListAvailableResourceTypes(
		gomock.Any(), gomock.Nil(), "org-id", "proj-id", "env-id", "", 100,
		model.ListAvailableResourceTypeParams{},
	).Return([]model.AvailableResourceType{
		{
			Id:            "my-res-type",
			Description:   "my-desc",
			OutputsSchema: map[string]interface{}{"type": "object"},
			Options: []model.Option{
				{ResourceClass: "default", ProjectId: ref.Ref("proj-id"), EnvId: ref.Ref("env-id"), DefinitionId: "def-a", ModuleParams: map[string]model.ModuleParam{"fruit": {}}, RuleId: "my-rule"},
				{ResourceClass: "default", EnvTypeId: ref.Ref("env-type-id"), DefinitionId: "def-b"},
				{ResourceClass: "default", ProjectId: ref.Ref("proj-id"), DefinitionId: "def-c"},
				{ResourceClass: "custom", ResourceId: "special", ProjectId: ref.Ref("proj-id"), DefinitionId: "def-d", RuleId: "my-other-rule"},
			},
		},
	}, "next-page", nil)
	ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, userid.InternalSystemUuid.String())
	r, err := s.ListAvailableResourceTypes(ctx, ListAvailableResourceTypesRequestObject{
		OrgId: "org-id", ProjectId: "proj-id", EnvId: "env-id",
	})
	require.NoError(t, err)
	assert.Equal(t, ListAvailableResourceTypes200JSONResponse{
		Items: []AvailableResourceType{
			{
				Description:  ref.Ref("my-desc"),
				Id:           "my-res-type",
				OutputSchema: map[string]interface{}{"type": "object"},
				Options: []AvailableResourceTypeOption{
					{
						ResourceClass: "custom",
						ResourceId:    ref.Ref("special"),
						RuleId:        "my-other-rule",
						ModuleId:      "def-d",
						ModuleParams:  map[string]ModuleParamItem{},
					},
					{
						ResourceClass: "default",
						RuleId:        "my-rule",
						ModuleId:      "def-a",
						ModuleParams:  map[string]ModuleParamItem{"fruit": {}},
					},
				},
			},
		},
		NextPageToken: ref.Ref("next-page"),
	}, r)
}
