package model

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestNativeTargetShapeRefusesBeforeDatabaseAccess(t *testing.T) {
	base := NativeExecutionTargetReference{RunnerID: "native_pool", RunnerUUID: uuid.New(), RuntimeVersion: "stellwerk.runner.rollout/v2", PipelineVersion: "stellwerk.pipeline/v2", ToolsDigest: strings.Repeat("a", 64), StateBackendType: "s3", StateConfigurationDigest: strings.Repeat("b", 64)}
	admission := DeploymentAdmission{OrgID: "org", AdmissionID: uuid.New(), ProjectUUID: uuid.New(), EnvironmentUUID: uuid.New(), ActorID: uuid.New(), RequestDigest: strings.Repeat("c", 64), Mode: "deploy", ModuleVersions: map[string]uuid.UUID{}, NativeTarget: &base}
	require.NoError(t, validateDeploymentAdmission(admission))
	maximum := base
	maximum.RunnerID = strings.Repeat("a", 128)
	admission.NativeTarget = &maximum
	require.NoError(t, validateDeploymentAdmission(admission))
	admission.NativeTarget = &base
	for _, change := range []struct {
		name string
		run  func(*NativeExecutionTargetReference)
	}{
		{"missing slug", func(r *NativeExecutionTargetReference) { r.RunnerID = "" }},
		{"oversize slug", func(r *NativeExecutionTargetReference) { r.RunnerID = strings.Repeat("a", 129) }},
		{"single character slug", func(r *NativeExecutionTargetReference) { r.RunnerID = "a" }},
		{"dotted resource ID", func(r *NativeExecutionTargetReference) { r.RunnerID = "pool.other" }},
		{"uppercase slug", func(r *NativeExecutionTargetReference) { r.RunnerID = "Pool" }},
		{"nil UUID", func(r *NativeExecutionTargetReference) { r.RunnerUUID = uuid.Nil }},
		{"old runtime", func(r *NativeExecutionTargetReference) { r.RuntimeVersion = "stellwerk.runner.rollout/v1" }},
		{"old pipeline", func(r *NativeExecutionTargetReference) { r.PipelineVersion = "stellwerk.pipeline/v1" }},
		{"unknown backend", func(r *NativeExecutionTargetReference) { r.StateBackendType = "future" }},
		{"missing tools digest", func(r *NativeExecutionTargetReference) { r.ToolsDigest = "" }},
		{"uppercase tools digest", func(r *NativeExecutionTargetReference) { r.ToolsDigest = strings.Repeat("A", 64) }},
		{"malformed tools digest", func(r *NativeExecutionTargetReference) { r.ToolsDigest = strings.Repeat("x", 64) }},
		{"missing state digest", func(r *NativeExecutionTargetReference) { r.StateConfigurationDigest = "" }},
		{"uppercase state digest", func(r *NativeExecutionTargetReference) { r.StateConfigurationDigest = strings.Repeat("B", 64) }},
		{"malformed state digest", func(r *NativeExecutionTargetReference) { r.StateConfigurationDigest = strings.Repeat("x", 64) }},
	} {
		t.Run(change.name, func(t *testing.T) {
			target := base
			change.run(&target)
			request := admission
			request.NativeTarget = &target
			_, err := AuthorizeDeploymentAdmission(t.Context(), nil, request)
			_, invalid := IsErrBadRequest(err)
			require.True(t, invalid, "invalid target must refuse before accessing nil database: %v", err)
			require.ErrorContains(t, err, "native target")
		})
	}
}
