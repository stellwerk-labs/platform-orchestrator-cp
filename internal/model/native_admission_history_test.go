package model

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestNativeWatermarkAndPlanRefuseMalformedAuthorityBeforeDatabaseAccess(t *testing.T) {
	base := DeploymentAdmission{
		OrgID: "org", AdmissionID: uuid.New(), ProjectUUID: uuid.New(), EnvironmentUUID: uuid.New(), ActorID: uuid.New(),
		Mode: "deploy", RequestDigest: strings.Repeat("a", 64), ModuleVersions: map[string]uuid.UUID{},
		NativeTarget: &NativeExecutionTargetReference{RunnerID: "native_pool", RunnerUUID: uuid.New(), RuntimeVersion: "stellwerk.runner.rollout/v2", PipelineVersion: "stellwerk.pipeline/v2", ToolsDigest: strings.Repeat("b", 64), StateBackendType: "s3", StateConfigurationDigest: strings.Repeat("c", 64)},
	}
	for _, watermark := range []NativeAdmissionWatermark{{Epoch: 1}, {AdmissionID: uuid.New()}, {AdmissionID: uuid.New(), Epoch: -1}} {
		request := base
		request.NativeAdmissionWatermark = &watermark
		_, err := AuthorizeDeploymentAdmission(t.Context(), nil, request)
		_, invalid := IsErrBadRequest(err)
		require.True(t, invalid, "%v", err)
	}
	for _, mode := range []string{"deploy", "plan_only", "rollback_plan"} {
		request := base
		request.Mode, request.NativeTarget = mode, nil
		request.NativeAdmissionWatermark = &NativeAdmissionWatermark{AdmissionID: uuid.New(), Epoch: 1}
		_, err := AuthorizeDeploymentAdmission(t.Context(), nil, request)
		_, invalid := IsErrBadRequest(err)
		require.True(t, invalid, "legacy authority cannot supply Native knowledge: %v", err)
		if mode != "deploy" {
			request.NativeAdmissionWatermark = nil
			_, err = AuthorizeDeploymentAdmission(t.Context(), nil, request)
			_, invalid = IsErrBadRequest(err)
			require.True(t, invalid, "legacy Plan has no ordinary Native guard: %v", err)
		}
	}
}
