package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"maps"
	"reflect"
	"slices"

	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"

	corev1 "k8s.io/api/core/v1"
)

func (s *Server) tryMakeRunnerConfigurationUpdate(
	ctx context.Context,
	orgId,
	runnerId,
	existingRunnerType string,
	patchRequest *RunnerConfigurationUpdate,
) (RunnerConfigurationAny, *model.RunnerConfigurationSecret, error) {
	if patchRequest == nil {
		return nil, nil, nil
	}

	// Validate runner configuration patch
	runnerType, patchRunnerCfg, err := checkUpdateBodyRunnerConfigurationStrict(*patchRequest)
	if err != nil {
		return nil, nil, err
	}

	// Verify runner type matches existing
	if runnerType != existingRunnerType {
		return nil, nil, errors.Errorf("runner type %s does not match existing runner type %s", runnerType, existingRunnerType)
	}

	// Purge secrets from configuration
	// in the future, if we'll have secrets not only in kubernetes cluster, we might need to rethink this as we are only storing patch secrets here
	patchRunnerCfg, patchRunnerCfgSecretPath, err := s.purgeConfigurationFromSecretsAndStoreSecrets(ctx, orgId, runnerId, runnerType, patchRunnerCfg)
	if err != nil {
		return nil, nil, err
	}

	// Validate pod template if present
	if jobCfg := getJobConfiguration(patchRunnerCfg); jobCfg.PodTemplate != nil {
		if err := validatePodTemplate(*jobCfg.PodTemplate); err != nil {
			return nil, nil, err
		}
	}

	return patchRunnerCfg, patchRunnerCfgSecretPath, nil
}

func updateRunnerConfiguration(existingConfig map[string]interface{}, patchConfig RunnerConfigurationAny) map[string]interface{} {
	result := make(map[string]interface{})
	maps.Copy(result, existingConfig)

	if patchConfig != nil {
		patchConfigJson, _ := json.Marshal(patchConfig)
		var patchHelper map[string]interface{}
		_ = json.Unmarshal(patchConfigJson, &patchHelper)
		maps.Copy(result, patchHelper)
	}

	return result
}

// RunnerConfigurationUnion represents any runner configuration type
type RunnerConfigurationUnion interface {
	*K8sRunnerConfiguration | *K8sGkeRunnerConfiguration | *K8sEksRunnerConfiguration | *K8sAgentRunnerConfiguration |
		*K8sRunnerConfigurationUpdateBody | *K8sGkeRunnerConfigurationUpdateBody | *K8sEksRunnerConfigurationUpdateBody | *K8sAgentRunnerConfigurationUpdateBody
}

// RunnerConfigurationAny represents any runner configuration type as concrete interface
type RunnerConfigurationAny interface {
	// GetJobConfiguration returns a k8s job configuration if this is a k8s runner. If not it can be an empty struct.
	GetJobConfiguration() K8sRunnerJobConfig
	// GetSecretConfiguration returns any secrets that need to be stored in vault for the runner
	GetSecretConfiguration() map[string]interface{}
}

func getJobConfiguration(runnerCfg RunnerConfigurationAny) K8sRunnerJobConfig {
	return runnerCfg.GetJobConfiguration()
}

func getSecretConfiguration(runnerCfg RunnerConfigurationAny) map[string]interface{} {
	return runnerCfg.GetSecretConfiguration()
}

func (r *K8sRunnerConfiguration) GetSecretConfiguration() map[string]interface{} {
	var outK8sCfg = K8sRunnerConfiguration{}
	outK8sCfg.Cluster = K8sRunnerK8sCluster{
		Auth: r.Cluster.Auth,
	}

	jsonK8sCfg, _ := json.Marshal(outK8sCfg)
	var out map[string]interface{}
	_ = json.Unmarshal(jsonK8sCfg, &out)

	return out
}

func (r *K8sRunnerConfigurationUpdateBody) GetSecretConfiguration() map[string]interface{} {
	if r.Cluster == nil {
		return nil
	}
	var outK8sCfg = K8sRunnerConfiguration{}
	outK8sCfg.Cluster = K8sRunnerK8sCluster{
		Auth: r.Cluster.Auth,
	}

	jsonK8sCfg, _ := json.Marshal(outK8sCfg)
	var out map[string]interface{}
	_ = json.Unmarshal(jsonK8sCfg, &out)

	return out
}

func (r *K8sGkeRunnerConfiguration) GetSecretConfiguration() map[string]interface{} {
	return nil
}

func (r *K8sGkeRunnerConfigurationUpdateBody) GetSecretConfiguration() map[string]interface{} {
	return nil
}

