//go:build integration

package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/hecho"
	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/events"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/genevents"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestDeletionPolicyDeleteTransactionPersistsMonotoneOptionsAndOutbox(t *testing.T) {
	db, grant := pluginDeploymentBoundaryDatabase(t)
	org := grant.OrgID
	// The first request is rolled back: neither desired state nor its outbox survives.
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, messages, err := DeleteEnvironmentAndCreateMessage(t.Context(), db, tx, org, "project", "development", DeleteEnvOptions{Force: true, DeleteRules: ref.Ref(true)})
	require.NoError(t, err)
	require.Len(t, messages, 1)
	require.NoError(t, tx.Rollback())
	env, err := db.GetEnvironment(t.Context(), nil, org, "project", "development", model.GetModeDefault)
	require.NoError(t, err)
	require.Nil(t, env.DeletionForce)
	require.Nil(t, env.DeletionDeleteRules)
	require.Nil(t, envFromDbModel(env).DeletionPolicy)
	for _, opts := range []DeleteEnvOptions{{DeleteRules: ref.Ref(true)}, {Force: true}, {}, {DeleteRules: ref.Ref(false)}} {
		tx, err = db.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		env, messages, err = DeleteEnvironmentAndCreateMessage(t.Context(), db, tx, org, "project", "development", opts)
		require.NoError(t, err)
		require.Len(t, messages, 1)
		var event events.CloudEvent[genevents.EnvChangedData]
		require.NoError(t, json.Unmarshal(messages[0].Payload, &event))
		require.Equal(t, env.DeletionForce, event.Data.Force)
		require.Equal(t, env.DeletionDeleteRules, event.Data.DeleteRules)
		require.NoError(t, tx.Commit())
	}
	fresh, err := db.GetEnvironmentByUuid(t.Context(), nil, org, env.Uuid, model.GetModeDefault)
	require.NoError(t, err)
	require.True(t, *fresh.DeletionForce)
	require.True(t, *fresh.DeletionDeleteRules)
	require.Equal(t, &EnvironmentDeletionPolicy{Force: true, DeleteRules: true}, envFromDbModel(fresh).DeletionPolicy)
	s := &Server{Database: db, Logger: zap.NewNop()}
	for _, status := range []EnvironmentStatus{EnvironmentStatusActive, EnvironmentStatusDeleteFailed} {
		response, err := s.InternalUpdateEnvironment(t.Context(), InternalUpdateEnvironmentRequestObject{OrgId: org, ProjectId: "project", EnvId: "development", Body: &InternalUpdateEnvironmentJSONRequestBody{Status: &status}})
		require.NoError(t, err)
		require.IsType(t, InternalUpdateEnvironment409JSONResponse{}, response)
	}
}

func TestDeletionPolicyFinalDeleteUsesCanonicalRulesAndExactLifecycle(t *testing.T) {
	for _, deleteRules := range []bool{false, true} {
		t.Run(map[bool]string{false: "retain-rules", true: "remove-rules"}[deleteRules], func(t *testing.T) {
			db, grant := pluginDeploymentBoundaryDatabase(t)
			ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, userid.InternalSystemUuid.String())
			tx, err := db.BeginTx(ctx, nil)
			require.NoError(t, err)
			rule, err := db.CreateModuleRule(ctx, tx, grant.OrgID, &model.DefinitionRule{DefinitionId: "module", ResourceType: "generic", ProjectId: opt.Of("project"), EnvId: opt.Of("development"), CreatedAt: time.Now()})
			require.NoError(t, err)
			env, _, err := DeleteEnvironmentAndCreateMessage(ctx, db, tx, grant.OrgID, "project", "development", DeleteEnvOptions{Force: true, DeleteRules: &deleteRules})
			require.NoError(t, err)
			require.NoError(t, tx.Commit())
			s := &Server{Database: db, Logger: zap.NewNop(), Publisher: &hmessaging.RecordingPublisher{}}
			request := InternalForceDeleteEnvironmentRequestObject{OrgId: grant.OrgID, ProjectId: "project", EnvId: "development", Params: InternalForceDeleteEnvironmentParams{ExpectedEnvUuid: ref.Ref(uuid.New()), DeleteRules: ref.Ref(!deleteRules)}}
			response, err := s.InternalForceDeleteEnvironment(ctx, request)
			require.NoError(t, err)
			require.IsType(t, InternalForceDeleteEnvironment409JSONResponse{}, response)
			_, err = db.GetEnvironment(ctx, nil, env.OrgId, env.ProjectId, env.Id, model.GetModeDefault)
			require.NoError(t, err)
			request.Params.ExpectedEnvUuid = &env.Uuid
			response, err = s.InternalForceDeleteEnvironment(ctx, request)
			require.NoError(t, err)
			require.IsType(t, InternalForceDeleteEnvironment204Response{}, response)
			rules, _, err := db.ListModuleRules(ctx, nil, grant.OrgID, "", 20, model.ListModuleRulesParams{})
			require.NoError(t, err)
			if deleteRules {
				require.Empty(t, rules)
			} else {
				require.Len(t, rules, 1)
				require.Equal(t, rule.Id, rules[0].Id)
			}
			recreated, err := db.CreateEnvironment(ctx, nil, &model.Environment{OrgId: env.OrgId, OrgUuid: env.OrgUuid, ProjectId: env.ProjectId, ProjectUuid: env.ProjectUuid, Id: env.Id, EnvTypeId: env.EnvTypeId, EnvTypeUuid: env.EnvTypeUuid, Status: model.EnvironmentStatusActive, CreatedAt: time.Now(), UpdatedAt: time.Now()})
			require.NoError(t, err)
			require.NotEqual(t, env.Uuid, recreated.Uuid)
			response, err = s.InternalForceDeleteEnvironment(ctx, request)
			require.NoError(t, err)
			require.IsType(t, InternalForceDeleteEnvironment409JSONResponse{}, response)
			current, err := db.GetEnvironment(ctx, nil, env.OrgId, env.ProjectId, env.Id, model.GetModeDefault)
			require.NoError(t, err)
			require.Equal(t, recreated.Uuid, current.Uuid)
		})
	}
}

