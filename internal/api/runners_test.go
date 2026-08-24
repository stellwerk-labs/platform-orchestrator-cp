package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/authz"
	orchestratoriam "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	mockorchestratoriam "github.com/stellwerk-labs/platform-orchestrator-cp/internal/clients/orchestratoriam/mocks"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	mockmodel "github.com/stellwerk-labs/platform-orchestrator-cp/internal/model/mocks"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/vault"
	mock_vault "github.com/stellwerk-labs/platform-orchestrator-cp/internal/vault/mocks"
)

// getJobConfigurationFromTestBody is a helper function for tests to get job configuration from test body
func getJobConfigurationFromTestBody(body interface{}) K8sRunnerJobConfig {
	switch cfg := body.(type) {
	case *K8sRunnerConfiguration:
		return cfg.Job
	case *K8sGkeRunnerConfiguration:
		return cfg.Job
	case *K8sEksRunnerConfiguration:
		return cfg.Job
	case *K8sAgentRunnerConfiguration:
		return cfg.Job
	case *K8sRunnerConfigurationUpdateBody:
		if cfg.Job != nil {
			return *cfg.Job
		}
		return K8sRunnerJobConfig{}
	case *K8sGkeRunnerConfigurationUpdateBody:
		if cfg.Job != nil {
			return *cfg.Job
		}
		return K8sRunnerJobConfig{}
	case *K8sEksRunnerConfigurationUpdateBody:
		if cfg.Job != nil {
			return *cfg.Job
		}
		return K8sRunnerJobConfig{}
	case *K8sAgentRunnerConfigurationUpdateBody:
		if cfg.Job != nil {
			return *cfg.Job
		}
		return K8sRunnerJobConfig{}
	default:
		return K8sRunnerJobConfig{}
	}
}

func TestCreateRunner_ImageValidation(t *testing.T) {
	tests := []struct {
		name        string
		image       string
		shouldFail  bool
		description string
	}{
		{"invalid empty string", "", true, "empty image name"},
		{"invalid spaces", "my image", true, "spaces not allowed"},
		{"invalid special chars", "invalid@image", true, "@ without the algorithm (eg. sha256) not allowed"},

		{"valid repository short", "a", false, "single character repository"},
		{"valid repository name", "nginx", false, "simple repository name"},
		{"valid repository name with tag", "nginx:latest", false, "repository with tag"},
		{"valid repository name with uppercase tag", "nginx:V1.2.3", false, "repository with uppercase tag"},
		{"valid repository name with underscores", "my_app", false, "repository with underscores"},
		{"valid repository name with dots", "app.service", false, "repository with dots"},
		{"valid repository name with hyphens", "my-awesome-app", false, "repository with hyphens"},

		{"valid namespace", "myorg/nginx", false, "repository with namespace"},
		{"valid namespace with path", "myorg/team/project/app", false, "repository with deep path"},

		{"valid registry", "registry.example.com/myorg/myapp:v1.2.3", false, "custom registry"},
		{"valid registry with port", "registry.example.com:5000/team/my-app:2.0", false, "custom registry with port"},
		{"valid registry docker hub", "docker.io/library/alpine:3.18", false, "explicit Docker Hub registry"},
		{"valid registry gcr", "gcr.io/my-project/my-app:latest", false, "Google Container Registry"},
		{"valid registry ecr", "123456789012.dkr.ecr.us-west-2.amazonaws.com/my-repo:v1.0", false, "AWS ECR"},
		{"valid registry localhost", "localhost:5000/test-image", false, "localhost registry"},

		{"valid tag latest ", "nginx:latest", false, "latest tag"},
		{"valid tag numeric ", "nginx:123", false, "numeric tag"},
		{"valid tag complex", "app:v1.2.3-alpha.1", false, "complex semantic version tag"},

		{"valid with digest sha256", "nginx@sha256:abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890", false, "with sha256 digest"},
		{"valid with digest sha512", "nginx@sha512:abcdef123456", false, "with sha512 digest"},
		{"valid with digest and tag", "nginx:latest@sha256:abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890", false, "with both tag and digest"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			createPayload := fmt.Sprintf(`{
				"id": "test-runner",
				"runner_configuration": {
					"type": "serverless-ecs",
					"auth": {
						"role_arn": "arn:aws:iam::123456789012:role/ServerlessEcsRole"
					},
					"job": {
						"region": "us-east-1",
						"cluster": "platform-orchestrator-runner",
						"subnets": ["subnet-12345", "subnet-67890"],
						"execution_role_arn": "arn:aws:iam::123456789012:role/RunnerExecRole",
						"image": "%s"
					}
				},
				"state_storage_configuration": {
					"type": "s3",
					"bucket": "test-bucket"
				}
			}`, tc.image)

			e, _, fin := MockServer(t)
			defer fin()

			req := httptest.NewRequest(http.MethodPost, "/orgs/test-org/runners", bytes.NewReader([]byte(createPayload)))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			e.ServeHTTP(rec, req)

			if tc.shouldFail {
				assert.Equal(t, http.StatusBadRequest, rec.Code, "Expected validation failure for: %s", tc.description)
				responseBody := rec.Body.String()
				assert.Contains(t, responseBody, "image", "Error should mention image field for: %s", tc.description)
			} else {
				assert.NotEqual(t, http.StatusBadRequest, rec.Code, "Valid image should not fail validation: %s", tc.description)
			}
		})
	}
}