func (r *K8sEksRunnerConfiguration) GetSecretConfiguration() map[string]interface{} {
	return nil
}

func (r *K8sEksRunnerConfigurationUpdateBody) GetSecretConfiguration() map[string]interface{} {
	return nil
}

func (r *K8sAgentRunnerConfiguration) GetSecretConfiguration() map[string]interface{} {
	return nil
}

func (r *K8sAgentRunnerConfigurationUpdateBody) GetSecretConfiguration() map[string]interface{} {
	return nil
}

func (r *ServerlessEcsRunnerConfiguration) GetSecretConfiguration() map[string]interface{} {
	return nil
}

func (r *ServerlessEcsRunnerConfigurationUpdateBody) GetSecretConfiguration() map[string]interface{} {
	return nil
}

// GetJobConfiguration implementations
func (r *K8sRunnerConfiguration) GetJobConfiguration() K8sRunnerJobConfig {
	return r.Job
}

func (r *K8sGkeRunnerConfiguration) GetJobConfiguration() K8sRunnerJobConfig {
	return r.Job
}

func (r *K8sEksRunnerConfiguration) GetJobConfiguration() K8sRunnerJobConfig {
	return r.Job
}

func (r *K8sAgentRunnerConfiguration) GetJobConfiguration() K8sRunnerJobConfig {
	return r.Job
}

func (r *K8sRunnerConfigurationUpdateBody) GetJobConfiguration() K8sRunnerJobConfig {
	if r.Job != nil {
		return *r.Job
	}
	return K8sRunnerJobConfig{}
}

func (r *K8sGkeRunnerConfigurationUpdateBody) GetJobConfiguration() K8sRunnerJobConfig {
	if r.Job != nil {
		return *r.Job
	}
	return K8sRunnerJobConfig{}
}

func (r *K8sEksRunnerConfigurationUpdateBody) GetJobConfiguration() K8sRunnerJobConfig {
	if r.Job != nil {
		return *r.Job
	}
	return K8sRunnerJobConfig{}
}

func (r *K8sAgentRunnerConfigurationUpdateBody) GetJobConfiguration() K8sRunnerJobConfig {
	if r.Job != nil {
		return *r.Job
	}
	return K8sRunnerJobConfig{}
}

func (r *ServerlessEcsRunnerConfiguration) GetJobConfiguration() K8sRunnerJobConfig {
	return K8sRunnerJobConfig{}
}
func (r *ServerlessEcsRunnerConfigurationUpdateBody) GetJobConfiguration() K8sRunnerJobConfig {
	return K8sRunnerJobConfig{}
}

func getStateStorage(stateCfg StateStorageConfiguration) (string, map[string]interface{}) {
	stateStorageType, _ := stateCfg.Discriminator()
	stateStorageConfigJson, _ := stateCfg.MarshalJSON()
	var stateStorageConfiguration map[string]interface{}
	_ = json.Unmarshal(stateStorageConfigJson, &stateStorageConfiguration)
	return stateStorageType, stateStorageConfiguration
}

