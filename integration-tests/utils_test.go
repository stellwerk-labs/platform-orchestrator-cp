package integrationtests

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/hashicorp/vault/api"
	orchestratordp "github.com/stellwerk-labs/platform-orchestrator-dp/shared/v2/genclient"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"

	orchestratoriam "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"

	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/genclient"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
)

var testHttpClient = &http.Client{
	Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			if strings.HasSuffix(host, ".localhost") {
				address = net.JoinHostPort("127.0.0.1", port)
			}
			dialer := &net.Dialer{
				Timeout: 30 * time.Second,
			}
			return dialer.DialContext(ctx, network, address)
		},
	},
}

func MustServerClient(t *testing.T) genclient.ClientWithResponsesInterface {
	client, err := genclient.NewClientWithResponses(
		os.Getenv("SERVER_URL"),
		genclient.WithRequestEditorFn(func(ctx context.Context, req *http.Request) error {
			req.Header.Set("From", userid.InternalSystemUuid.String())
			return nil
		}), genclient.WithHTTPClient(testHttpClient),
	)
	require.NoError(t, err)
	return client
}

func MustInternalServerClient(t *testing.T) genclient.ClientWithResponsesInterface {
	client, err := genclient.NewClientWithResponses(
		os.Getenv("INTERNAL_SERVER_URL"),
		genclient.WithRequestEditorFn(func(ctx context.Context, req *http.Request) error {
			req.Header.Set("From", userid.InternalSystemUuid.String())
			return nil
		}),
	)
	require.NoError(t, err)
	return client
}

func MustServerClientWithId(t *testing.T, userId string) genclient.ClientWithResponsesInterface {
	client, err := genclient.NewClientWithResponses(
		os.Getenv("SERVER_URL"),
		genclient.WithRequestEditorFn(func(ctx context.Context, req *http.Request) error {
			req.Header.Set("From", userId)
			return nil
		}), genclient.WithHTTPClient(testHttpClient),
	)
	require.NoError(t, err)
	return client
}

func MustGenerateTestUserToken(t *testing.T) string {
	t.Helper()
	ageRecipient, err := age.ParseX25519Recipient(os.Getenv("TEST_USER_IDENTITY_RECIPIENT"))
	require.NoError(t, err)
	buff := new(bytes.Buffer)
	bw := base64.NewEncoder(base64.StdEncoding, buff)
	aw, _ := age.Encrypt(bw, ageRecipient)
	_ = json.NewEncoder(aw).Encode(map[string]string{
		"ProviderId":  rand.Text(),
		"DisplayName": "bob.smith",
	})
	_ = aw.Close()
	_ = bw.Close()
	return buff.String()
}

func MustDpClient(t *testing.T) orchestratordp.ClientWithResponsesInterface {
	client, err := orchestratordp.NewClientWithResponses(os.Getenv("SERVER_URL"),
		orchestratordp.WithRequestEditorFn(func(ctx context.Context, req *http.Request) error {
			req.Header.Set("From", userid.InternalSystemUuid.String())
			if strings.HasPrefix(req.URL.Path, "/internal") {
				return fmt.Errorf("path %s is internal", req.URL.Path)
			}
			return nil
		}), orchestratordp.WithHTTPClient(testHttpClient))
	require.NoError(t, err)
	return client
}

func MustIamClient(t *testing.T) orchestratoriam.ClientWithResponsesInterface {
	client, err := orchestratoriam.NewClientWithResponses(os.Getenv("SERVER_URL"),
		orchestratoriam.WithRequestEditorFn(func(ctx context.Context, req *http.Request) error {
			req.Header.Set("From", userid.InternalSystemUuid.String())
			if strings.HasPrefix(req.URL.Path, "/internal") {
				return fmt.Errorf("path %s is internal", req.URL.Path)
			}
			return nil
		}), orchestratoriam.WithHTTPClient(testHttpClient))
	require.NoError(t, err)
	return client
}

func MustInternalIamClient(t *testing.T) orchestratoriam.ClientWithResponsesInterface {
	client, err := orchestratoriam.NewClientWithResponses(
		os.Getenv("INTERNAL_IAM_URL"),
		orchestratoriam.WithRequestEditorFn(func(ctx context.Context, req *http.Request) error {
			req.Header.Set("From", userid.InternalSystemUuid.String())
			return nil
		}),
	)
	require.NoError(t, err)
	return client
}

func MustCreateOrg(t *testing.T, cpClient genclient.ClientWithResponsesInterface) *genclient.InternalOrganization {
	t.Helper()
	res, err := cpClient.CreateInternalOrganizationWithResponse(t.Context(), genclient.CreateInternalOrganizationJSONRequestBody{IdPrefix: ref.Ref("org")})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, res.StatusCode(), "unexpected: %s", string(res.Body))
	return res.JSON201
}