func TestDeletionPolicyBoundFinalDeleteRefusesLegacyUnknown(t *testing.T) {
	db, grant := pluginDeploymentBoundaryDatabase(t)
	ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, userid.InternalSystemUuid.String())
	env, err := db.GetEnvironment(ctx, nil, grant.OrgID, "project", "development", model.GetModeDefault)
	require.NoError(t, err)
	s := &Server{Database: db, Logger: zap.NewNop()}
	response, err := s.InternalForceDeleteEnvironment(ctx, InternalForceDeleteEnvironmentRequestObject{OrgId: env.OrgId, ProjectId: env.ProjectId, EnvId: env.Id, Params: InternalForceDeleteEnvironmentParams{ExpectedEnvUuid: &env.Uuid}})
	require.NoError(t, err)
	require.IsType(t, InternalForceDeleteEnvironment409JSONResponse{}, response)
}

func TestDeletionPolicyLegacyUnboundDeletionRequiresExplicitRetry(t *testing.T) {
	db, grant := pluginDeploymentBoundaryDatabase(t)
	ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, userid.InternalSystemUuid.String())
	s := &Server{Database: db, Logger: zap.NewNop(), Publisher: &hmessaging.RecordingPublisher{}}
	request := InternalForceDeleteEnvironmentRequestObject{OrgId: grant.OrgID, ProjectId: "project", EnvId: "development"}
	for _, status := range []model.EnvironmentStatus{model.EnvironmentStatusDeleting, model.EnvironmentStatusDeleteFailed} {
		_, err := db.UpdateEnvironment(ctx, nil, grant.OrgID, "project", "development", &model.EnvironmentPatch{Status: opt.Of(status), UpdatedAt: time.Now()})
		require.NoError(t, err)
		response, err := s.InternalForceDeleteEnvironment(ctx, request)
		require.NoError(t, err)
		require.IsType(t, InternalForceDeleteEnvironment409JSONResponse{}, response)
	}
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, _, err = DeleteEnvironmentAndCreateMessage(ctx, db, tx, grant.OrgID, "project", "development", DeleteEnvOptions{})
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	response, err := s.InternalForceDeleteEnvironment(ctx, request)
	require.NoError(t, err)
	require.IsType(t, InternalForceDeleteEnvironment204Response{}, response)
}

func TestDeletionPolicyLegacyActiveUnboundInternalEscapeHatch(t *testing.T) {
	db, grant := pluginDeploymentBoundaryDatabase(t)
	ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, userid.InternalSystemUuid.String())
	s := &Server{Database: db, Logger: zap.NewNop(), Publisher: &hmessaging.RecordingPublisher{}}
	response, err := s.InternalForceDeleteEnvironment(ctx, InternalForceDeleteEnvironmentRequestObject{OrgId: grant.OrgID, ProjectId: "project", EnvId: "development"})
	require.NoError(t, err)
	require.IsType(t, InternalForceDeleteEnvironment204Response{}, response)
}