// asTStrictConfiguration converts raw json data from runner configuration to a specific configuration type
func asTStrictConfiguration[T any](r json.RawMessage) (*T, error) {
	dec := json.NewDecoder(bytes.NewReader(r))
	dec.DisallowUnknownFields()

	var cfg T
	if err := dec.Decode(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

type discriminatorConfig interface {
	Discriminator() (string, error)
}

var (
	createTypeMap = map[string]func(json.RawMessage) (RunnerConfigurationAny, error){
		string(RunnerTypeKubernetesGke): func(union json.RawMessage) (RunnerConfigurationAny, error) {
			return asTStrictConfiguration[K8sGkeRunnerConfiguration](union)
		},
		string(RunnerTypeKubernetesEks): func(union json.RawMessage) (RunnerConfigurationAny, error) {
			return asTStrictConfiguration[K8sEksRunnerConfiguration](union)
		},
		string(RunnerTypeKubernetes): func(union json.RawMessage) (RunnerConfigurationAny, error) {
			return asTStrictConfiguration[K8sRunnerConfiguration](union)
		},
		string(RunnerTypeKubernetesAgent): func(union json.RawMessage) (RunnerConfigurationAny, error) {
			if cfg, err := asTStrictConfiguration[K8sAgentRunnerConfiguration](union); err != nil {
				return nil, err
			} else {
				if err := validateEd25519PublicKey(cfg.Key); err != nil {
					return nil, errors.Wrap(err, "invalid public key")
				} else {
					return cfg, nil
				}
			}
		},
		string(RunnerTypeServerlessEcs): func(union json.RawMessage) (RunnerConfigurationAny, error) {
			ecs, err := asTStrictConfiguration[ServerlessEcsRunnerConfiguration](union)
			if err == nil {
				if err := validateEcsRunnerEnvironment(ecs.Job.Environment); err != nil {
					return nil, err
				}
			}
			return ecs, err
		},
	}

	updateTypeMap = map[string]func(json.RawMessage) (RunnerConfigurationAny, error){
		string(RunnerTypeKubernetesGke): func(union json.RawMessage) (RunnerConfigurationAny, error) {
			return asTStrictConfiguration[K8sGkeRunnerConfigurationUpdateBody](union)
		},
		string(RunnerTypeKubernetesEks): func(union json.RawMessage) (RunnerConfigurationAny, error) {
			return asTStrictConfiguration[K8sEksRunnerConfigurationUpdateBody](union)
		},
		string(RunnerTypeKubernetes): func(union json.RawMessage) (RunnerConfigurationAny, error) {
			return asTStrictConfiguration[K8sRunnerConfigurationUpdateBody](union)
		},
		string(RunnerTypeKubernetesAgent): func(union json.RawMessage) (RunnerConfigurationAny, error) {
			if cfg, err := asTStrictConfiguration[K8sAgentRunnerConfigurationUpdateBody](union); err != nil {
				return nil, err
			} else {
				if cfg.Key != nil {
					if err := validateEd25519PublicKey(*cfg.Key); err != nil {
						return nil, errors.Wrap(err, "invalid public key")
					}
				}
				return cfg, nil
			}
		},
		string(RunnerTypeServerlessEcs): func(union json.RawMessage) (RunnerConfigurationAny, error) {
			ecs, err := asTStrictConfiguration[ServerlessEcsRunnerConfigurationUpdateBody](union)
			if err == nil {
				if ecs.Job != nil && ecs.Job.Environment != nil {
					if err := validateEcsRunnerEnvironment(ecs.Job.Environment); err != nil {
						return nil, err
					}
				}
			}
			return ecs, err
		},
	}
)

func validateEcsRunnerEnvironment(e map[string]string) error {
	for k := range e {
		if slices.Contains(forbiddenPORunnerEnvVariables, k) {
			return errors.Errorf("environment variable %s is reserved and cannot be set by the runner", k)
		} else if slices.Contains(forbiddenSensitiveRunnerEnvVariables, k) {
			return errors.Errorf("environment variable %s is sensitive and must be set using a secret", k)
		}
	}
	return nil
}

func validateEd25519PublicKey(publicKeyPEM string) error {
	block, _ := pem.Decode([]byte(publicKeyPEM))
	if block == nil || block.Type != "PUBLIC KEY" {
		return errors.New("failed to decode PEM block containing public key")
	}

	publicKeyInterface, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return errors.Wrap(err, "failed to parse public key from PEM")
	}

	if _, ok := publicKeyInterface.(ed25519.PublicKey); !ok {
		return errors.New("public key is not an ed25519 key")
	}
	return nil
}

func checkRunnerConfigurationStrict[T discriminatorConfig](runnerConfiguration T, typeMap map[string]func(json.RawMessage) (RunnerConfigurationAny, error)) (runnerType string, parsedConfiguration RunnerConfigurationAny, err error) {
	runnerType, _ = runnerConfiguration.Discriminator()

	configParser, exists := typeMap[runnerType]
	if !exists {
		return runnerType, nil, errors.Errorf("runner type `%s` not supported", runnerType)
	}

	var union json.RawMessage
	switch v := interface{}(runnerConfiguration).(type) {
	case RunnerConfiguration:
		union = v.union
	case RunnerConfigurationUpdate:
		union = v.union
	}

	if parsedConfiguration, err = configParser(union); err != nil {
		return runnerType, nil, errors.Wrapf(err, "the supplied runner configuration does not match the specified runner type %s", runnerType)
	}

	return runnerType, parsedConfiguration, nil
}

func checkCreateBodyRunnerConfigurationStrict(runnerConfiguration RunnerConfiguration) (runnerType string, parsedConfiguration RunnerConfigurationAny, err error) {
	return checkRunnerConfigurationStrict(runnerConfiguration, createTypeMap)
}

func checkUpdateBodyRunnerConfigurationStrict(runnerConfiguration RunnerConfigurationUpdate) (runnerType string, parsedConfiguration RunnerConfigurationAny, err error) {
	return checkRunnerConfigurationStrict(runnerConfiguration, updateTypeMap)
}

func (s *Server) purgeConfigurationFromSecretsAndStoreSecrets(ctx context.Context, orgId, runnerId, runnerType string, runnerCfg RunnerConfigurationAny) (RunnerConfigurationAny, *model.RunnerConfigurationSecret, error) {
	switch runnerType {
	case string(RunnerTypeServerlessEcs):
		return runnerCfg, nil, nil
	case string(RunnerTypeKubernetesGke):
		return runnerCfg, nil, nil
	case string(RunnerTypeKubernetesEks):
		return runnerCfg, nil, nil
	case string(RunnerTypeKubernetes):
		var secretPath string
		var secretVersion int
		var err error

		secretCfg := getSecretConfiguration(runnerCfg)
		if secretCfg == nil {
			return runnerCfg, nil, nil
		}
		secretPath = credsRunnerSecretPath(orgId, runnerId)
		if secretVersion, err = s.Vault.UpsertSecret(ctx, secretPath, secretCfg); err != nil {
			return nil, nil, errors.Wrap(err, "failed to store cluster credentials")
		} else {
			if reflect.TypeOf(runnerCfg) == reflect.TypeOf(&K8sRunnerConfiguration{}) {
				k8sRunnerCfg, _ := runnerCfg.(*K8sRunnerConfiguration)
				k8sRunnerCfg.Cluster.Auth = K8sRunnerK8sClusterAuth{
					ClientCertificateData: ref.ReplaceStringOrNil(k8sRunnerCfg.Cluster.Auth.ClientCertificateData, SecretPlaceholder),
					ServiceAccountToken:   ref.ReplaceStringOrNil(k8sRunnerCfg.Cluster.Auth.ServiceAccountToken, SecretPlaceholder),
					ClientKeyData:         ref.ReplaceStringOrNil(k8sRunnerCfg.Cluster.Auth.ClientKeyData, SecretPlaceholder),
				}
				return k8sRunnerCfg, &model.RunnerConfigurationSecret{Path: secretPath, Version: secretVersion}, nil
			} else {
				k8sRunnerCfg, _ := runnerCfg.(*K8sRunnerConfigurationUpdateBody)
				k8sRunnerCfg.Cluster.Auth = K8sRunnerK8sClusterAuth{
					ClientCertificateData: ref.ReplaceStringOrNil(k8sRunnerCfg.Cluster.Auth.ClientCertificateData, SecretPlaceholder),
					ServiceAccountToken:   ref.ReplaceStringOrNil(k8sRunnerCfg.Cluster.Auth.ServiceAccountToken, SecretPlaceholder),
					ClientKeyData:         ref.ReplaceStringOrNil(k8sRunnerCfg.Cluster.Auth.ClientKeyData, SecretPlaceholder),
				}
				return k8sRunnerCfg, &model.RunnerConfigurationSecret{Path: secretPath, Version: secretVersion}, nil
			}
		}
	case string(RunnerTypeKubernetesAgent):
		return runnerCfg, nil, nil
	default:
		return nil, nil, errors.Errorf("runner type `%s` not supported", runnerType)
	}
}

func validatePodTemplate(podTemplate map[string]interface{}) error {
	var podTemplatePatch corev1.PodTemplateSpec
	if err := runtime.DefaultUnstructuredConverter.FromUnstructuredWithValidation(podTemplate, &podTemplatePatch, true); err != nil {
		return errors.Wrap(err, "failed to convert supplied pod template into a kubernetes pod template")
	} else {
		if podTemplatePatch.Namespace != "" {
			return errors.New("it is not allowed to specify a namespace in the pod_template field of the runner configuration")
		}
		if podTemplatePatch.Spec.ServiceAccountName != "" {
			return errors.New("it is not allowed to specify a serviceAccountName in the pod_template field of the runner configuration")
		}
		for _, container := range podTemplatePatch.Spec.Containers {
			if container.Name == "" {
				return errors.New("pod_template.spec.containers[*].name is required")
			}
			if container.Name == RunnerContainerName || container.Name == RunnerLegacyContainerName {
				for _, envVar := range container.Env {
					if slices.Contains(forbiddenPORunnerEnvVariables, envVar.Name) {
						return errors.Errorf("environment variable `%s` is a reserved one and can't be overwritten by pod_template field", envVar.Name)
					}
					if slices.Contains(forbiddenSensitiveRunnerEnvVariables, envVar.Name) && envVar.ValueFrom == nil {
						return errors.Errorf("environment variable %q is sensitive and must be set using the value-from property", envVar.Name)
					}
				}
			}
		}
		return nil
	}
}
