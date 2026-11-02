package api

import (
	"context"
	"encoding/json"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidatePodTemplate_ok(t *testing.T) {
	for _, n := range []string{RunnerContainerName, RunnerLegacyContainerName} {
		t.Run(n, func(t *testing.T) {
			require.NoError(t, validatePodTemplate(map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{
							"name": n,
							"env": []map[string]interface{}{
								{"name": "CUSTOM_KEY", "value": "custom-value"},
							},
						},
					},
				},
			}))
		})
	}
}

func TestValidatePodTemplate_missing_name(t *testing.T) {
	require.EqualError(t, validatePodTemplate(map[string]interface{}{
		"spec": map[string]interface{}{
			"containers": []interface{}{
				map[string]interface{}{},
			},
		},
	}), "pod_template.spec.containers[*].name is required")
}

func TestPatchRunnerConfiguration(t *testing.T) {
	t.Run("should preserve auth when patching with nil", func(t *testing.T) {
		existingConfig := map[string]interface{}{
			"type": "serverless-ecs",
			"auth": map[string]interface{}{
				"role_arn":     "arn:aws:iam::123456789012:role/ExistingRole",
				"session_name": "existing-session",
			},
			"job": map[string]interface{}{
				"image":       "existing-image:v1",
				"environment": map[string]string{"OLD_VAR": "old_value"},
			},
		}

		newImage := "new-image:v2"
		patchConfig := &ServerlessEcsRunnerConfigurationUpdateBody{
			Type: RunnerTypeServerlessEcs,
			Auth: nil, // Omitted - should preserve existing
			Job: &ServerlessEcsRunnerJob{
				Image:       &newImage,
				Environment: map[string]string{"NEW_VAR": "new_value"},
			},
		}

		result := updateRunnerConfiguration(existingConfig, patchConfig)

		// Should preserve existing auth
		assert.Contains(t, result, "auth", "auth should be preserved when omitted from patch")
		authValue := result["auth"].(map[string]interface{})

		assert.Equal(t, "arn:aws:iam::123456789012:role/ExistingRole", authValue["role_arn"], "existing auth role_arn should be preserved")
		assert.Equal(t, "existing-session", authValue["session_name"], "existing auth session_name should be preserved")

		// Should update job
		assert.Contains(t, result, "job", "job should be updated")
		jobValue := result["job"].(map[string]interface{})

		assert.Equal(t, "new-image:v2", jobValue["image"], "job image should be updated")
	})
	t.Run("should preserve auth when patching with unspecified fields", func(t *testing.T) {
		existingConfig := map[string]interface{}{
			"type": "serverless-ecs",
			"auth": map[string]interface{}{
				"role_arn":     "arn:aws:iam::123456789012:role/ExistingRole",
				"session_name": "existing-session",
			},
			"job": map[string]interface{}{
				"image":       "existing-image:v1",
				"environment": map[string]string{"OLD_VAR": "old_value"},
			},
		}

		patchConfig := &ServerlessEcsRunnerConfigurationUpdateBody{
			Type: RunnerTypeServerlessEcs,
		}

		result := updateRunnerConfiguration(existingConfig, patchConfig)

		// Should preserve existing auth
		assert.Contains(t, result, "auth", "auth should be preserved when omitted from patch")
		authValue := result["auth"].(map[string]interface{})

		assert.Equal(t, "arn:aws:iam::123456789012:role/ExistingRole", authValue["role_arn"], "existing auth role_arn should be preserved")
		assert.Equal(t, "existing-session", authValue["session_name"], "existing auth session_name should be preserved")

		// Should preserve job
		assert.Contains(t, result, "job", "job should be updated")
		jobValue := result["job"].(map[string]interface{})

		assert.Equal(t, "existing-image:v1", jobValue["image"], "job image should be the same")
	})

}

func TestValidateAndPrepareRunnerConfigurationPatch(t *testing.T) {
	t.Run("should handle ECS patch with job update but no auth", func(t *testing.T) {
		server := &Server{}

		newImage := "new-image:v2"
		patchRequest := &RunnerConfigurationUpdate{}

		// Auth is omitted to test preservation
		ecsUpdateBody := ServerlessEcsRunnerConfigurationUpdateBody{
			Type: RunnerTypeServerlessEcs,
			Job: &ServerlessEcsRunnerJob{
				Image:       &newImage,
				Environment: map[string]string{"NEW_VAR": "new_value"},
			},
		}

		// Marshal to JSON to simulate what the API layer would do
		jsonBytes, err := json.Marshal(ecsUpdateBody)
		require.NoError(t, err)

		patchRequest.union = jsonBytes

		// Test the validation function
		runnerCfg, secretPath, err := server.tryMakeRunnerConfigurationUpdate(
			context.Background(),
			"test-org",
			"test-runner",
			"serverless-ecs",
			patchRequest,
		)

		// Should succeed without error
		require.NoError(t, err, "validation should succeed")
		assert.NotNil(t, runnerCfg, "should return runner config")
		assert.Nil(t, secretPath, "should return nil secret path")

		// Should return the correct update body type
		processedUpdateBody, ok := runnerCfg.(*ServerlessEcsRunnerConfigurationUpdateBody)
		assert.True(t, ok, "should return ServerlessEcsRunnerConfigurationUpdateBody type")

		// Auth should be nil (preserved from patch)
		assert.Nil(t, processedUpdateBody.Auth, "auth should be nil when omitted from patch")

		// Job should be present and correct
		assert.NotNil(t, processedUpdateBody.Job, "job should be present")
		assert.Equal(t, "new-image:v2", *processedUpdateBody.Job.Image, "job image should be correct")
		assert.Equal(t, "new_value", processedUpdateBody.Job.Environment["NEW_VAR"], "job environment should be correct")
	})
}

