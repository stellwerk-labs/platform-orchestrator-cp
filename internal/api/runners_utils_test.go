package api

import (
	"context"
	"encoding/json"
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
