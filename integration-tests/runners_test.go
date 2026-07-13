package integrationtests

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/vault/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"

	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/genclient"
)

const (
	gkeRunnerId = "my-gke-runner"
	k8sRunnerId = "my-vanilla-runner"
)

var remoteRunnerPubKey = getRemoteRunnerPublicKey()

func TestRunners(t *testing.T) {
	t.Parallel()
	client := MustServerClient(t)
	internalClient := MustInternalServerClient(t)
	orgId := MustCreateOrg(t, internalClient).Id

	t.Run("empty list", func(t *testing.T) {
		res, err := client.ListRunnersWithResponse(t.Context(), orgId, &genclient.ListRunnersParams{})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.Empty(t, res.JSON200.Items)
			assert.Empty(t, res.JSON200.NextPageToken)
		}
	})
	vltClient := MustCreateVaultClient(t, os.Getenv("VAULT_URL"), os.Getenv("VAULT_TOKEN"))

	pj := MustCreateProject(t, client, orgId, "project-1")
	envType := MustCreateEnvType(t, client, orgId, "env-type-1")

	var createdRunner genclient.Runner
	t.Run("create runner - gke", func(t *testing.T) {
		cfg := new(genclient.RunnerConfiguration)
		require.NoError(t, cfg.FromK8sGkeRunnerConfiguration(genclient.K8sGkeRunnerConfiguration{
			Cluster: genclient.K8sRunnerGkeCluster{
				Name:      "rinsewind",
				ProjectId: "my-gcp-project-id",
				Location:  "eu-west-2a",
				Auth: genclient.K8sRunnerGcpTemporaryAuth{
					GcpAudience:       "boo",
					GcpServiceAccount: "gcp@google.com",
				},
			},
			Job: genclient.K8sRunnerJobConfig{
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
					}}),
			},
			Type: genclient.RunnerTypeKubernetesGke,
		}))

		res, err := client.CreateRunnerWithResponse(t.Context(), orgId, genclient.CreateRunnerJSONRequestBody{
			Id:                        gkeRunnerId,
			RunnerConfiguration:       *cfg,
			StateStorageConfiguration: *getKubernetesStateStorageConfiguration(t),
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body)) {
			createdRunner = *res.JSON201

			expectedRunnerCfg := new(genclient.RunnerConfiguration)
			require.NoError(t, expectedRunnerCfg.FromK8sGkeRunnerConfiguration(genclient.K8sGkeRunnerConfiguration{
				Cluster: genclient.K8sRunnerGkeCluster{
					Name:       "rinsewind",
					ProjectId:  "my-gcp-project-id",
					Location:   "eu-west-2a",
					InternalIp: ref.Ref(false),
					Auth: genclient.K8sRunnerGcpTemporaryAuth{
						GcpAudience:       "boo",
						GcpServiceAccount: "gcp@google.com",
					},
				},
				Job: genclient.K8sRunnerJobConfig{
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
						}}),
				},
				// Type: genclient.RunnerTypeKubernetesGke,
			}))
			assert.NotEmpty(t, res.JSON201.CreatedAt)
			assert.NotEmpty(t, res.JSON201.UpdatedAt)
			res.JSON201.CreatedAt = time.Time{}
			res.JSON201.UpdatedAt = time.Time{}
			actualGkeRunnerCfg, err := res.JSON201.RunnerConfiguration.AsK8sGkeRunnerConfiguration()
			require.NoError(t, err)
			require.NoError(t, res.JSON201.RunnerConfiguration.FromK8sGkeRunnerConfiguration(actualGkeRunnerCfg))
			assert.Equal(t, genclient.Runner{
				OrgId:                     orgId,
				Id:                        "my-gke-runner",
				RunnerConfiguration:       *expectedRunnerCfg,
				StateStorageConfiguration: *getKubernetesStateStorageConfiguration(t),
				UpdatedAt:                 time.Time{},
			}, *res.JSON201)

		}
	})

	t.Run("cannot create runner with same id", func(t *testing.T) {
		cfg := new(genclient.RunnerConfiguration)
		require.NoError(t, cfg.FromK8sGkeRunnerConfiguration(genclient.K8sGkeRunnerConfiguration{
			Cluster: genclient.K8sRunnerGkeCluster{
				Name:      "rinsewind",
				ProjectId: "my-gcp-project-id",
				Location:  "eu-west-2a",
				Auth: genclient.K8sRunnerGcpTemporaryAuth{
					GcpAudience:       "boo",
					GcpServiceAccount: "gcp@google.com",
				},
			},
			Job: genclient.K8sRunnerJobConfig{
				Namespace:      "default",
				ServiceAccount: "platform-orchestrator-runner",
			},
			Type: genclient.RunnerTypeKubernetesGke,
		}))

		stateCfg := new(genclient.StateStorageConfiguration)
		require.NoError(t, stateCfg.FromK8sStorageConfiguration(genclient.K8sStorageConfiguration{Namespace: "platform-orchestrator-state-namespace"}))

		res, err := client.CreateRunnerWithResponse(t.Context(), orgId, genclient.CreateRunnerJSONRequestBody{
			Id:                        gkeRunnerId,
			Description:               ref.Ref("GKE runner"),
			RunnerConfiguration:       *cfg,
			StateStorageConfiguration: *stateCfg,
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusConflict, res.StatusCode(), string(res.Body)) {
			assert.Equal(t, "HTTP-409", res.JSON409.Error)
			assert.Equal(t, "runner with id my-gke-runner already exists", res.JSON409.Message)
		}
	})

	t.Run("can list runner", func(t *testing.T) {
		res, err := client.ListRunnersWithResponse(t.Context(), orgId, &genclient.ListRunnersParams{})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) && assert.Len(t, res.JSON200.Items, 1) {
			item := res.JSON200.Items[0]
			assert.NotEmpty(t, item.CreatedAt)
			assert.NotEmpty(t, item.UpdatedAt)
			item.CreatedAt = time.Time{}
			item.UpdatedAt = time.Time{}
			assert.Equal(t, genclient.RunnerSummary{
				OrgId:       orgId,
				Id:          createdRunner.Id,
				Description: createdRunner.Description,
				RunnerConfiguration: &genclient.RunnerConfigurationSummary{
					Type: genclient.RunnerTypeKubernetesGke,
				},
				StateStorageConfiguration: &genclient.StateStorageConfigurationSummary{
					Type: genclient.StateStorageTypeKubernetes,
				},
				UpdatedAt: time.Time{},
			}, item)
			assert.Empty(t, res.JSON200.NextPageToken)
		}
	})

	var updatedRunner genclient.Runner
	t.Run("can update runner - job update", func(t *testing.T) {
		stateCfgUpd := new(genclient.StateStorageConfiguration)
		require.NoError(t, stateCfgUpd.FromK8sStorageConfiguration(genclient.K8sStorageConfiguration{Namespace: "another-state-namespace"}))
		descriptionUpd := ref.Ref("GKE runner UPD")
		updCfg := new(genclient.RunnerConfigurationUpdate)
		require.NoError(t, updCfg.FromK8sGkeRunnerConfigurationUpdateBody(genclient.K8sGkeRunnerConfigurationUpdateBody{
			Cluster: &genclient.K8sRunnerGkeCluster{
				Name:      "rinsewind",
				ProjectId: "my-gcp-project-id",
				Location:  "eu-west-2a",
				Auth: genclient.K8sRunnerGcpTemporaryAuth{
					GcpAudience:       "boo",
					GcpServiceAccount: "gcp@google.com",
				},
			},
			Job: &genclient.K8sRunnerJobConfig{
				Namespace:      "default",
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
					}}),
			},
		}))
		res, err := client.UpdateRunnerWithResponse(t.Context(), orgId, createdRunner.Id, genclient.UpdateRunnerJSONRequestBody{
			Description:               descriptionUpd,
			StateStorageConfiguration: stateCfgUpd,
			RunnerConfiguration:       updCfg,
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			updatedRunner = *res.JSON200
			assert.NotEmpty(t, updatedRunner.UpdatedAt)
			assert.Equal(t, descriptionUpd, updatedRunner.Description)
			updatedK8sStateCfgBackend, _ := updatedRunner.StateStorageConfiguration.AsK8sStorageConfiguration()
			assert.Equal(t, genclient.K8sStorageConfiguration{Namespace: "another-state-namespace", Type: genclient.StateStorageTypeKubernetes}, updatedK8sStateCfgBackend)
			actualRunnerCfg, err := updatedRunner.RunnerConfiguration.AsK8sGkeRunnerConfiguration()
			require.NoError(t, err)
			assert.Equal(t, genclient.K8sGkeRunnerConfiguration{
				Cluster: genclient.K8sRunnerGkeCluster{
					Name:       "rinsewind",
					ProjectId:  "my-gcp-project-id",
					Location:   "eu-west-2a",
					InternalIp: ref.Ref(false),
					Auth: genclient.K8sRunnerGcpTemporaryAuth{
						GcpAudience:       "boo",
						GcpServiceAccount: "gcp@google.com",
					},
				},
				Job: genclient.K8sRunnerJobConfig{
					Namespace:      "default",
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
						}}),
				},
				Type: genclient.RunnerTypeKubernetesGke,
			}, actualRunnerCfg)
		}
	})

	t.Run("can update runner - cluster update", func(t *testing.T) {
		cfg := new(genclient.RunnerConfigurationUpdate)
		require.NoError(t, cfg.FromK8sGkeRunnerConfigurationUpdateBody(genclient.K8sGkeRunnerConfigurationUpdateBody{
			Cluster: &genclient.K8sRunnerGkeCluster{
				Name:      "rinsewind",
				ProjectId: "my-fixed-gcp-project-id",
				Location:  "eu-west-2a",
				Auth: genclient.K8sRunnerGcpTemporaryAuth{
					GcpAudience:       "boo",
					GcpServiceAccount: "gcp@google.com",
				},
			},
		}))
		res, err := client.UpdateRunnerWithResponse(t.Context(), orgId, createdRunner.Id, genclient.UpdateRunnerJSONRequestBody{
			RunnerConfiguration: cfg,
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			updatedRunner = *res.JSON200
			assert.NotEmpty(t, updatedRunner.UpdatedAt)
			assert.Equal(t, ref.Ref("GKE runner UPD"), updatedRunner.Description)
			updatedK8sStateCfgBackend, _ := updatedRunner.StateStorageConfiguration.AsK8sStorageConfiguration()
			assert.Equal(t, genclient.K8sStorageConfiguration{Namespace: "another-state-namespace", Type: genclient.StateStorageTypeKubernetes}, updatedK8sStateCfgBackend)
			actualRunnerCfg, err := updatedRunner.RunnerConfiguration.AsK8sGkeRunnerConfiguration()
			require.NoError(t, err)
			assert.Equal(t, genclient.K8sGkeRunnerConfiguration{
				Cluster: genclient.K8sRunnerGkeCluster{
					Name:       "rinsewind",
					ProjectId:  "my-fixed-gcp-project-id",
					Location:   "eu-west-2a",
					InternalIp: ref.Ref(false),
					Auth: genclient.K8sRunnerGcpTemporaryAuth{
						GcpAudience:       "boo",
						GcpServiceAccount: "gcp@google.com",
					},
				},
				Job: genclient.K8sRunnerJobConfig{
					Namespace:      "default",
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
						}}),
				},
				Type: genclient.RunnerTypeKubernetesGke,
			}, actualRunnerCfg)
		}
	})

	t.Run("can get runner", func(t *testing.T) {
		res, err := client.GetRunnerWithResponse(t.Context(), orgId, updatedRunner.Id)
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			item := res.JSON200
			assert.NotEmpty(t, item.CreatedAt)
			assert.NotEmpty(t, item.UpdatedAt)
			item.CreatedAt = time.Time{}
			item.UpdatedAt = time.Time{}
			assert.Equal(t, &genclient.Runner{
				OrgId:                     orgId,
				Id:                        updatedRunner.Id,
				Description:               updatedRunner.Description,
				RunnerConfiguration:       updatedRunner.RunnerConfiguration,
				StateStorageConfiguration: updatedRunner.StateStorageConfiguration,
				UpdatedAt:                 time.Time{},
				CreatedAt:                 time.Time{},
			}, item)
		}
	})

	t.Run("no secret runner configuration is stored for a runner without secrets", func(t *testing.T) {
		res, err := internalClient.GetInternalRunnerWithResponse(t.Context(), orgId, updatedRunner.Id)
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			item := res.JSON200
			assert.NotEmpty(t, item.CreatedAt)
			assert.NotEmpty(t, item.UpdatedAt)
			item.CreatedAt = time.Time{}
			item.UpdatedAt = time.Time{}
			internalRunnerCfg, _ := updatedRunner.RunnerConfiguration.AsK8sGkeRunnerConfiguration()
			expectedCfg := new(genclient.RunnerConfiguration)
			require.NoError(t, expectedCfg.FromK8sGkeRunnerConfiguration(internalRunnerCfg))
			actualItemRunnerCfg, err := item.RunnerConfiguration.AsK8sGkeRunnerConfiguration()
			require.NoError(t, err)
			require.NoError(t, item.RunnerConfiguration.FromK8sGkeRunnerConfiguration(actualItemRunnerCfg))
			assert.Equal(t, &genclient.InternalRunner{
				OrgId:                     orgId,
				Id:                        updatedRunner.Id,
				Description:               updatedRunner.Description,
				RunnerConfiguration:       *expectedCfg,
				StateStorageConfiguration: updatedRunner.StateStorageConfiguration,
				UpdatedAt:                 time.Time{},
				CreatedAt:                 time.Time{},
			}, item)
		}
	})

	t.Run("create runner - k8s", func(t *testing.T) {
		cfg := new(genclient.RunnerConfiguration)
		require.NoError(t, cfg.FromK8sRunnerConfiguration(genclient.K8sRunnerConfiguration{
			Cluster: genclient.K8sRunnerK8sCluster{
				ClusterData: genclient.K8sRunnerK8sClusterClusterData{
					Server: "server",
				},
				Auth: genclient.K8sRunnerK8sClusterAuth{
					ServiceAccountToken: ref.Ref("t0k3n"),
				},
			},
			Job: genclient.K8sRunnerJobConfig{
				Namespace:      "hum",
				ServiceAccount: "platform-orchestrator-runner",
			},
			Type: genclient.RunnerTypeKubernetes,
		}))

		res, err := client.CreateRunnerWithResponse(t.Context(), orgId, genclient.CreateRunnerJSONRequestBody{
			Id:                        k8sRunnerId,
			Description:               ref.Ref("Vanilla runner"),
			RunnerConfiguration:       *cfg,
			StateStorageConfiguration: *getKubernetesStateStorageConfiguration(t),
		})

		if assert.NoError(t, err) && assert.Equal(t, http.StatusCreated, res.StatusCode()) {
			actualRunnerCfg, err := res.JSON201.RunnerConfiguration.AsK8sRunnerConfiguration()
			require.NoError(t, err)
			assert.Equal(t, genclient.K8sRunnerConfiguration{
				Cluster: genclient.K8sRunnerK8sCluster{
					ClusterData: genclient.K8sRunnerK8sClusterClusterData{
						Server: "server",
					},
					Auth: genclient.K8sRunnerK8sClusterAuth{
						ServiceAccountToken: ref.Ref("SECRET"),
					},
				},
				Job: genclient.K8sRunnerJobConfig{
					Namespace:      "hum",
					ServiceAccount: "platform-orchestrator-runner",
				},
				Type: genclient.RunnerTypeKubernetes,
			}, actualRunnerCfg)
		}

	})

	var k8sRunner genclient.Runner
	t.Run("can know the secrets set in the configuration when we get the runner", func(t *testing.T) {
		res, err := client.GetRunnerWithResponse(t.Context(), orgId, k8sRunnerId)
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode()) {
			expectedCfg := new(genclient.RunnerConfiguration)
			require.NoError(t, expectedCfg.FromK8sRunnerConfiguration(genclient.K8sRunnerConfiguration{
				Cluster: genclient.K8sRunnerK8sCluster{
					ClusterData: genclient.K8sRunnerK8sClusterClusterData{
						Server: "server",
					},
					Auth: genclient.K8sRunnerK8sClusterAuth{
						ServiceAccountToken: ref.Ref("SECRET"),
					},
				},
				Job: genclient.K8sRunnerJobConfig{
					Namespace:      "hum",
					ServiceAccount: "platform-orchestrator-runner",
				},
				Type: genclient.RunnerTypeKubernetes,
			}))

			stateCfg := new(genclient.StateStorageConfiguration)
			k8sRunner = *res.JSON200
			actualRunnerCfg, err := k8sRunner.RunnerConfiguration.AsK8sRunnerConfiguration()
			require.NoError(t, err)
			require.NoError(t, k8sRunner.RunnerConfiguration.FromK8sRunnerConfiguration(actualRunnerCfg))
			require.NoError(t, stateCfg.FromK8sStorageConfiguration(genclient.K8sStorageConfiguration{Namespace: "platform-orchestrator-state-namespace"}))
			assert.Equal(t, genclient.Runner{
				Description:               ref.Ref("Vanilla runner"),
				RunnerConfiguration:       *expectedCfg,
				StateStorageConfiguration: *stateCfg,
				CreatedAt:                 res.JSON200.CreatedAt,
				Id:                        k8sRunnerId,
				OrgId:                     orgId,
				UpdatedAt:                 res.JSON200.UpdatedAt,
			}, k8sRunner)
		}
	})

	t.Run("can retrieve the secrets path in the configuration when we get the runner with the internal endpoint", func(t *testing.T) {
		res, err := internalClient.GetInternalRunnerWithResponse(t.Context(), orgId, k8sRunnerId)
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode()) {

			actualRunnerCfg, err := res.JSON200.RunnerConfiguration.AsK8sRunnerConfiguration()
			require.NoError(t, err)
			require.NoError(t, res.JSON200.RunnerConfiguration.FromK8sRunnerConfiguration(actualRunnerCfg))
			assert.Equal(t, &genclient.InternalRunner{
				Description:               k8sRunner.Description,
				RunnerConfiguration:       k8sRunner.RunnerConfiguration,
				StateStorageConfiguration: k8sRunner.StateStorageConfiguration,
				CreatedAt:                 res.JSON200.CreatedAt,
				Id:                        k8sRunnerId,
				OrgId:                     orgId,
				UpdatedAt:                 res.JSON200.UpdatedAt,
				RunnerConfigurationSecret: genclient.ConfigurationSecret{
					Path:    fmt.Sprintf("/platform-orchestrator/orgs/%s/runners/%s", orgId, k8sRunnerId),
					Version: 1,
				},
			}, res.JSON200)

			s, err := vltClient.KVv2("secret").GetVersion(t.Context(), fmt.Sprintf("/platform-orchestrator/orgs/%s/runners/%s", orgId, k8sRunnerId), 1)
			if assert.NoError(t, err) && assert.NotNil(t, s) {
				assert.Equal(t, map[string]interface{}{
					"cluster": map[string]interface{}{
						"auth": map[string]interface{}{"service_account_token": "t0k3n"},
						"cluster_data": map[string]interface{}{
							"server":                     "",
							"certificate_authority_data": "",
						},
					},
					"job": map[string]interface{}{
						"namespace":       "",
						"service_account": "",
					}, "type": ""}, s.Data)
			}
		}
	})

	t.Run("can update runner with new credentials and pod template", func(t *testing.T) {
		cfg := new(genclient.RunnerConfigurationUpdate)
		updatedJob := &genclient.K8sRunnerJobConfig{
			Namespace:      "d3f4ult",
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
				}}),
		}
		require.NoError(t, cfg.FromK8sRunnerConfigurationUpdateBody(genclient.K8sRunnerConfigurationUpdateBody{
			Cluster: &genclient.K8sRunnerK8sCluster{
				ClusterData: genclient.K8sRunnerK8sClusterClusterData{
					Server: "server",
				},
				Auth: genclient.K8sRunnerK8sClusterAuth{
					ClientKeyData: ref.Ref("0th3rtk"),
				},
			},
			Job:  updatedJob,
			Type: genclient.RunnerTypeKubernetes,
		}))

		res, err := client.UpdateRunnerWithResponse(t.Context(), orgId, k8sRunnerId, genclient.UpdateRunnerJSONRequestBody{
			RunnerConfiguration: cfg,
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			k8sRunner = *res.JSON200
			actualRunnerCfg, err := k8sRunner.RunnerConfiguration.AsK8sRunnerConfiguration()
			require.NoError(t, err)
			assert.Equal(t, genclient.K8sRunnerConfiguration{
				Cluster: genclient.K8sRunnerK8sCluster{
					ClusterData: genclient.K8sRunnerK8sClusterClusterData{
						Server: "server",
					},
					Auth: genclient.K8sRunnerK8sClusterAuth{
						ClientKeyData: ref.Ref("SECRET"),
					},
				},
				Job:  *updatedJob,
				Type: genclient.RunnerTypeKubernetes,
			}, actualRunnerCfg)
		}
	})

	t.Run("can retrieve the secrets path in the configuration when we get the runner with the internal endpoint", func(t *testing.T) {
		res, err := internalClient.GetInternalRunnerWithResponse(t.Context(), orgId, k8sRunnerId)
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode()) {
			assert.Equal(t, &genclient.InternalRunner{
				Description:               k8sRunner.Description,
				RunnerConfiguration:       k8sRunner.RunnerConfiguration,
				StateStorageConfiguration: k8sRunner.StateStorageConfiguration,
				CreatedAt:                 res.JSON200.CreatedAt,
				Id:                        k8sRunnerId,
				OrgId:                     orgId,
				UpdatedAt:                 res.JSON200.UpdatedAt,
				RunnerConfigurationSecret: genclient.ConfigurationSecret{
					Path:    fmt.Sprintf("/platform-orchestrator/orgs/%s/runners/%s", orgId, k8sRunnerId),
					Version: 2,
				},
			}, res.JSON200)

			s, err := vltClient.KVv2("secret").GetVersion(t.Context(), fmt.Sprintf("/platform-orchestrator/orgs/%s/runners/%s", orgId, k8sRunnerId), 2)
			if assert.NoError(t, err) && assert.NotNil(t, s) {
				assert.Equal(t, map[string]interface{}{
					"cluster": map[string]interface{}{
						"auth": map[string]interface{}{"client_key_data": "0th3rtk"},
						"cluster_data": map[string]interface{}{
							"server":                     "",
							"certificate_authority_data": "",
						},
					},
					"job": map[string]interface{}{
						"namespace":       "",
						"service_account": "",
					}, "type": ""}, s.Data)
			}
		}
	})

	t.Run("there are no rule for the 1st created runner yet", func(t *testing.T) {
		res, err := client.ListRunnerRulesInOrgWithResponse(t.Context(), orgId, &genclient.ListRunnerRulesInOrgParams{
			ByRunnerId: ref.Ref(gkeRunnerId),
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.Empty(t, res.JSON200.Items)
		}
	})

	var gkeRuleId uuid.UUID
	t.Run("can create an org wide rule for the 1st created runner", func(t *testing.T) {
		res, err := client.CreateRunnerRuleInOrgWithResponse(t.Context(), orgId, genclient.CreateRunnerRuleInOrgJSONRequestBody{
			RunnerId: gkeRunnerId,
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body)) {
			assert.NotEmpty(t, res.JSON201.CreatedAt)
			assert.NotEmpty(t, res.JSON201.Id)
			res.JSON201.CreatedAt = time.Time{}
			assert.Equal(t, genclient.RunnerRule{
				OrgId:     orgId,
				Id:        res.JSON201.Id,
				RunnerId:  gkeRunnerId,
				ProjectId: "",
				EnvTypeId: "",
				CreatedAt: time.Time{},
			}, *res.JSON201)
			gkeRuleId = res.JSON201.Id
		}
	})

	t.Run("can get runner rule", func(t *testing.T) {
		res, err := client.GetRunnerRuleInOrgWithResponse(t.Context(), orgId, gkeRuleId)
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.NotEmpty(t, res.JSON200.CreatedAt)
			res.JSON200.CreatedAt = time.Time{}
			assert.Equal(t, genclient.RunnerRule{
				OrgId:     orgId,
				Id:        gkeRuleId,
				RunnerId:  gkeRunnerId,
				ProjectId: "",
				EnvTypeId: "",
				CreatedAt: time.Time{},
			}, *res.JSON200)
		}
	})

	t.Run("the runner is matched with an environment", func(t *testing.T) {
		{
			res, err := client.CreateEnvironmentWithResponse(t.Context(), orgId, pj.Id, genclient.CreateEnvironmentJSONRequestBody{
				EnvTypeId: envType.Id,
				Id:        "test-env",
			})
			require.NoError(t, err)
			require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
		}
		{
			res, err := client.UpdateRunnerInAnEnvironmentWithResponse(t.Context(), orgId, pj.Id, "test-env", &genclient.UpdateRunnerInAnEnvironmentParams{})
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body))
			assert.Equal(t, gkeRunnerId, res.JSON200.RunnerId)
		}
	})

	t.Run("cannot create the same rule on another runner", func(t *testing.T) {
		res, err := client.CreateRunnerRuleInOrgWithResponse(t.Context(), orgId, genclient.CreateRunnerRuleInOrgJSONRequestBody{
			RunnerId:  k8sRunnerId,
			ProjectId: ref.Ref(""),
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusConflict, res.StatusCode(), string(res.Body)) {
			assert.Equal(t, "HTTP-409", res.JSON409.Error)
			assert.Equal(t, fmt.Sprintf("runner rule conflicts with existing rule '%s' for runner '%s'", gkeRuleId, gkeRunnerId), res.JSON409.Message)
		}
	})

	t.Run("can create another rule on the same runner", func(t *testing.T) {
		res, err := client.CreateRunnerRuleInOrgWithResponse(t.Context(), orgId, genclient.CreateRunnerRuleInOrgJSONRequestBody{
			RunnerId:  gkeRunnerId,
			ProjectId: &pj.Id,
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body)) {
			assert.NotEmpty(t, res.JSON201.CreatedAt)
			assert.NotEmpty(t, res.JSON201.Id)
			res.JSON201.CreatedAt = time.Time{}
			assert.Equal(t, genclient.RunnerRule{
				OrgId:     orgId,
				Id:        res.JSON201.Id,
				RunnerId:  gkeRunnerId,
				ProjectId: pj.Id,
				EnvTypeId: "",
				CreatedAt: time.Time{},
			}, *res.JSON201)
		}
	})

	t.Run("can create a more specific rule on another runner", func(t *testing.T) {
		res, err := client.CreateRunnerRuleInOrgWithResponse(t.Context(), orgId, genclient.CreateRunnerRuleInOrgJSONRequestBody{
			RunnerId:  k8sRunnerId,
			ProjectId: &pj.Id,
			EnvTypeId: &envType.Id,
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body)) {
			assert.NotEmpty(t, res.JSON201.CreatedAt)
			assert.NotEmpty(t, res.JSON201.Id)
			res.JSON201.CreatedAt = time.Time{}
			assert.Equal(t, genclient.RunnerRule{
				OrgId:     orgId,
				Id:        res.JSON201.Id,
				RunnerId:  k8sRunnerId,
				ProjectId: pj.Id,
				EnvTypeId: envType.Id,
				CreatedAt: time.Time{},
			}, *res.JSON201)
		}
	})

	t.Run("cannot create the same rule again", func(t *testing.T) {
		res, err := client.CreateRunnerRuleInOrgWithResponse(t.Context(), orgId, genclient.CreateRunnerRuleInOrgJSONRequestBody{
			RunnerId:  gkeRunnerId,
			ProjectId: &pj.Id,
			EnvTypeId: &envType.Id,
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusConflict, res.StatusCode(), string(res.Body)) {
			assert.Equal(t, "HTTP-409", res.JSON409.Error)
			assert.Contains(t, res.JSON409.Message, "runner rule conflicts with existing rule")
		}
	})

	t.Run("can list runner rules by project id", func(t *testing.T) {
		res, err := client.ListRunnerRulesInOrgWithResponse(t.Context(), orgId, &genclient.ListRunnerRulesInOrgParams{ByProjectId: ref.Ref(pj.Id)})
		require.NoError(t, err)
		if assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.Len(t, res.JSON200.Items, 3)
			assert.True(t, slices.ContainsFunc(res.JSON200.Items, func(item genclient.RunnerRule) bool {
				return item.OrgId == orgId && item.Id == gkeRuleId && item.RunnerId == gkeRunnerId && item.ProjectId == "" && item.EnvTypeId == ""
			}))
		}
	})

	t.Run("can list runner rules by runner id", func(t *testing.T) {
		res, err := client.ListRunnerRulesInOrgWithResponse(t.Context(), orgId, &genclient.ListRunnerRulesInOrgParams{ByRunnerId: ref.Ref(gkeRunnerId)})
		require.NoError(t, err)
		if assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.Len(t, res.JSON200.Items, 2)
			assert.True(t, slices.ContainsFunc(res.JSON200.Items, func(item genclient.RunnerRule) bool {
				return item.OrgId == orgId && item.Id == gkeRuleId && item.RunnerId == gkeRunnerId && item.ProjectId == "" && item.EnvTypeId == ""
			}))
		}
	})

	t.Run("can delete runner rule", func(t *testing.T) {
		res, err := client.DeleteRunnerRuleInOrgWithResponse(t.Context(), orgId, gkeRuleId)
		require.NoError(t, err)
		assert.Equal(t, http.StatusNoContent, res.StatusCode(), string(res.Body))
	})

	t.Run("rule is not in the list runner rules anymore", func(t *testing.T) {
		res, err := client.ListRunnerRulesInOrgWithResponse(t.Context(), orgId, &genclient.ListRunnerRulesInOrgParams{ByRunnerId: ref.Ref(gkeRunnerId)})
		require.NoError(t, err)
		if assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.Len(t, res.JSON200.Items, 1)
			assert.False(t, slices.ContainsFunc(res.JSON200.Items, func(item genclient.RunnerRule) bool {
				return item.OrgId == orgId && item.Id == gkeRuleId && item.RunnerId == gkeRunnerId && item.ProjectId == "" && item.EnvTypeId == ""
			}))
		}
	})

	t.Run("cannot delete the runner as it is used by an environment", func(t *testing.T) {
		res, err := client.DeleteRunnerWithResponse(t.Context(), orgId, gkeRunnerId)
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusConflict, res.StatusCode(), string(res.Body))
			assert.Equal(t, fmt.Sprintf("runner %s is in use by the environments: %s/%s", gkeRunnerId, pj.Id, "test-env"), res.JSON409.Message)
		}
	})

	t.Run("can delete the runner after deleting the environment", func(t *testing.T) {
		res, err := internalClient.InternalForceDeleteEnvironmentWithResponse(t.Context(), orgId, pj.Id, "test-env", &genclient.InternalForceDeleteEnvironmentParams{})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusNoContent, res.StatusCode(), string(res.Body)) {
			res, err := client.DeleteRunnerWithResponse(t.Context(), orgId, gkeRunnerId)
			if assert.NoError(t, err) && assert.Equal(t, http.StatusNoContent, res.StatusCode(), string(res.Body)) {
				_, err := vltClient.KVv2("secret").Get(t.Context(), fmt.Sprintf("/platform-orchestrator/orgs/%s/runners/%s", orgId, gkeRunnerId))
				require.ErrorIs(t, err, api.ErrSecretNotFound)
			}
		}
	})

	t.Run("cannot create a rule on a deleted runner", func(t *testing.T) {
		res, err := client.CreateRunnerRuleInOrgWithResponse(t.Context(), orgId, genclient.CreateRunnerRuleInOrgJSONRequestBody{
			RunnerId: gkeRunnerId,
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusConflict, res.StatusCode(), string(res.Body)) {
			assert.Equal(t, "HTTP-409", res.JSON409.Error)
			assert.Contains(t, fmt.Sprintf("runner '%s' not found", gkeRunnerId), res.JSON409.Message)
		}
	})

	t.Run("delete the runner means remove also cluster credentials", func(t *testing.T) {
		res, err := client.DeleteRunnerWithResponse(t.Context(), orgId, k8sRunnerId)
		if assert.NoError(t, err) && assert.Equal(t, http.StatusNoContent, res.StatusCode()) {
			_, err := vltClient.KVv2("secret").Get(t.Context(), fmt.Sprintf("/platform-orchestrator/orgs/%s/runners/%s", orgId, k8sRunnerId))
			require.ErrorIs(t, err, api.ErrSecretNotFound)
		}
	})

	t.Run("cannot create remote runner with cluster configuration", func(t *testing.T) {
		bodyRequest := map[string]interface{}{
			"id": "remote-runner",
			"runner_configuration": map[string]interface{}{
				"cluster": map[string]interface{}{
					"cluster_data": map[string]interface{}{
						"server": "server",
					},
					"auth": map[string]interface{}{
						"service_account_token": "t0k3n",
					},
				},
				"job": map[string]interface{}{
					"namespace":       "default",
					"service_account": "platform-orchestrator-runner",
				},
				"key":  remoteRunnerPubKey,
				"type": genclient.RunnerTypeKubernetesAgent,
			},
			"state_storage_configuration": map[string]interface{}{
				"namespace": "platform-orchestrator-state-namespace",
				"type":      genclient.StateStorageTypeKubernetes,
			},
		}
		encoded, _ := json.Marshal(bodyRequest)

		res, err := client.CreateRunnerWithBodyWithResponse(t.Context(), orgId, "application/json", bytes.NewReader(encoded))
		if assert.NoError(t, err) && assert.Equal(t, http.StatusBadRequest, res.StatusCode(), string(res.Body)) {
			assert.Equal(t, "the supplied runner configuration does not match the specified runner type kubernetes-agent: json: unknown field \"cluster\"", res.JSON400.Message)
		}
	})

	t.Run("cannot create a remote runner with an rsa public key", func(t *testing.T) {
		remoteCfg := new(genclient.RunnerConfiguration)
		require.NoError(t, remoteCfg.FromK8sAgentRunnerConfiguration(genclient.K8sAgentRunnerConfiguration{
			Job: genclient.K8sRunnerJobConfig{
				Namespace:      "default",
				ServiceAccount: "platform-orchestrator-runner",
			},
			Key:  generateRSAPEM(t),
			Type: genclient.RunnerTypeKubernetesAgent,
		}))
		res, err := client.CreateRunnerWithResponse(t.Context(), orgId, genclient.CreateRunnerJSONRequestBody{
			Id:                        "remote-runner-rsa",
			RunnerConfiguration:       *remoteCfg,
			StateStorageConfiguration: *getKubernetesStateStorageConfiguration(t),
		})
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusBadRequest, res.StatusCode(), string(res.Body))
			assert.Contains(t, res.JSON400.Message, "maximum string length is 200")
		}
	})

	t.Run("cannot create a remote runner with a fake key", func(t *testing.T) {
		// Generate a fake PEM public key string with correct prefix and length
		const fakeKeyLen = 100
		fakeKeyBody := ""
		for len(fakeKeyBody) < fakeKeyLen {
			fakeKeyBody += "A"
		}
		fakePEM := "-----BEGIN PUBLIC KEY-----\n" + fakeKeyBody + "\n-----END PUBLIC KEY-----\n"

		remoteCfg := new(genclient.RunnerConfiguration)
		require.NoError(t, remoteCfg.FromK8sAgentRunnerConfiguration(genclient.K8sAgentRunnerConfiguration{
			Job: genclient.K8sRunnerJobConfig{
				Namespace:      "default",
				ServiceAccount: "platform-orchestrator-runner",
			},
			Key:  fakePEM,
			Type: genclient.RunnerTypeKubernetesAgent,
		}))
		res, err := client.CreateRunnerWithResponse(t.Context(), orgId, genclient.CreateRunnerJSONRequestBody{
			Id:                        "remote-runner-fake",
			RunnerConfiguration:       *remoteCfg,
			StateStorageConfiguration: *getKubernetesStateStorageConfiguration(t),
		})
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusBadRequest, res.StatusCode(), string(res.Body))
			assert.Contains(t, res.JSON400.Message, "invalid public key")
		}
	})

	t.Run("can create a remote runner", func(t *testing.T) {
		remoteCfg := new(genclient.RunnerConfiguration)
		require.NoError(t, remoteCfg.FromK8sAgentRunnerConfiguration(genclient.K8sAgentRunnerConfiguration{
			Job: genclient.K8sRunnerJobConfig{
				Namespace:      "default",
				ServiceAccount: "platform-orchestrator-runner",
			},
			Key:  remoteRunnerPubKey,
			Type: genclient.RunnerTypeKubernetesAgent,
		}))
		res, err := client.CreateRunnerWithResponse(t.Context(), orgId, genclient.CreateRunnerJSONRequestBody{
			Id:                        "remote-runner",
			RunnerConfiguration:       *remoteCfg,
			StateStorageConfiguration: *getKubernetesStateStorageConfiguration(t),
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body)) {
			actualRemoteCfg, err := res.JSON201.RunnerConfiguration.AsK8sAgentRunnerConfiguration()
			require.NoError(t, err)
			assert.Equal(t, genclient.K8sAgentRunnerConfiguration{
				Job: genclient.K8sRunnerJobConfig{
					Namespace:      "default",
					ServiceAccount: "platform-orchestrator-runner",
				},
				Type: genclient.RunnerTypeKubernetesAgent,
				Key:  remoteRunnerPubKey,
			}, actualRemoteCfg)
		}
	})

	t.Run("can get a remote runner", func(t *testing.T) {
		res, err := client.GetRunnerWithResponse(t.Context(), orgId, "remote-runner")
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			actualRemoteCfg, err := res.JSON200.RunnerConfiguration.AsK8sAgentRunnerConfiguration()
			require.NoError(t, err)
			assert.Equal(t, genclient.K8sAgentRunnerConfiguration{
				Job: genclient.K8sRunnerJobConfig{
					Namespace:      "default",
					ServiceAccount: "platform-orchestrator-runner",
				},
				Type: genclient.RunnerTypeKubernetesAgent,
				Key:  remoteRunnerPubKey,
			}, actualRemoteCfg)
		}
	})

	t.Run("can get a remote runner - internal endpoint", func(t *testing.T) {
		res, err := internalClient.GetInternalRunnerWithResponse(t.Context(), orgId, "remote-runner")
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			actualRemoteCfg, err := res.JSON200.RunnerConfiguration.AsK8sAgentRunnerConfiguration()
			require.NoError(t, err)
			assert.Equal(t, genclient.K8sAgentRunnerConfiguration{
				Job: genclient.K8sRunnerJobConfig{
					Namespace:      "default",
					ServiceAccount: "platform-orchestrator-runner",
				},
				Type: genclient.RunnerTypeKubernetesAgent,
				Key:  remoteRunnerPubKey,
			}, actualRemoteCfg)
		}
	})
	t.Run("can update a remote runner - update job", func(t *testing.T) {
		updateCfg := new(genclient.RunnerConfigurationUpdate)
		require.NoError(t, updateCfg.FromK8sAgentRunnerConfigurationUpdateBody(genclient.K8sAgentRunnerConfigurationUpdateBody{
			Job: &genclient.K8sRunnerJobConfig{
				Namespace:      "platform-orchestrator-runner",
				ServiceAccount: "platform-orchestrator-runner",
			},
			Type: genclient.RunnerTypeKubernetesAgent,
		}))
		res, err := client.UpdateRunnerWithResponse(t.Context(), orgId, "remote-runner", genclient.UpdateRunnerJSONRequestBody{
			RunnerConfiguration: updateCfg,
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			actualRemoteCfg, err := res.JSON200.RunnerConfiguration.AsK8sAgentRunnerConfiguration()
			require.NoError(t, err)
			assert.Equal(t, genclient.K8sAgentRunnerConfiguration{
				Job: genclient.K8sRunnerJobConfig{
					Namespace:      "platform-orchestrator-runner",
					ServiceAccount: "platform-orchestrator-runner",
				},
				Type: genclient.RunnerTypeKubernetesAgent,
				Key:  remoteRunnerPubKey,
			}, actualRemoteCfg)
			assert.Equal(t, "remote-runner", res.JSON200.Id)
		}
	})

	t.Run("can update a remote runner - update pub key", func(t *testing.T) {
		updatedPubKey := getRemoteRunnerPublicKey()
		updateCfg := new(genclient.RunnerConfigurationUpdate)
		require.NoError(t, updateCfg.FromK8sAgentRunnerConfigurationUpdateBody(genclient.K8sAgentRunnerConfigurationUpdateBody{
			Key:  ref.Ref(updatedPubKey),
			Type: genclient.RunnerTypeKubernetesAgent,
		}))
		res, err := client.UpdateRunnerWithResponse(t.Context(), orgId, "remote-runner", genclient.UpdateRunnerJSONRequestBody{
			RunnerConfiguration: updateCfg,
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			actualRemoteCfg, err := res.JSON200.RunnerConfiguration.AsK8sAgentRunnerConfiguration()
			require.NoError(t, err)
			assert.Equal(t, genclient.K8sAgentRunnerConfiguration{
				Job: genclient.K8sRunnerJobConfig{
					Namespace:      "platform-orchestrator-runner",
					ServiceAccount: "platform-orchestrator-runner",
				},
				Type: genclient.RunnerTypeKubernetesAgent,
				Key:  updatedPubKey,
			}, actualRemoteCfg)
			assert.Equal(t, "remote-runner", res.JSON200.Id)
		}
	})

	t.Run("cannot update with wrong pub key", func(t *testing.T) {
		// Generate a fake PEM public key string with correct prefix and length
		const fakeKeyLen = 100
		fakeKeyBody := ""
		for len(fakeKeyBody) < fakeKeyLen {
			fakeKeyBody += "A"
		}
		fakePEM := "-----BEGIN PUBLIC KEY-----\n" + fakeKeyBody + "\n-----END PUBLIC KEY-----\n"

		updateCfg := new(genclient.RunnerConfigurationUpdate)
		require.NoError(t, updateCfg.FromK8sAgentRunnerConfigurationUpdateBody(genclient.K8sAgentRunnerConfigurationUpdateBody{
			Key:  ref.Ref(fakePEM),
			Type: genclient.RunnerTypeKubernetesAgent,
		}))
		res, err := client.UpdateRunnerWithResponse(t.Context(), orgId, "remote-runner", genclient.UpdateRunnerJSONRequestBody{
			RunnerConfiguration: updateCfg,
		})
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusBadRequest, res.StatusCode(), string(res.Body))
			assert.Contains(t, res.JSON400.Message, "invalid public key")
		}
	})

	t.Run("can update a remote runner - update job and pub key", func(t *testing.T) {
		updatedPubKey := getRemoteRunnerPublicKey()
		updateCfg := new(genclient.RunnerConfigurationUpdate)
		require.NoError(t, updateCfg.FromK8sAgentRunnerConfigurationUpdateBody(genclient.K8sAgentRunnerConfigurationUpdateBody{
			Key: ref.Ref(updatedPubKey),
			Job: &genclient.K8sRunnerJobConfig{
				Namespace:      "default",
				ServiceAccount: "platform-orchestrator-runner",
			},
			Type: genclient.RunnerTypeKubernetesAgent,
		}))
		res, err := client.UpdateRunnerWithResponse(t.Context(), orgId, "remote-runner", genclient.UpdateRunnerJSONRequestBody{
			RunnerConfiguration: updateCfg,
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			actualRemoteCfg, err := res.JSON200.RunnerConfiguration.AsK8sAgentRunnerConfiguration()
			require.NoError(t, err)
			assert.Equal(t, genclient.K8sAgentRunnerConfiguration{
				Job: genclient.K8sRunnerJobConfig{
					Namespace:      "default",
					ServiceAccount: "platform-orchestrator-runner",
				},
				Type: genclient.RunnerTypeKubernetesAgent,
				Key:  updatedPubKey,
			}, actualRemoteCfg)
			assert.Equal(t, "remote-runner", res.JSON200.Id)
		}
	})

	t.Run("can create gke runner with gcs state storage", func(t *testing.T) {
		cfg := new(genclient.RunnerConfiguration)
		require.NoError(t, cfg.FromK8sGkeRunnerConfiguration(genclient.K8sGkeRunnerConfiguration{
			Cluster: genclient.K8sRunnerGkeCluster{
				Name:      "rinsewind",
				ProjectId: "my-gcp-project-id",
				Location:  "eu-west-2a",
				Auth: genclient.K8sRunnerGcpTemporaryAuth{
					GcpAudience:       "boo",
					GcpServiceAccount: "gcp@google.com",
				},
			},
			Job: genclient.K8sRunnerJobConfig{
				Namespace:      "platform-orchestrator-runner",
				ServiceAccount: "platform-orchestrator-runner",
			},
			Type: genclient.RunnerTypeKubernetesGke,
		}))
		ssc := new(genclient.StateStorageConfiguration)
		require.NoError(t, ssc.FromGCSStorageConfiguration(genclient.GCSStorageConfiguration{
			Type:       genclient.StateStorageTypeGcs,
			Bucket:     "my-gcs-bucket",
			PathPrefix: ref.Ref("my/prefix/path"),
		}))

		gcsRunnerId := "gcs-" + strings.ToLower(rand.Text())
		var gcsRunner *genclient.Runner
		{
			r, err := client.CreateRunnerWithResponse(t.Context(), orgId, genclient.RunnerCreateBody{
				Id:                        gcsRunnerId,
				Description:               ref.Ref("my gcs runner"),
				RunnerConfiguration:       *cfg,
				StateStorageConfiguration: *ssc,
			})
			require.NoError(t, err)
			require.Equal(t, http.StatusCreated, r.StatusCode(), string(r.Body))
			gcsRunner = r.JSON201
			assert.NotEmpty(t, gcsRunner.CreatedAt)
			assert.Equal(t, "my gcs runner", *gcsRunner.Description)
			assert.Equal(t, *ssc, gcsRunner.StateStorageConfiguration)

			actualGcsCfg, err := gcsRunner.StateStorageConfiguration.AsGCSStorageConfiguration()
			require.NoError(t, err)
			assert.Equal(t, "my-gcs-bucket", actualGcsCfg.Bucket)
			assert.Equal(t, ref.Ref("my/prefix/path"), actualGcsCfg.PathPrefix)
		}

		t.Run("can get", func(t *testing.T) {
			r, err := client.GetRunnerWithResponse(t.Context(), orgId, gcsRunner.Id)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, r.StatusCode(), string(r.Body))
			actualGcsCfg, err := r.JSON200.StateStorageConfiguration.AsGCSStorageConfiguration()
			require.NoError(t, err)
			assert.Equal(t, "my-gcs-bucket", actualGcsCfg.Bucket)
			assert.Equal(t, ref.Ref("my/prefix/path"), actualGcsCfg.PathPrefix)
		})

		t.Run("can update state storage to gcs with different bucket", func(t *testing.T) {
			updatedSsc := new(genclient.StateStorageConfiguration)
			require.NoError(t, updatedSsc.FromGCSStorageConfiguration(genclient.GCSStorageConfiguration{
				Type:   genclient.StateStorageTypeGcs,
				Bucket: "updated-gcs-bucket",
			}))
			r, err := client.UpdateRunnerWithResponse(t.Context(), orgId, gcsRunner.Id, genclient.UpdateRunnerJSONRequestBody{
				StateStorageConfiguration: updatedSsc,
			})
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, r.StatusCode(), string(r.Body))
			actualGcsCfg, err := r.JSON200.StateStorageConfiguration.AsGCSStorageConfiguration()
			require.NoError(t, err)
			assert.Equal(t, "updated-gcs-bucket", actualGcsCfg.Bucket)
			assert.Nil(t, actualGcsCfg.PathPrefix)
		})

		t.Run("can update state storage to gcs with different bucket and prefix", func(t *testing.T) {
			updatedSsc := new(genclient.StateStorageConfiguration)
			require.NoError(t, updatedSsc.FromGCSStorageConfiguration(genclient.GCSStorageConfiguration{
				Type:       genclient.StateStorageTypeGcs,
				Bucket:     "updated-gcs-bucket",
				PathPrefix: ref.Ref("my-prefix"),
			}))
			r, err := client.UpdateRunnerWithResponse(t.Context(), orgId, gcsRunner.Id, genclient.UpdateRunnerJSONRequestBody{
				StateStorageConfiguration: updatedSsc,
			})
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, r.StatusCode(), string(r.Body))
			actualGcsCfg, err := r.JSON200.StateStorageConfiguration.AsGCSStorageConfiguration()
			require.NoError(t, err)
			assert.Equal(t, "updated-gcs-bucket", actualGcsCfg.Bucket)
			assert.Equal(t, ref.Ref("my-prefix"), actualGcsCfg.PathPrefix)
		})

		t.Run("can delete", func(t *testing.T) {
			r, err := client.DeleteRunnerWithResponse(t.Context(), orgId, gcsRunner.Id)
			require.NoError(t, err)
			require.Equal(t, http.StatusNoContent, r.StatusCode(), string(r.Body))
		})
	})

	t.Run("can create k8s runner with azurerm state storage", func(t *testing.T) {
		cfg := new(genclient.RunnerConfiguration)
		require.NoError(t, cfg.FromK8sRunnerConfiguration(genclient.K8sRunnerConfiguration{
			Cluster: genclient.K8sRunnerK8sCluster{
				ClusterData: genclient.K8sRunnerK8sClusterClusterData{
					Server: "https://my-k8s-cluster.example.com",
				},
				Auth: genclient.K8sRunnerK8sClusterAuth{
					ServiceAccountToken: ref.Ref("my-token"),
				},
			},
			Job: genclient.K8sRunnerJobConfig{
				Namespace:      "platform-orchestrator-runner",
				ServiceAccount: "platform-orchestrator-runner",
			},
			Type: genclient.RunnerTypeKubernetes,
		}))

		baseAzureConfig := genclient.AzureRMStorageConfiguration{
			Type:               genclient.StateStorageTypeAzurerm,
			ResourceGroupName:  ref.Ref("rg-terraform-state"),
			StorageAccountName: "sttfstatepro001",
			ContainerName:      "tfstate",
			PathPrefix:         ref.Ref("path/to/state"),
		}

		ssc := new(genclient.StateStorageConfiguration)
		require.NoError(t, ssc.FromAzureRMStorageConfiguration(baseAzureConfig))

		t.Run("cannot create k8s runner with invalid azurerm state storage path prefix", func(t *testing.T) {
			invalidConfig := baseAzureConfig
			invalidConfig.PathPrefix = ref.Ref("/path/to/state")
			invalidSsc := new(genclient.StateStorageConfiguration)
			require.NoError(t, invalidSsc.FromAzureRMStorageConfiguration(invalidConfig))

			res, err := client.CreateRunnerWithResponse(t.Context(), orgId, genclient.RunnerCreateBody{
				Id:                        "azure-invalid-" + strings.ToLower(rand.Text()),
				Description:               ref.Ref("my azure runner invalid"),
				RunnerConfiguration:       *cfg,
				StateStorageConfiguration: *invalidSsc,
			})
			if assert.NoError(t, err) && assert.Equal(t, http.StatusBadRequest, res.StatusCode(), string(res.Body)) {
				assert.Contains(t, res.JSON400.Message, "state storage path_prefix is not a valid AzureRM path.")
			}
		})

		azureRunnerId := "azure-" + strings.ToLower(rand.Text())
		var azureRunner *genclient.Runner
		{
			r, err := client.CreateRunnerWithResponse(t.Context(), orgId, genclient.RunnerCreateBody{
				Id:                        azureRunnerId,
				Description:               ref.Ref("my azure runner"),
				RunnerConfiguration:       *cfg,
				StateStorageConfiguration: *ssc,
			})
			require.NoError(t, err)
			require.Equal(t, http.StatusCreated, r.StatusCode(), string(r.Body))
			azureRunner = r.JSON201
			assert.NotEmpty(t, azureRunner.CreatedAt)
			assert.Equal(t, "my azure runner", *azureRunner.Description)
			assert.Equal(t, *ssc, azureRunner.StateStorageConfiguration)

			actualAzureCfg, err := azureRunner.StateStorageConfiguration.AsAzureRMStorageConfiguration()
			require.NoError(t, err)
			assert.Equal(t, "rg-terraform-state", *actualAzureCfg.ResourceGroupName)
			assert.Equal(t, "sttfstatepro001", actualAzureCfg.StorageAccountName)
			assert.Equal(t, "tfstate", actualAzureCfg.ContainerName)
		}

		t.Run("can get", func(t *testing.T) {
			r, err := client.GetRunnerWithResponse(t.Context(), orgId, azureRunner.Id)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, r.StatusCode(), string(r.Body))
			actualAzureCfg, err := r.JSON200.StateStorageConfiguration.AsAzureRMStorageConfiguration()
			require.NoError(t, err)
			assert.Equal(t, "rg-terraform-state", *actualAzureCfg.ResourceGroupName)
			assert.Equal(t, "sttfstatepro001", actualAzureCfg.StorageAccountName)
			assert.Equal(t, "tfstate", actualAzureCfg.ContainerName)
		})

		t.Run("can update state storage to azurerm with different container", func(t *testing.T) {
			updatedConfig := baseAzureConfig
			updatedConfig.ContainerName = "tfstate-updated"
			updatedSsc := new(genclient.StateStorageConfiguration)
			require.NoError(t, updatedSsc.FromAzureRMStorageConfiguration(updatedConfig))
			r, err := client.UpdateRunnerWithResponse(t.Context(), orgId, azureRunner.Id, genclient.UpdateRunnerJSONRequestBody{
				StateStorageConfiguration: updatedSsc,
			})
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, r.StatusCode(), string(r.Body))
			actualAzureCfg, err := r.JSON200.StateStorageConfiguration.AsAzureRMStorageConfiguration()
			require.NoError(t, err)
			assert.Equal(t, "rg-terraform-state", *actualAzureCfg.ResourceGroupName)
			assert.Equal(t, "sttfstatepro001", actualAzureCfg.StorageAccountName)
			assert.Equal(t, "tfstate-updated", actualAzureCfg.ContainerName)
			assert.Equal(t, ref.Ref("path/to/state"), actualAzureCfg.PathPrefix)
		})

		t.Run("can update state storage to azurerm with different path prefix", func(t *testing.T) {
			updatedConfig := baseAzureConfig
			updatedConfig.ContainerName = "tfstate-updated"
			updatedConfig.PathPrefix = ref.Ref("state/directory")
			updatedSsc := new(genclient.StateStorageConfiguration)
			require.NoError(t, updatedSsc.FromAzureRMStorageConfiguration(updatedConfig))
			r, err := client.UpdateRunnerWithResponse(t.Context(), orgId, azureRunner.Id, genclient.UpdateRunnerJSONRequestBody{
				StateStorageConfiguration: updatedSsc,
			})
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, r.StatusCode(), string(r.Body))
			actualAzureCfg, err := r.JSON200.StateStorageConfiguration.AsAzureRMStorageConfiguration()
			require.NoError(t, err)
			assert.Equal(t, "rg-terraform-state", *actualAzureCfg.ResourceGroupName)
			assert.Equal(t, "sttfstatepro001", actualAzureCfg.StorageAccountName)
			assert.Equal(t, "tfstate-updated", actualAzureCfg.ContainerName)
			assert.Equal(t, "sttfstatepro001", actualAzureCfg.StorageAccountName)
			assert.Equal(t, ref.Ref("state/directory"), actualAzureCfg.PathPrefix)
		})

		t.Run("can update state storage to azurerm with different storage account", func(t *testing.T) {
			updatedConfig := baseAzureConfig
			updatedConfig.ResourceGroupName = ref.Ref("rg-terraform-state-new")
			updatedConfig.StorageAccountName = "sttfstatepro002"
			updatedConfig.ContainerName = "tfstate-new"
			updatedSsc := new(genclient.StateStorageConfiguration)
			require.NoError(t, updatedSsc.FromAzureRMStorageConfiguration(updatedConfig))
			r, err := client.UpdateRunnerWithResponse(t.Context(), orgId, azureRunner.Id, genclient.UpdateRunnerJSONRequestBody{
				StateStorageConfiguration: updatedSsc,
			})
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, r.StatusCode(), string(r.Body))
			actualAzureCfg, err := r.JSON200.StateStorageConfiguration.AsAzureRMStorageConfiguration()
			require.NoError(t, err)
			assert.Equal(t, "rg-terraform-state-new", *actualAzureCfg.ResourceGroupName)
			assert.Equal(t, "sttfstatepro002", actualAzureCfg.StorageAccountName)
			assert.Equal(t, "tfstate-new", actualAzureCfg.ContainerName)
			assert.Equal(t, ref.Ref("path/to/state"), actualAzureCfg.PathPrefix)
		})

		t.Run("can delete", func(t *testing.T) {
			r, err := client.DeleteRunnerWithResponse(t.Context(), orgId, azureRunner.Id)
			require.NoError(t, err)
			require.Equal(t, http.StatusNoContent, r.StatusCode(), string(r.Body))
		})
	})

	t.Run("can create ecs runner", func(t *testing.T) {
		rc := new(genclient.RunnerConfiguration)
		_ = rc.FromServerlessEcsRunnerConfiguration(genclient.ServerlessEcsRunnerConfiguration{
			Type: genclient.RunnerTypeServerlessEcs,
			Auth: genclient.AwsTemporaryAuth{
				RoleArn: "arn:aws:iam::123456789012:role/RunnerAuthRole",
			},
			Job: genclient.ServerlessEcsRunnerJob{
				Region:            "us-east-1",
				Cluster:           "platform-orchestrator-runner",
				Subnets:           []string{"subnet1", "subnet2"},
				IsPublicIpEnabled: true,
				ExecutionRoleArn:  "arn:aws:iam::123456789012:role/RunnerExecRole",
				TaskRoleArn:       ref.Ref("arn:aws:iam::123456789012:role/RunnerTaskRole"),
				Environment:       map[string]string{"A": "B"},
				Secrets: map[string]string{ //nolint:gosec // fake ARN values used only in test fixtures
					"MOUNTED_SECRET": "arn:aws:secretsmanager:us-east-1:123456789012:secret:prod/database/credentials-AbCdEf",
					"MOUNTED_PARAM":  "arn:aws:ssm:eu-central-1:667740703053:parameter/ben-testing",
				},
			},
		})
		ssc := new(genclient.StateStorageConfiguration)
		_ = ssc.FromS3StorageConfiguration(genclient.S3StorageConfiguration{
			Type:       "s3",
			Bucket:     "platform-orchestrator-runner",
			PathPrefix: ref.Ref("my/prefix/path"),
		})
		var ecsRunner *genclient.Runner
		{
			r, err := client.CreateRunnerWithResponse(t.Context(), orgId, genclient.RunnerCreateBody{
				Id:                        "ecs-" + strings.ToLower(rand.Text()),
				Description:               ref.Ref("my ecs runner"),
				RunnerConfiguration:       *rc,
				StateStorageConfiguration: *ssc,
			})
			require.NoError(t, err)
			require.Equal(t, http.StatusCreated, r.StatusCode(), string(r.Body))
			ecsRunner = r.JSON201
			assert.NotEmpty(t, ecsRunner.CreatedAt)
			assert.Equal(t, "my ecs runner", *ecsRunner.Description)
			assert.Equal(t, *rc, ecsRunner.RunnerConfiguration)
			assert.Equal(t, *ssc, ecsRunner.StateStorageConfiguration)
		}

		t.Run("can get", func(t *testing.T) {
			r, err := client.GetRunnerWithResponse(t.Context(), orgId, ecsRunner.Id)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, r.StatusCode(), string(r.Body))
			assert.Equal(t, *rc, ecsRunner.RunnerConfiguration)
			assert.Equal(t, *ssc, ecsRunner.StateStorageConfiguration)
		})

		t.Run("can update", func(t *testing.T) {
			rc := new(genclient.RunnerConfigurationUpdate)
			_ = rc.FromServerlessEcsRunnerConfigurationUpdateBody(genclient.ServerlessEcsRunnerConfigurationUpdateBody{
				Type: genclient.RunnerTypeServerlessEcs,
				Job: &genclient.ServerlessEcsRunnerJob{
					Region:            "us-east-1",
					Cluster:           "platform-orchestrator-runner",
					Subnets:           []string{"subnet1"},
					IsPublicIpEnabled: false,
					ExecutionRoleArn:  "arn:aws:iam::123456789012:role/RunnerExecRole",
				},
			})
			r, err := client.UpdateRunnerWithResponse(t.Context(), orgId, ecsRunner.Id, genclient.UpdateRunnerJSONRequestBody{
				RunnerConfiguration: rc,
			})
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, r.StatusCode(), string(r.Body))
			assert.Equal(t, *ssc, ecsRunner.StateStorageConfiguration)
		})

		t.Run("can delete", func(t *testing.T) {
			r, err := client.DeleteRunnerWithResponse(t.Context(), orgId, ecsRunner.Id)
			require.NoError(t, err)
			require.Equal(t, http.StatusNoContent, r.StatusCode(), string(r.Body))
		})
	})
}

func getKubernetesStateStorageConfiguration(t *testing.T) *genclient.StateStorageConfiguration {
	stateCfg := new(genclient.StateStorageConfiguration)
	k8sStateCfg := genclient.K8sStorageConfiguration{
		Namespace: "platform-orchestrator-state-namespace",
	}
	require.NoError(t, stateCfg.FromK8sStorageConfiguration(k8sStateCfg))
	return stateCfg
}

func getRemoteRunnerPublicKey() string {
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(fmt.Sprintf("failed to generate ed25519 key pair: %v", err))
	}
	derBytes, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		panic(fmt.Sprintf("failed to marshal public key: %v", err))
	}

	pem := pem.EncodeToMemory(&pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: derBytes,
	})

	return string(pem)
}

func generateRSAPEM(t require.TestingT) string {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err, "failed to generate RSA key pair")
	pub := &priv.PublicKey
	derBytes, err := x509.MarshalPKIXPublicKey(pub)
	require.NoError(t, err, "failed to marshal public key")
	pemBlock := &pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: derBytes,
	}
	return string(pem.EncodeToMemory(pemBlock))
}