func TestRunnerParallelJobConfigurationIsPositiveOptionalAndPreserved(t *testing.T) {
	for _, runnerType := range []string{"kubernetes", "kubernetes-gke", "kubernetes-eks", "kubernetes-agent", "serverless-ecs"} {
		t.Run(runnerType, func(t *testing.T) {
			var legacy RunnerConfigurationUpdate
			require.NoError(t, json.Unmarshal([]byte(`{"type":"`+runnerType+`"}`), &legacy))
			_, patch, err := checkUpdateBodyRunnerConfigurationStrict(legacy)
			require.NoError(t, err)
			encoded, err := json.Marshal(patch)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), "max_parallel_jobs")
			for _, limit := range []int{0, -1} {
				body := map[string]any{"type": runnerType, "max_parallel_jobs": limit}
				raw, err := json.Marshal(body)
				require.NoError(t, err)
				var invalid RunnerConfigurationUpdate
				require.NoError(t, json.Unmarshal(raw, &invalid))
				_, _, err = checkUpdateBodyRunnerConfigurationStrict(invalid)
				require.ErrorContains(t, err, "positive")
			}
			var config RunnerConfigurationUpdate
			require.NoError(t, json.Unmarshal([]byte(`{"type":"`+runnerType+`","max_parallel_jobs":2,"supported_protocols":["environment-observation/v1"]}`), &config))
			_, patch, err = checkUpdateBodyRunnerConfigurationStrict(config)
			require.NoError(t, err)
			merged := updateRunnerConfiguration(map[string]any{"job": map[string]any{"image": "runner:v1"}}, patch)
			require.InDelta(t, 2, merged["max_parallel_jobs"], 0)
			require.Equal(t, []any{"environment-observation/v1"}, merged["supported_protocols"])
			internal := internalApiRunnerFromDbRunner(model.Runner{OrgId: "acme", Id: "runner", RunnerConfiguration: merged})
			raw, err := json.Marshal(internal)
			require.NoError(t, err)
			require.Contains(t, string(raw), `"max_parallel_jobs":2`)
		})
	}
}

func TestRunnerCommonFieldsPreserveOmissionAndSupportExplicitNull(t *testing.T) {
	for _, runnerType := range []string{"kubernetes", "kubernetes-gke", "kubernetes-eks", "kubernetes-agent", "serverless-ecs"} {
		t.Run(runnerType, func(t *testing.T) {
			existing := map[string]any{"type": runnerType, "max_parallel_jobs": float64(2), "supported_protocols": []any{"environment-observation/v1"}}
			for _, body := range []string{`{"type":"` + runnerType + `"}`, `{"type":"` + runnerType + `","job":{}}`} {
				var update RunnerConfigurationUpdate
				require.NoError(t, json.Unmarshal([]byte(body), &update))
				_, parsed, err := checkUpdateBodyRunnerConfigurationStrict(update)
				require.NoError(t, err)
				merged := updateRunnerConfiguration(existing, parsed)
				require.NoError(t, applyRunnerConfigurationClears(merged, &update))
				require.Equal(t, existing["max_parallel_jobs"], merged["max_parallel_jobs"])
				require.Equal(t, existing["supported_protocols"], merged["supported_protocols"])
			}
			var clear RunnerConfigurationUpdate
			require.NoError(t, json.Unmarshal([]byte(`{"type":"`+runnerType+`","max_parallel_jobs":null,"supported_protocols":null}`), &clear))
			_, parsed, err := checkUpdateBodyRunnerConfigurationStrict(clear)
			require.NoError(t, err)
			merged := updateRunnerConfiguration(existing, parsed)
			require.NoError(t, applyRunnerConfigurationClears(merged, &clear))
			require.NotContains(t, merged, "max_parallel_jobs")
			require.NotContains(t, merged, "supported_protocols")
			require.Contains(t, existing, "max_parallel_jobs", "a patch must not mutate its input")
			var empty RunnerConfigurationUpdate
			require.NoError(t, json.Unmarshal([]byte(`{"type":"`+runnerType+`","supported_protocols":[]}`), &empty))
			_, parsed, err = checkUpdateBodyRunnerConfigurationStrict(empty)
			require.NoError(t, err)
			merged = updateRunnerConfiguration(existing, parsed)
			require.NoError(t, applyRunnerConfigurationClears(merged, &empty))
			require.Equal(t, []any{}, merged["supported_protocols"])
			require.Equal(t, existing["max_parallel_jobs"], merged["max_parallel_jobs"])
			for _, field := range []string{"max_parallel_jobs", "supported_protocols"} {
				var create RunnerConfiguration
				require.NoError(t, json.Unmarshal([]byte(`{"type":"`+runnerType+`","`+field+`":null}`), &create))
				_, _, err := checkCreateBodyRunnerConfigurationStrict(create)
				require.ErrorContains(t, err, "cannot be null")
			}
			summary := apiRunnerSummaryFromDbRunner(model.Runner{RunnerType: runnerType, RunnerConfiguration: existing})
			require.NotNil(t, summary.RunnerConfiguration.MaxParallelJobs)
			require.Equal(t, 2, *summary.RunnerConfiguration.MaxParallelJobs)
			require.Equal(t, []string{"environment-observation/v1"}, *summary.RunnerConfiguration.SupportedProtocols)
		})
	}
}