func MustRegisterUser(t *testing.T, iamClient orchestratoriam.ClientWithResponsesInterface, tut string) uuid.UUID {
	t.Helper()
	r, err := iamClient.RegisterUserWithResponse(t.Context(), &orchestratoriam.RegisterUserParams{}, orchestratoriam.RegisterUserJSONRequestBody{
		Provider:      "testuser",
		ProviderToken: tut,
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusAccepted, r.StatusCode(), string(r.Body))
	return r.JSON202.Id
}

func MustCreateMembershipInOrg(t *testing.T, internalIamClient orchestratoriam.ClientWithResponsesInterface, orgId, roleDisplayName, scope string, userId uuid.UUID) {
	t.Helper()

	roles, err := internalIamClient.ListRolesWithResponse(t.Context(), orgId, nil)
	require.NoError(t, err)
	var roleId uuid.UUID
	require.Equal(t, http.StatusOK, roles.StatusCode(), "unexpected: %s", string(roles.Body))
	for _, r := range roles.JSON200.Items {
		if r.DisplayName == roleDisplayName {
			roleId = r.Id
			break
		}
	}
	require.NotEmpty(t, roleId)

	res, err := internalIamClient.InternalCreateOrgMembershipWithResponse(t.Context(), orgId, orchestratoriam.InternalCreateOrgMembershipJSONRequestBody{
		UserId:      userId,
		Subject:     roleId.String(),
		SubjectType: orchestratoriam.SubjectTypeRole,
		Scope:       ref.Ref(scope),
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, res.StatusCode(), "unexpected: %s", string(res.Body))
}

func MustCreateProject(t *testing.T, cpClient genclient.ClientWithResponsesInterface, orgId string, projectId string) *genclient.Project {
	t.Helper()
	res, err := cpClient.CreateProjectWithResponse(t.Context(), orgId, genclient.CreateProjectJSONRequestBody{Id: projectId})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, res.StatusCode(), "unexpected: %s", string(res.Body))
	return res.JSON201
}

func MustCreateEnvType(t *testing.T, cpClient genclient.ClientWithResponsesInterface, orgId string, et string) *genclient.EnvironmentType {
	t.Helper()
	res, err := cpClient.CreateEnvironmentTypeWithResponse(t.Context(), orgId, genclient.CreateEnvironmentTypeJSONRequestBody{Id: et})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, res.StatusCode(), "unexpected: %s", string(res.Body))
	return res.JSON201
}

func MustCreateEnv(t *testing.T, cpClient genclient.ClientWithResponsesInterface, orgId, et, projectId, env string) *genclient.Environment {
	t.Helper()
	res, err := cpClient.CreateEnvironmentWithResponse(t.Context(), orgId, projectId, genclient.CreateEnvironmentJSONRequestBody{EnvTypeId: et, Id: env})
	require.NoError(t, err)
	require.Equalf(t, http.StatusCreated, res.StatusCode(), "unexpected: %s", string(res.Body))
	return res.JSON201
}

func MustCreateVaultClient(t *testing.T, vaultUrl, vaulToken string) *api.Client {
	cfg := api.DefaultConfig()
	cfg.Address = vaultUrl
	client, err := api.NewClient(cfg)
	require.NoError(t, err)
	client.SetToken(vaulToken)
	return client
}

func MustCreateRunnerWithRule(t *testing.T, cpClient genclient.ClientWithResponsesInterface, orgId string, envTypeId string, projectId string, runnerId string) *genclient.Runner {
	t.Helper()
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

	stateCfg := new(genclient.StateStorageConfiguration)
	k8sStateCfg := genclient.K8sStorageConfiguration{
		Namespace: "platform-orchestrator-state-namespace",
		Type:      genclient.StateStorageTypeKubernetes,
	}
	require.NoError(t, stateCfg.FromK8sStorageConfiguration(k8sStateCfg))
	res, err := cpClient.CreateRunnerWithResponse(t.Context(), orgId, genclient.CreateRunnerJSONRequestBody{
		Id:                        runnerId,
		RunnerConfiguration:       *cfg,
		StateStorageConfiguration: *stateCfg,
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))

	ruleRes, err := cpClient.CreateRunnerRuleInOrgWithResponse(t.Context(), orgId, genclient.CreateRunnerRuleInOrgJSONRequestBody{
		RunnerId:  runnerId,
		ProjectId: ref.Ref(projectId),
		EnvTypeId: ref.Ref(envTypeId),
	})

	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, ruleRes.StatusCode(), string(ruleRes.Body))
	return res.JSON201
}

func MustCreateResourceType(t *testing.T, cpClient genclient.ClientWithResponsesInterface, orgId string, rt string) *genclient.ResourceType {
	t.Helper()
	res, err := cpClient.CreateResourceTypeWithResponse(t.Context(), orgId, genclient.CreateResourceTypeJSONRequestBody{Id: rt, OutputSchema: map[string]interface{}{}})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, res.StatusCode(), "unexpected: %s", string(res.Body))
	return res.JSON201
}

func MustCreateEmptyModule(t *testing.T, cpClient genclient.ClientWithResponsesInterface, orgId, id, rt string) *genclient.Module {
	t.Helper()
	res, err := cpClient.CreateModuleWithResponse(t.Context(), orgId, genclient.CreateModuleJSONRequestBody{
		Id:           id,
		ResourceType: rt,
		ModuleSource: "git::https://github.com/stellwerk-labs/example-tf-module",
		ModuleInputs: map[string]interface{}{
			"thing": "${context.env_id}",
		},
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, res.StatusCode(), "unexpected: %s", string(res.Body))
	return res.JSON201
}
