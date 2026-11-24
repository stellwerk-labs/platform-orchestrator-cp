package api

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProjectPipelineScoreRoutesReachCoreAndPreserveDataPlaneBoundary(t *testing.T) {
	score, err := os.ReadFile("../../score.yaml")
	require.NoError(t, err)
	var pattern string
	for line := range strings.SplitSeq(string(score), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "path: /(") {
			pattern = strings.TrimPrefix(trimmed, "path: ")
			break
		}
	}
	require.NotEmpty(t, pattern, "the deployed Score route must expose project pipeline APIs")
	route, err := regexp.Compile("^" + pattern + "$")
	require.NoError(t, err)
	for _, path := range []string{
		"/orgs/acme/projects/project/pipelines",
		"/orgs/acme/projects/project/pipelines/policy",
		"/orgs/acme/projects/project/pipelines/00000000-0000-4000-8000-000000000001/publish",
		"/orgs/acme/projects/project/pipelines/versions/00000000-0000-4000-8000-000000000001/retire",
		"/orgs/acme/projects/project/envs/development/pipeline-choices",
		"/orgs/acme/projects/project/envs/development/module-catalogue",
	} {
		require.True(t, route.MatchString(path), "Core route misses %s", path)
	}
	for _, path := range []string{"/orgs/acme/deployments", "/orgs/acme/projects/project/envs/development/deploys", "/orgs/acme/projects/projectish/pipeline-choices"} {
		require.False(t, route.MatchString(path), "Core route captures unrelated endpoint %s", path)
	}
}