func TestCreateRunner(t *testing.T) {
	e, s, fin := MockServer(t)
	defer fin()
	const orgId = "test-org"
	remoteRunnerPubKey := `-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEAc5dgCx4ano39JT0XgTsHnts3jej+5xl7ZAwSIrKpef0=
-----END PUBLIC KEY-----`

	for _, tc := range []struct {
		name          string
		body          any
		stateStorage  any
		requireVault  bool
		expectedError string
		expectedType  RunnerType
	}{
		{
			name: "GKE k8s valid runner",
			body: &K8sGkeRunnerConfiguration{
				Cluster: K8sRunnerGkeCluster{
					Name:      "rinsewind",
					ProjectId: "my-gcp-project",
					Location:  "eu-west-2a",
					Auth: K8sRunnerGcpTemporaryAuth{
						GcpAudience:       "boo",
						GcpServiceAccount: "gcp@google.com",
					},
				},
				Job: K8sRunnerJobConfig{
					Namespace:      "platform-orchestrator-runner",
					ServiceAccount: "platform-orchestrator-runner",
					PodTemplate: ref.Ref(map[string]interface{}{
						"metadata": map[string]interface{}{
							"labels": map[string]interface{}{
								"added-via-configuration": "true"},
						},
						"spec": map[string]interface{}{
							"containers": []interface{}{
								map[string]interface{}{
									"name": "platform-orchestrator-runner",
									"securityContext": map[string]interface{}{
										"allowPrivilegeEscalation": false,
										"capabilities":             map[string]interface{}{"drop": []interface{}{"ALL"}},
										"runAsGroup":               float64(2000),
										"runAsNonRoot":             true,
										"runAsUser":                float64(2000),
										"seccompProfile": map[string]interface{}{
											"type": "RuntimeDefault",
										},
									},
								},
							},
						},
					}),
				},
				Type: RunnerTypeKubernetesGke,
			},
			expectedType: RunnerTypeKubernetesGke,
		},
		{
			name: "EKS k8s valid runner",
			body: &K8sEksRunnerConfiguration{
				Cluster: K8sRunnerEksCluster{
					Name:   "my-eks-cluster",
					Region: "us-west-2",
					Auth: AwsTemporaryAuth{
						RoleArn:     "arn:aws:iam::123456789012:role/EKSClusterRole",
						SessionName: ref.Ref("platform-orchestrator-runner"),
						StsRegion:   ref.Ref("us-west-2"),
					},
				},
				Job: K8sRunnerJobConfig{
					Namespace:      "platform-orchestrator-runner",
					ServiceAccount: "platform-orchestrator-runner",
					PodTemplate: ref.Ref(map[string]interface{}{
						"metadata": map[string]interface{}{
							"labels": map[string]interface{}{
								"added-via-configuration": "true"},
						},
						"spec": map[string]interface{}{
							"containers": []interface{}{
								map[string]interface{}{
									"name": "platform-orchestrator-runner",
									"securityContext": map[string]interface{}{
										"allowPrivilegeEscalation": false,
										"capabilities":             map[string]interface{}{"drop": []interface{}{"ALL"}},
										"runAsGroup":               float64(2000),
										"runAsNonRoot":             true,
										"runAsUser":                float64(2000),
										"seccompProfile": map[string]interface{}{
											"type": "RuntimeDefault",
										},
									},
								},
							},
						},
					}),
				},
				Type: RunnerTypeKubernetesEks,
			},
			expectedType: RunnerTypeKubernetesEks,
		},
		{
			name:         "Vanilla K8s valid runner",
			requireVault: true,
			body: &K8sRunnerConfiguration{
				Cluster: K8sRunnerK8sCluster{
					Auth: K8sRunnerK8sClusterAuth{
						ServiceAccountToken: ref.Ref("t0ken"),
					},
					ClusterData: K8sRunnerK8sClusterClusterData{
						CertificateAuthorityData: "boo",
						Server:                   "http://10.10.10.10",
					},
				},
				Job: K8sRunnerJobConfig{
					Namespace:      "platform-orchestrator-runner",
					ServiceAccount: "platform-orchestrator-runner",
				},
				Type: RunnerTypeKubernetes,
			},
			expectedType: RunnerTypeKubernetes,
		},
		{
			name: "remote runner",
			body: &K8sAgentRunnerConfiguration{
				Job: K8sRunnerJobConfig{
					Namespace:      "platform-orchestrator-runner",
					ServiceAccount: "platform-orchestrator-runner",
				},
				Key:  remoteRunnerPubKey,
				Type: RunnerTypeKubernetesAgent,
			},
			expectedType: RunnerTypeKubernetesAgent,
		},
		{
			name: "ecs runner minimal configuration",
			body: &ServerlessEcsRunnerConfiguration{
				Type: RunnerTypeServerlessEcs,
				Auth: AwsTemporaryAuth{
					RoleArn: "arn:aws:iam::123456789012:role/ServerlessEcsRole",
				},
				Job: ServerlessEcsRunnerJob{
					Region:           "us-east-1",
					Cluster:          "platform-orchestrator-runner",
					Subnets:          []string{"subnet-12345"},
					ExecutionRoleArn: "arn:aws:iam::123456789012:role/RunnerExecRole",
				},
			},
			stateStorage: &S3StorageConfiguration{
				Type:   StateStorageTypeS3,
				Bucket: "platform-orchestrator-runner",
			},
			expectedType: RunnerTypeServerlessEcs,
		},
		{
			name: "ecs runner full configuration",
			body: &ServerlessEcsRunnerConfiguration{
				Type: RunnerTypeServerlessEcs,
				Auth: AwsTemporaryAuth{
					RoleArn: "arn:aws:iam::123456789012:role/ServerlessEcsRole",
				},
				Job: ServerlessEcsRunnerJob{
					Region:           "us-east-1",
					Cluster:          "platform-orchestrator-runner",
					Subnets:          []string{"subnet-12345", "subnet-67890"},
					ExecutionRoleArn: "arn:aws:iam::123456789012:role/RunnerExecRole",
					TaskRoleArn:      ref.Ref("arn:aws:iam::123456789012:role/RunnerTaskRole"),
					Image:            ref.Ref("custom-runner:v1.2.3"),
					Environment:      map[string]string{"A": "B"},
					Secrets: map[string]string{ //nolint:gosec // fake ARN values used only in test fixtures
						"MOUNTED_SECRET": "arn:aws:secretsmanager:us-east-1:123456789012:secret:prod/database/credentials-AbCdEf",
						"MOUNTED_PARAM":  "arn:aws:ssm:eu-central-1:667740703053:parameter/ben-testing",
					},
				},
			},
			stateStorage: &S3StorageConfiguration{
				Type:       StateStorageTypeS3,
				Bucket:     "platform-orchestrator-runner",
				PathPrefix: ref.Ref("path/prefix"),
			},
			expectedType: RunnerTypeServerlessEcs,
		},
		{
			name: "Runner type does not match runner configuration",
			body: &K8sRunnerConfiguration{
				Cluster: K8sRunnerK8sCluster{
					Auth: K8sRunnerK8sClusterAuth{
						ServiceAccountToken: ref.Ref("t0ken"),
					},
					ClusterData: K8sRunnerK8sClusterClusterData{
						CertificateAuthorityData: "boo",
						Server:                   "http://10.10.10.10",
					},
				},
				Job: K8sRunnerJobConfig{
					Namespace:      "platform-orchestrator-runner",
					ServiceAccount: "platform-orchestrator-runner",
				},
				Type: RunnerTypeKubernetesGke,
			},
			expectedError: "/runner_configuration/cluster/auth/gcp_audience\\\": property \\\"gcp_audience\\\" is missing",
		},
		{
			name: "bad configuration",
			body: &K8sRunnerConfiguration{
				Cluster: K8sRunnerK8sCluster{
					ClusterData: K8sRunnerK8sClusterClusterData{
						CertificateAuthorityData: "boo",
						Server:                   "http://10.10.10.10",
					},
				},
				Job: K8sRunnerJobConfig{
					Namespace:      "platform-orchestrator-runner",
					ServiceAccount: "platform-orchestrator-runner",
				},
				Type: RunnerTypeKubernetesGke,
			},
			expectedError: "/runner_configuration/cluster/auth/gcp_audience\\\": property \\\"gcp_audience\\\" is missing",
		},
		{
			name: "GKE k8s valid runner with invalid job pod template",
			body: &K8sGkeRunnerConfiguration{
				Cluster: K8sRunnerGkeCluster{
					Name:      "rinsewind",
					ProjectId: "my-gcp-project",
					Location:  "eu-west-2a",
					Auth: K8sRunnerGcpTemporaryAuth{
						GcpAudience:       "boo",
						GcpServiceAccount: "gcp@google.com",
					},
				},
				Job: K8sRunnerJobConfig{
					Namespace:      "platform-orchestrator-runner",
					ServiceAccount: "platform-orchestrator-runner",
					PodTemplate: ref.Ref(map[string]interface{}{
						"metadata": map[string]interface{}{
							"labels": map[string]interface{}{
								"added-via-configuration": "true"},
						},
						"spec": map[string]interface{}{
							"containers": []interface{}{
								map[string]interface{}{
									"name": "platform-orchestrator-runner",
									"securityContext": map[string]interface{}{
										"allowPrivilegeEscalation": 1,
									},
								},
							},
						}}),
				},
				Type: RunnerTypeKubernetesGke,
			},
			expectedError: "\"message\":\"failed to convert supplied pod template into a kubernetes pod template: unrecognized type: bool",
		},
		{
			name: "GKE k8s valid runner with invalid job pod template - namespace specified",
			body: &K8sGkeRunnerConfiguration{
				Cluster: K8sRunnerGkeCluster{
					Name:      "rinsewind",
					ProjectId: "my-gcp-project",
					Location:  "eu-west-2a",
					Auth: K8sRunnerGcpTemporaryAuth{
						GcpAudience:       "boo",
						GcpServiceAccount: "gcp@google.com",
					},
				},
				Job: K8sRunnerJobConfig{
					Namespace:      "platform-orchestrator-runner",
					ServiceAccount: "platform-orchestrator-runner",
					PodTemplate: ref.Ref(map[string]interface{}{
						"metadata": map[string]interface{}{
							"namespace": "another-ns",
						},
						"spec": map[string]interface{}{
							"containers": []interface{}{
								map[string]interface{}{
									"name": "platform-orchestrator-runner",
									"securityContext": map[string]interface{}{
										"allowPrivilegeEscalation": true,
									},
								},
							},
						}}),
				},
				Type: RunnerTypeKubernetesGke,
			},
			expectedError: "\"message\":\"it is not allowed to specify a namespace in the pod_template field of the runner configuration",
		},
		{
			name: "GKE k8s valid runner with invalid job pod template - service account specified",
			body: &K8sGkeRunnerConfiguration{
				Cluster: K8sRunnerGkeCluster{
					Name:      "rinsewind",
					ProjectId: "my-gcp-project",
					Location:  "eu-west-2a",
					Auth: K8sRunnerGcpTemporaryAuth{
						GcpAudience:       "boo",
						GcpServiceAccount: "gcp@google.com",
					},
				},
				Job: K8sRunnerJobConfig{
					Namespace:      "platform-orchestrator-runner",
					ServiceAccount: "platform-orchestrator-runner",
					PodTemplate: ref.Ref(map[string]interface{}{
						"spec": map[string]interface{}{
							"serviceAccountName": "my-sa",
							"containers": []interface{}{
								map[string]interface{}{
									"name": "platform-orchestrator-runner",
									"securityContext": map[string]interface{}{
										"allowPrivilegeEscalation": true,
									},
								},
							},
						}}),
				},
				Type: RunnerTypeKubernetesGke,
			},
			expectedError: "\"message\":\"it is not allowed to specify a serviceAccountName in the pod_template field of the runner configuration",
		},
		{
			name: "GKE k8s valid runner with invalid job pod template - forbidden env variable",
			body: &K8sGkeRunnerConfiguration{
				Cluster: K8sRunnerGkeCluster{
					Name:      "rinsewind",
					ProjectId: "my-gcp-project",
					Location:  "eu-west-2a",
					Auth: K8sRunnerGcpTemporaryAuth{
						GcpAudience:       "boo",
						GcpServiceAccount: "gcp@google.com",
					},
				},
				Job: K8sRunnerJobConfig{
					Namespace:      "platform-orchestrator-runner",
					ServiceAccount: "platform-orchestrator-runner",
					PodTemplate: ref.Ref(map[string]interface{}{
						"spec": map[string]interface{}{
							"containers": []interface{}{
								map[string]interface{}{
									"name": "platform-orchestrator-runner",
									"securityContext": map[string]interface{}{
										"allowPrivilegeEscalation": true,
									},
									"env": []map[string]interface{}{
										{"name": "MODE", "value": "my-own-mode"},
									},
								},
							},
						}}),
				},
				Type: RunnerTypeKubernetesGke,
			},
			expectedError: "\"message\":\"environment variable `MODE` is a reserved one and can't be overwritten by pod_template field",
		},
		{
			name: "GKE runner with GCS state storage",
			body: &K8sGkeRunnerConfiguration{
				Cluster: K8sRunnerGkeCluster{
					Name:      "rinsewind",
					ProjectId: "my-gcp-project",
					Location:  "eu-west-2a",
					Auth: K8sRunnerGcpTemporaryAuth{
						GcpAudience:       "boo",
						GcpServiceAccount: "gcp@google.com",
					},
				},
				Job: K8sRunnerJobConfig{
					Namespace:      "platform-orchestrator-runner",
					ServiceAccount: "platform-orchestrator-runner",
				},
				Type: RunnerTypeKubernetesGke,
			},
			stateStorage: &GCSStorageConfiguration{
				Type:   StateStorageTypeGcs,
				Bucket: "platform-orchestrator-runner-bucket",
			},
			expectedType: RunnerTypeKubernetesGke,
		},
		{
			name: "GKE runner with GCS state storage and path prefix",
			body: &K8sGkeRunnerConfiguration{
				Cluster: K8sRunnerGkeCluster{
					Name:      "rinsewind",
					ProjectId: "my-gcp-project",
					Location:  "eu-west-2a",
					Auth: K8sRunnerGcpTemporaryAuth{
						GcpAudience:       "boo",
						GcpServiceAccount: "gcp@google.com",
					},
				},
				Job: K8sRunnerJobConfig{
					Namespace:      "platform-orchestrator-runner",
					ServiceAccount: "platform-orchestrator-runner",
				},
				Type: RunnerTypeKubernetesGke,
			},
			stateStorage: &GCSStorageConfiguration{
				Type:       StateStorageTypeGcs,
				Bucket:     "platform-orchestrator-runner-bucket",
				PathPrefix: ref.Ref("path/prefix"),
			},
			expectedType: RunnerTypeKubernetesGke,
		},
		{
			name: "ecs runner cannot have gcs state storage",
			body: &ServerlessEcsRunnerConfiguration{
				Type: RunnerTypeServerlessEcs,
				Auth: AwsTemporaryAuth{RoleArn: "arn:aws:iam::123456789012:role/RunnerExecRole"},
				Job: ServerlessEcsRunnerJob{
					Region:           "us-east-1",
					Cluster:          "platform-orchestrator-runner",
					Subnets:          []string{"subnet-12345"},
					ExecutionRoleArn: "arn:aws:iam::123456789012:role/RunnerExecRole",
				},
			},
			stateStorage:  &GCSStorageConfiguration{Type: StateStorageTypeGcs, Bucket: "platform-orchestrator-runner"},
			expectedError: `"state storage type 'gcs' is not compatible with runner type 'serverless-ecs', it must be one of [s3]"`,
		},
		{
			name: "ecs runner missing auth",
			body: &ServerlessEcsRunnerConfiguration{
				Type: RunnerTypeServerlessEcs,
				Job: ServerlessEcsRunnerJob{
					Region:           "us-east-1",
					Cluster:          "platform-orchestrator-runner",
					ExecutionRoleArn: "arn:aws:iam::123456789012:role/RunnerExecRole",
				},
			},
			stateStorage:  &S3StorageConfiguration{Type: StateStorageTypeS3, Bucket: "platform-orchestrator-runner"},
			expectedError: `"role_arn is not a valid IAM Role ARN"`,
		},
		{
			name: "ecs runner cannot have kubernetes state storage",
			body: &ServerlessEcsRunnerConfiguration{
				Type: RunnerTypeServerlessEcs,
				Auth: AwsTemporaryAuth{RoleArn: "arn:aws:iam::123456789012:role/RunnerExecRole"},
				Job: ServerlessEcsRunnerJob{
					Region:           "us-east-1",
					Cluster:          "platform-orchestrator-runner",
					Subnets:          []string{"subnet-12345"},
					ExecutionRoleArn: "arn:aws:iam::123456789012:role/RunnerExecRole",
				},
			},
			expectedError: `"state storage type 'kubernetes' is not compatible with runner type 'serverless-ecs', it must be one of [s3]"`,
		},
		{
			name: "ecs runner bad secret arn",
			body: &ServerlessEcsRunnerConfiguration{
				Type: RunnerTypeServerlessEcs,
				Auth: AwsTemporaryAuth{RoleArn: "arn:aws:iam::123456789012:role/RunnerExecRole"},
				Job: ServerlessEcsRunnerJob{
					Region:           "us-east-1",
					Cluster:          "platform-orchestrator-runner",
					ExecutionRoleArn: "arn:aws:iam::123456789012:role/RunnerExecRole",
					Secrets:          map[string]string{"a": "b"},
				},
			},
			stateStorage:  &S3StorageConfiguration{Type: StateStorageTypeS3, Bucket: "platform-orchestrator-runner"},
			expectedError: `"job secrets value is not a valid AWS Secret or Parameter ARN"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := s.Database.(*mockmodel.MockDatabaser)
			var storedRunner *model.Runner
			db.EXPECT().CreateRunner(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _ model.Tx, request *model.Runner) (*model.Runner, error) {
				storedRunner = &model.Runner{Id: "test-runner", RunnerConfiguration: request.RunnerConfiguration, RunnerType: string(request.RunnerType)}
				return storedRunner, nil
			}).MaxTimes(1)
			if tc.requireVault {
				vlt := s.Vault.(*mock_vault.MockVaultClientInterface)
				vlt.EXPECT().UpsertSecret(gomock.Any(), fmt.Sprintf("/platform-orchestrator/orgs/%s/runners/%s", orgId, "test-runner"),
					map[string]interface{}{"cluster": map[string]interface{}{"auth": map[string]interface{}{"service_account_token": "t0ken"}, "cluster_data": map[string]interface{}{
						"certificate_authority_data": "", "server": "",
					}}, "job": map[string]interface{}{"namespace": "", "service_account": ""}, "type": ""}).Return(2, nil).Times(1)
			}

			encodedRunnerCfg, _ := json.Marshal(tc.body)
			if tc.stateStorage == nil {
				// default to kubernetes state storage unless specified
				tc.stateStorage = map[string]interface{}{ //nolint:gosec // "secret_suffix" key name triggers false positive; value is not a credential
					"secret_suffix": "state-suffix-my-project-my-env",
					"namespace":     "platform-orchestrator-runner",
					"type":          StateStorageTypeKubernetes,
				}
			}
			encodedStateCfg, _ := json.Marshal(tc.stateStorage)

			userId := userid.NewHumanUserId()
			mockIamClient := s.IamClient.(*mockorchestratoriam.MockClientWithResponsesInterface)
			mockIamClient.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
				UserId: userId,
				Checks: []orchestratoriam.ResourcePermissionCheck{authz.OrgCheck(orgId, authz.PermissionRunnerWrite)},
			}).Return(&orchestratoriam.InternalAuthorizeResponse{
				HTTPResponse: &http.Response{StatusCode: http.StatusNoContent},
			}, nil)

			bod, _ := json.Marshal(RunnerCreateBody{
				Description:               ref.Ref("Test Runner"),
				Id:                        "test-runner",
				RunnerConfiguration:       RunnerConfiguration{union: encodedRunnerCfg},
				StateStorageConfiguration: StateStorageConfiguration{union: encodedStateCfg},
			})
			req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/orgs/%s/runners", orgId), bytes.NewReader(bod))
			req.Header.Set("From", userId.String())
			resp := httptest.NewRecorder()
			e.ServeHTTP(resp, req)
			b, _ := io.ReadAll(resp.Result().Body)
			if tc.expectedError == "" {
				var respBody CreateRunner201JSONResponse
				require.NoError(t, json.Unmarshal(b, &respBody))
				require.Equal(t, http.StatusCreated, resp.Result().StatusCode, string(b))
				runnerType, _ := respBody.RunnerConfiguration.Discriminator()
				require.Equal(t, string(tc.expectedType), runnerType)
				switch tc.expectedType {
				case RunnerTypeKubernetes:
					k8sCfg, err := respBody.RunnerConfiguration.AsK8sRunnerConfiguration()
					require.NoError(t, err)

					require.Equal(t, K8sRunnerK8sCluster{
						Auth: K8sRunnerK8sClusterAuth{
							ServiceAccountToken: ref.Ref("SECRET"),
						},
						ClusterData: K8sRunnerK8sClusterClusterData{
							CertificateAuthorityData: "boo",
							Server:                   "http://10.10.10.10",
						}}, k8sCfg.Cluster)
				case RunnerTypeKubernetesAgent:
					remoteCfg, err := respBody.RunnerConfiguration.AsK8sAgentRunnerConfiguration()
					require.NoError(t, err)
					require.Equal(t, K8sAgentRunnerConfiguration{
						Job: K8sRunnerJobConfig{
							Namespace:      "platform-orchestrator-runner",
							ServiceAccount: "platform-orchestrator-runner",
						},
						Key:  remoteRunnerPubKey,
						Type: RunnerTypeKubernetesAgent,
					}, remoteCfg)
				case RunnerTypeServerlessEcs:
					expected, _ := json.Marshal(tc.body)
					actual, _ := respBody.RunnerConfiguration.MarshalJSON()
					assert.JSONEq(t, string(expected), string(actual))
				}

				expectedJob := getJobConfigurationFromTestBody(tc.body)
				var actualJobCfg K8sRunnerJobConfig
				if tc.expectedType == RunnerTypeKubernetesGke {
					gkeCfg, err := respBody.RunnerConfiguration.AsK8sGkeRunnerConfiguration()
					require.NoError(t, err)
					actualJobCfg = gkeCfg.Job
				} else {
					k8sCfg, err := respBody.RunnerConfiguration.AsK8sRunnerConfiguration()
					require.NoError(t, err)
					actualJobCfg = k8sCfg.Job
				}
				require.Equal(t, expectedJob, actualJobCfg)
			} else {
				require.Contains(t, string(b), tc.expectedError)
			}
		})
	}
}

func TestUpdateRunner(t *testing.T) {
	existingRunner := &model.Runner{
		Description: opt.Of("GKE Runner"),
		Id:          "my-gke-runner",
		RunnerConfiguration: map[string]interface{}{
			"cluster": map[string]interface{}{
				"name":        "my-eu-cluster",
				"project_id":  "my-gcp-project",
				"location":    "europe-west1-b",
				"internal_ip": false,
				"auth": map[string]interface{}{
					"gcp_audience":        "boo",
					"gcp_service_account": "gcp@google.com",
				},
			},
			"job": map[string]interface{}{
				"namespace":       "platform-orchestrator-runner",
				"service_account": "platform-orchestrator-runner",
			},
		},
		RunnerType:       string(RunnerTypeKubernetesGke),
		StateStorageType: string(StateStorageTypeKubernetes),
		StateStorageConfiguration: map[string]interface{}{ //nolint:gosec // "secret_suffix" key name triggers false positive; value is not a credential
			"secret_suffix": "state-suffix-my-project-my-env",
			"namespace":     "platform-orchestrator-runner",
		},
	}

	existingEcsRunner := &model.Runner{
		Description: opt.Of("ECS Runner"),
		Id:          "my-ecs-runner",
		RunnerConfiguration: map[string]interface{}{
			"auth": map[string]interface{}{
				"role_arn": "arn:aws:iam::123456789012:role/EcsRole",
			},
			"job": map[string]interface{}{
				"region":             "us-east-1",
				"cluster":            "my-cluster",
				"subnets":            []interface{}{"subnet-12345"},
				"execution_role_arn": "arn:aws:iam::123456789012:role/ExecutionRole",
			},
			"type": "serverless-ecs",
		},
		RunnerType:       string(RunnerTypeServerlessEcs),
		StateStorageType: string(StateStorageTypeS3),
		StateStorageConfiguration: map[string]interface{}{
			"bucket": "my-s3-bucket",
			"type":   "s3",
		},
	}

	gkeConfigUpdate, _ := json.Marshal(map[string]interface{}{
		"cluster": map[string]interface{}{
			"name":        "my-us-cluster",
			"project_id":  "my-gcp-project",
			"location":    "us-east1-b",
			"internal_ip": false,
			"auth": map[string]interface{}{
				"gcp_audience":        "boo",
				"gcp_service_account": "gcp@google.com",
			},
		},
		"type": string(RunnerTypeKubernetesGke),
	})

	gkeConfigUpdateWithJob, _ := json.Marshal(map[string]interface{}{
		"cluster": map[string]interface{}{
			"name":        "my-us-cluster",
			"project_id":  "my-gcp-project",
			"location":    "us-east1-b",
			"internal_ip": false,
			"auth": map[string]interface{}{
				"gcp_audience":        "boo",
				"gcp_service_account": "gcp@google.com",
			},
		},
		"type": string(RunnerTypeKubernetesGke),
		"job": map[string]interface{}{
			"namespace":       "default",
			"service_account": "platform-orchestrator-runner",
		},
	})

	gkeConfigUpdateWithJobAndTemplate, _ := json.Marshal(map[string]interface{}{
		"cluster": map[string]interface{}{
			"name":        "my-us-cluster",
			"project_id":  "my-gcp-project",
			"location":    "us-east1-b",
			"internal_ip": false,
			"auth": map[string]interface{}{
				"gcp_audience":        "boo",
				"gcp_service_account": "gcp@google.com",
			},
		},
		"type": string(RunnerTypeKubernetesGke),
		"job": map[string]interface{}{
			"namespace":       "default",
			"service_account": "platform-orchestrator-runner",
			"pod_template": ref.Ref(map[string]interface{}{
				"metadata": map[string]interface{}{
					"labels": map[string]interface{}{
						"added-via-configuration": "true"},
				},
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{
							"name": "platform-orchestrator-runner",
							"securityContext": map[string]interface{}{
								"allowPrivilegeEscalation": 1,
							},
						},
					},
				}}),
		},
	})

	gkeConfigUpdateWithJobAndInvalidTemplate, _ := json.Marshal(map[string]interface{}{
		"cluster": map[string]interface{}{
			"name":        "my-us-cluster",
			"project_id":  "my-gcp-project",
			"location":    "us-east1-b",
			"internal_ip": false,
			"auth": map[string]interface{}{
				"gcp_audience":        "boo",
				"gcp_service_account": "gcp@google.com",
			},
		},
		"type": string(RunnerTypeKubernetesGke),
		"job": map[string]interface{}{
			"namespace":       "default",
			"service_account": "platform-orchestrator-runner",
			"pod_template": ref.Ref(map[string]interface{}{
				"metadata": map[string]interface{}{
					"labels": map[string]interface{}{
						"added-via-configuration": "true"},
				},
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{
							"name": "platform-orchestrator-runner",
							"securityContext": map[string]interface{}{
								"allowPrivilegeEscalation": true,
							},
							"env": []map[string]interface{}{
								{"name": "ORG_ID", "value": "my-org"},
							},
						},
					},
				}}),
		},
	})

	gkeConfigUpdateMissingField, _ := json.Marshal(map[string]interface{}{
		"cluster": map[string]interface{}{
			"project_id":  "my-gcp-project",
			"location":    "us-east1-b",
			"internal_ip": false,
			"auth": map[string]interface{}{
				"gcp_audience":        "boo",
				"gcp_service_account": "gcp@google.com",
			},
		},
		"type": string(RunnerTypeKubernetesGke),
	})

	gkeConfigUpdateUnknownField, _ := json.Marshal(map[string]interface{}{
		"cluster": map[string]interface{}{
			"name":          "my-us-cluster",
			"project_id":    "my-gcp-project",
			"location":      "us-east1-b",
			"internal_ip":   false,
			"unknown_field": "unknown_value",
			"auth": map[string]interface{}{
				"gcp_audience":        "boo",
				"gcp_service_account": "gcp@google.com",
			},
		},
		"type": string(RunnerTypeKubernetesGke),
	})

	k8sConfigUpdate, _ := json.Marshal(map[string]interface{}{
		"cluster": map[string]interface{}{
			"cluster_data": map[string]interface{}{
				"certificate_authority_data": "boo",
				"server":                     "http://10.10.10.10",
			},
			"auth": map[string]interface{}{
				"service_account_token": "t0ken",
			},
		},
		"type": string(RunnerTypeKubernetes),
	})

	e, s, fin := MockServer(t)
	defer fin()
	for _, tc := range []struct {
		name                             string
		runnerId                         string
		body                             UpdateRunnerJSONRequestBody
		expectedModelRunnerConfiguration map[string]interface{}
		expectedError                    string
	}{
		{
			name:     "Mismatching types between existing runner and patch",
			runnerId: existingRunner.Id,
			body: UpdateRunnerJSONRequestBody{
				RunnerConfiguration: &RunnerConfigurationUpdate{
					union: k8sConfigUpdate,
				},
			},
			expectedError: "runner type kubernetes does not match existing runner type kubernetes-gke",
		},
		{
			name:     "Missing field in gke cluster patch",
			runnerId: existingRunner.Id,
			body: UpdateRunnerJSONRequestBody{
				RunnerConfiguration: &RunnerConfigurationUpdate{
					union: gkeConfigUpdateMissingField,
				},
			},
			expectedError: "property \\\"name\\\" is missing",
		},
		{
			name:     "Unknown field in gke cluster patch",
			runnerId: existingRunner.Id,
			body: UpdateRunnerJSONRequestBody{
				RunnerConfiguration: &RunnerConfigurationUpdate{
					union: gkeConfigUpdateUnknownField,
				},
			},
			expectedError: "the supplied runner configuration does not match the specified runner type kubernetes-gke: json: unknown field \\\"unknown_field\\\"",
		},
		{
			name:     "Update existing GKE runner configuration",
			runnerId: existingRunner.Id,
			body: RunnerUpdateBody{
				RunnerConfiguration: &RunnerConfigurationUpdate{
					union: gkeConfigUpdate,
				},
			},
			expectedModelRunnerConfiguration: map[string]interface{}{
				"cluster": map[string]interface{}{
					"name":        "my-us-cluster",
					"project_id":  "my-gcp-project",
					"location":    "us-east1-b",
					"internal_ip": false,
					"auth": map[string]interface{}{
						"gcp_audience":        "boo",
						"gcp_service_account": "gcp@google.com",
					},
				},
				"job": map[string]interface{}{
					"namespace":       "platform-orchestrator-runner",
					"service_account": "platform-orchestrator-runner",
				},
				"type": "kubernetes-gke",
			},
		},
		{
			name:     "Update existing GKE runner multiple props",
			runnerId: existingRunner.Id,
			body: RunnerUpdateBody{
				Description: ref.Ref("GKE Runner Updated"),
				RunnerConfiguration: &RunnerConfigurationUpdate{
					union: gkeConfigUpdateWithJob,
				},
			},
			expectedModelRunnerConfiguration: map[string]interface{}{
				"cluster": map[string]interface{}{
					"name":        "my-us-cluster",
					"project_id":  "my-gcp-project",
					"location":    "us-east1-b",
					"internal_ip": false,
					"auth": map[string]interface{}{
						"gcp_audience":        "boo",
						"gcp_service_account": "gcp@google.com",
					},
				},
				"job": map[string]interface{}{
					"namespace":       "default",
					"service_account": "platform-orchestrator-runner",
				},
				"type": "kubernetes-gke",
			},
		},
		{
			name:     "Fail updating existing GKE runner with invalid pod template configuration",
			runnerId: existingRunner.Id,
			body: RunnerUpdateBody{
				RunnerConfiguration: &RunnerConfigurationUpdate{
					union: gkeConfigUpdateWithJobAndTemplate,
				},
			},
			expectedError: "\"message\":\"failed to convert supplied pod template into a kubernetes pod template: unrecognized type: bool",
		},
		{
			name:     "Fail updating existing GKE runner with invalid pod template configuration - forbidden env var",
			runnerId: existingRunner.Id,
			body: RunnerUpdateBody{
				RunnerConfiguration: &RunnerConfigurationUpdate{union: gkeConfigUpdateWithJobAndInvalidTemplate},
			},
			expectedError: "\"message\":\"environment variable `ORG_ID` is a reserved one and can't be overwritten by pod_template field",
		},
		{
			name:     "Update ECS runner image field",
			runnerId: existingEcsRunner.Id,
			body: RunnerUpdateBody{
				RunnerConfiguration: &RunnerConfigurationUpdate{
					union: func() json.RawMessage {
						data, _ := json.Marshal(&ServerlessEcsRunnerConfigurationUpdateBody{
							Type: RunnerTypeServerlessEcs,
							Job: &ServerlessEcsRunnerJob{
								Region:           "us-east-1",
								Cluster:          "my-cluster",
								Subnets:          []string{"subnet-12345"},
								ExecutionRoleArn: "arn:aws:iam::123456789012:role/ExecutionRole",
								Image:            ref.Ref("new-runner-image:v2.0"),
							},
						})
						return data
					}(),
				},
			},
			expectedModelRunnerConfiguration: map[string]interface{}{
				"auth": map[string]interface{}{
					"role_arn": "arn:aws:iam::123456789012:role/EcsRole",
				},
				"job": map[string]interface{}{
					"region":             "us-east-1",
					"cluster":            "my-cluster",
					"subnets":            []interface{}{"subnet-12345"},
					"execution_role_arn": "arn:aws:iam::123456789012:role/ExecutionRole",
					"image":              "new-runner-image:v2.0",
				},
				"type": "serverless-ecs",
			},
		},
		{
			name:     "Fail updating non-existing runner",
			runnerId: "my-non-existing-runner",
			body: RunnerUpdateBody{
				RunnerConfiguration: &RunnerConfigurationUpdate{union: gkeConfigUpdate},
			},
			expectedError: "runner not found",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var getResp []interface{}
			switch tc.runnerId {
			case existingRunner.Id:
				getResp = []interface{}{existingRunner, nil}
			case existingEcsRunner.Id:
				getResp = []interface{}{existingEcsRunner, nil}
			default:
				getResp = []interface{}{nil, model.NewErrNotFound("runner not found")}
			}
			db := s.Database.(*mockmodel.MockDatabaser)
			db.EXPECT().
				GetRunner(gomock.Any(), gomock.Any(), "my-org", tc.runnerId).
				Return(getResp...).
				MaxTimes(1)
			if len(tc.expectedModelRunnerConfiguration) > 0 {
				db.EXPECT().
					UpdateRunner(gomock.Any(), gomock.Any(), "my-org", tc.runnerId, gomock.Any()).
					DoAndReturn(func(_ context.Context, _ model.Tx, _ string, _ string, runnerPatch *model.RunnerPatch) (*model.Runner, error) {
						assert.Equal(t, tc.expectedModelRunnerConfiguration, *runnerPatch.RunnerConfiguration)
						return &model.Runner{
							UpdatedAt: time.Now().UTC(),
						}, nil
					}).Times(1)
			}

			userId := userid.NewHumanUserId()
			mockIamClient := s.IamClient.(*mockorchestratoriam.MockClientWithResponsesInterface)
			mockIamClient.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
				UserId: userId,
				Checks: []orchestratoriam.ResourcePermissionCheck{authz.OrgCheck("my-org", authz.PermissionRunnerWrite)},
			}).Return(&orchestratoriam.InternalAuthorizeResponse{
				HTTPResponse: &http.Response{StatusCode: http.StatusNoContent},
			}, nil)
			bod, _ := json.Marshal(tc.body)
			req := httptest.NewRequest(http.MethodPatch, "/orgs/my-org/runners/"+tc.runnerId, bytes.NewReader(bod))
			req.Header.Set("From", userId.String())
			resp := httptest.NewRecorder()
			e.ServeHTTP(resp, req)
			b, _ := io.ReadAll(resp.Result().Body)
			if tc.expectedError == "" {
				require.Equal(t, http.StatusOK, resp.Result().StatusCode, string(b))
			} else {
				require.Contains(t, string(b), tc.expectedError)
			}
		})
	}
}

func TestDeleteRunner(t *testing.T) {
	e, s, fin := MockServer(t)
	defer fin()
	const orgId = "test-org"
	const runnerId = "test-runner"

	for _, tc := range []struct {
		name           string
		runnerId       string
		setupMocks     func(*mockmodel.MockDatabaser, *mock_vault.MockVaultClientInterface)
		expectedStatus int
		expectedError  string
	}{
		{
			name:     "Successfully delete kubernetes runner",
			runnerId: runnerId,
			setupMocks: func(db *mockmodel.MockDatabaser, vlt *mock_vault.MockVaultClientInterface) {
				runner := &model.Runner{
					Id:         runnerId,
					OrgId:      orgId,
					RunnerType: string(RunnerTypeKubernetes),
				}
				db.EXPECT().GetRunner(gomock.Any(), gomock.Any(), orgId, runnerId).Return(runner, nil).Times(1)
				vlt.EXPECT().DeleteSecret(gomock.Any(), fmt.Sprintf("/platform-orchestrator/orgs/%s/runners/%s", orgId, runnerId)).Return(nil).Times(1)
				db.EXPECT().ListEnvironmentsByRunnerId(gomock.Any(), gomock.Any(), orgId, runnerId, "", getEnvsByRunnerPaginationSize).Return([]model.Environment{}, "", nil).Times(1)
				db.EXPECT().DeleteRunner(gomock.Any(), gomock.Any(), orgId, runnerId).Return(nil).Times(1)
			},
			expectedStatus: 204,
		},
		{
			name:     "Successfully delete non-kubernetes runner (no vault deletion)",
			runnerId: runnerId,
			setupMocks: func(db *mockmodel.MockDatabaser, vlt *mock_vault.MockVaultClientInterface) {
				runner := &model.Runner{
					Id:         runnerId,
					OrgId:      orgId,
					RunnerType: string(RunnerTypeKubernetesGke),
				}
				db.EXPECT().GetRunner(gomock.Any(), gomock.Any(), orgId, runnerId).Return(runner, nil).Times(1)
				db.EXPECT().ListEnvironmentsByRunnerId(gomock.Any(), gomock.Any(), orgId, runnerId, "", getEnvsByRunnerPaginationSize).Return([]model.Environment{}, "", nil).Times(1)
				db.EXPECT().DeleteRunner(gomock.Any(), gomock.Any(), orgId, runnerId).Return(nil).Times(1)
			},
			expectedStatus: 204,
		},
		{
			name:     "Runner not found - 404",
			runnerId: "non-existent-runner",
			setupMocks: func(db *mockmodel.MockDatabaser, vlt *mock_vault.MockVaultClientInterface) {
				db.EXPECT().GetRunner(gomock.Any(), gomock.Any(), orgId, "non-existent-runner").Return(nil, model.NewErrNotFound("runner not found")).Times(1)
			},
			expectedStatus: 404,
			expectedError:  "runner not found",
		},
		{
			name:     "Runner in use by environments - 409 - no vault deletion",
			runnerId: runnerId,
			setupMocks: func(db *mockmodel.MockDatabaser, vlt *mock_vault.MockVaultClientInterface) {
				runner := &model.Runner{
					Id:         runnerId,
					OrgId:      orgId,
					RunnerType: string(RunnerTypeKubernetes),
				}
				environments := []model.Environment{
					{Id: "env1", ProjectId: "project1"},
					{Id: "env2", ProjectId: "project2"},
				}
				db.EXPECT().GetRunner(gomock.Any(), gomock.Any(), orgId, runnerId).Return(runner, nil)
				db.EXPECT().ListEnvironmentsByRunnerId(gomock.Any(), gomock.Any(), orgId, runnerId, "", getEnvsByRunnerPaginationSize).Return(environments, "", nil)
				vlt.EXPECT().DeleteSecret(gomock.Any(), fmt.Sprintf("/platform-orchestrator/orgs/%s/runners/%s", orgId, runnerId)).Return(nil).Times(0)
			},
			expectedStatus: 409,
			expectedError:  "runner test-runner is in use by the environments: project1/env1, project2/env2",
		},
		{
			name:     "Database deletion fails with not found - 404",
			runnerId: runnerId,
			setupMocks: func(db *mockmodel.MockDatabaser, vlt *mock_vault.MockVaultClientInterface) {
				runner := &model.Runner{
					Id:         runnerId,
					OrgId:      orgId,
					RunnerType: string(RunnerTypeKubernetesGke),
				}
				db.EXPECT().GetRunner(gomock.Any(), gomock.Any(), orgId, runnerId).Return(runner, nil).Times(1)
				db.EXPECT().ListEnvironmentsByRunnerId(gomock.Any(), gomock.Any(), orgId, runnerId, "", getEnvsByRunnerPaginationSize).Return([]model.Environment{}, "", nil).Times(1)
				db.EXPECT().DeleteRunner(gomock.Any(), gomock.Any(), orgId, runnerId).Return(model.NewErrNotFound("runner not found")).Times(1)
			},
			expectedStatus: 404,
			expectedError:  "runner not found",
		},
		{
			name:     "Vault secret not found (should not fail)",
			runnerId: runnerId,
			setupMocks: func(db *mockmodel.MockDatabaser, vlt *mock_vault.MockVaultClientInterface) {
				runner := &model.Runner{
					Id:         runnerId,
					OrgId:      orgId,
					RunnerType: string(RunnerTypeKubernetes),
				}
				db.EXPECT().GetRunner(gomock.Any(), gomock.Any(), orgId, runnerId).Return(runner, nil).Times(1)
				db.EXPECT().ListEnvironmentsByRunnerId(gomock.Any(), gomock.Any(), orgId, runnerId, "", getEnvsByRunnerPaginationSize).Return([]model.Environment{}, "", nil).Times(1)
				db.EXPECT().DeleteRunner(gomock.Any(), gomock.Any(), orgId, runnerId).Return(nil).Times(1)
				vlt.EXPECT().DeleteSecret(gomock.Any(), fmt.Sprintf("/platform-orchestrator/orgs/%s/runners/%s", orgId, runnerId)).Return(vault.ErrSecretNotFound).Times(1)
			},
			expectedStatus: 204,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := s.Database.(*mockmodel.MockDatabaser)
			vlt := s.Vault.(*mock_vault.MockVaultClientInterface)

			tc.setupMocks(db, vlt)

			userId := userid.NewHumanUserId()
			mockIamClient := s.IamClient.(*mockorchestratoriam.MockClientWithResponsesInterface)
			mockIamClient.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
				UserId: userId,
				Checks: []orchestratoriam.ResourcePermissionCheck{authz.OrgCheck(orgId, authz.PermissionRunnerWrite)},
			}).Return(&orchestratoriam.InternalAuthorizeResponse{
				HTTPResponse: &http.Response{StatusCode: http.StatusNoContent},
			}, nil)

			req := httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/orgs/%s/runners/%s", orgId, tc.runnerId), nil)
			req.Header.Set("From", userId.String())
			resp := httptest.NewRecorder()
			e.ServeHTTP(resp, req)

			b, _ := io.ReadAll(resp.Result().Body)
			require.Equal(t, tc.expectedStatus, resp.Result().StatusCode, string(b))

			if tc.expectedError != "" {
				require.Contains(t, string(b), tc.expectedError)
			}
		})
	}
}
