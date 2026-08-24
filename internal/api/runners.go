package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/stellwerk-labs/golib/hlogger"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/authz"
	"go.uber.org/zap"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/logging"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/vault"
)

const (
	SecretPlaceholder             = "SECRET"
	RunnerContainerName           = "main"
	RunnerLegacyContainerName     = "platform-orchestrator-runner"
	getEnvsByRunnerPaginationSize = 10
)

var (
	forbiddenPORunnerEnvVariables        = []string{"ORG_ID", "DEPLOYMENT_ID", "MODE", "TOKEN", "ENCRYPTING_KEY", "PLATFORM_ORCHESTRATOR_BASE_URL"}
	forbiddenSensitiveRunnerEnvVariables = []string{"RUNNER_GIT_SSH_KEYS"}
)

// stateStorageCompatibilityMatrix maps each runner type to a supported state storage type.
var stateStorageCompatibilityMatrix = map[RunnerType][]StateStorageType{
	RunnerTypeKubernetes:      {StateStorageTypeKubernetes, StateStorageTypeS3, StateStorageTypeGcs, StateStorageTypeAzurerm},
	RunnerTypeKubernetesAgent: {StateStorageTypeKubernetes, StateStorageTypeS3, StateStorageTypeGcs, StateStorageTypeAzurerm},
	RunnerTypeKubernetesEks:   {StateStorageTypeKubernetes, StateStorageTypeS3, StateStorageTypeGcs, StateStorageTypeAzurerm},
	RunnerTypeKubernetesGke:   {StateStorageTypeKubernetes, StateStorageTypeS3, StateStorageTypeGcs, StateStorageTypeAzurerm},
	RunnerTypeServerlessEcs:   {StateStorageTypeS3},
}

func credsRunnerSecretPath(orgId, runnerId string) string {
	return fmt.Sprintf("/platform-orchestrator/orgs/%s/runners/%s", orgId, runnerId)
}

func apiRunnerFromDbRunner(rn model.Runner) Runner {
	rnB, _ := json.Marshal(rn.RunnerConfiguration)
	var rnCfg RunnerConfiguration
	_ = json.Unmarshal(rnB, &rnCfg)

	stateJson, _ := json.Marshal(rn.StateStorageConfiguration)
	return Runner{
		CreatedAt:           rn.CreatedAt,
		Description:         rn.Description.Ref(),
		Id:                  rn.Id,
		RunnerConfiguration: rnCfg,
		OrgId:               rn.OrgId,
		StateStorageConfiguration: StateStorageConfiguration{
			union: stateJson,
		},
		UpdatedAt: rn.UpdatedAt,
	}
}

func apiRunnerSummaryFromDbRunner(rn model.Runner) RunnerSummary {
	return RunnerSummary{
		CreatedAt:                 rn.CreatedAt,
		Description:               rn.Description.Ref(),
		Id:                        rn.Id,
		RunnerConfiguration:       &RunnerConfigurationSummary{Type: RunnerType(rn.RunnerType)},
		OrgId:                     rn.OrgId,
		StateStorageConfiguration: &StateStorageConfigurationSummary{Type: StateStorageType(rn.StateStorageType)},
		UpdatedAt:                 rn.UpdatedAt,
	}
}

func internalApiRunnerFromDbRunner(rn model.Runner) InternalRunner {
	rnB, _ := json.Marshal(rn.RunnerConfiguration)
	var rnCfg RunnerConfiguration
	_ = json.Unmarshal(rnB, &rnCfg)

	stateJson, _ := json.Marshal(rn.StateStorageConfiguration)
	return InternalRunner{
		CreatedAt:           rn.CreatedAt,
		Description:         rn.Description.Ref(),
		Id:                  rn.Id,
		RunnerConfiguration: rnCfg,
		OrgId:               rn.OrgId,
		StateStorageConfiguration: StateStorageConfiguration{
			union: stateJson,
		},
		RunnerConfigurationSecret: ConfigurationSecret{
			Path:    ref.DerefOr(rn.RunnerConfigurationSecret, model.RunnerConfigurationSecret{}).Path,
			Version: ref.DerefOr(rn.RunnerConfigurationSecret, model.RunnerConfigurationSecret{}).Version,
		},
		UpdatedAt: rn.UpdatedAt,
	}
}

// List runners in the org
// (GET /orgs/{orgId}/runners)
func (s *Server) ListRunners(ctx context.Context, request ListRunnersRequestObject) (ListRunnersResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgAuthorization(ctx, uid, request.OrgId, authz.PermissionRunnerRead); err != nil {
		return nil, err
	}
	page, next, err := s.Database.ListRunners(ctx, nil, request.OrgId, ref.DerefOr(request.Params.Page, ""), ref.DerefOr(request.Params.PerPage, 100))
	if err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return ListRunners404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
		}
		return nil, errors.Wrap(err, "failed to list modules")
	}
	out := make([]RunnerSummary, len(page))
	for i, item := range page {
		out[i] = apiRunnerSummaryFromDbRunner(item)
	}
	return ListRunners200JSONResponse{
		Items:         out,
		NextPageToken: ref.RefStringEmptyNil(next),
	}, nil
}

// Create a runner in the org
// (POST /orgs/{orgId}/runners)
func (s *Server) CreateRunner(ctx context.Context, request CreateRunnerRequestObject) (CreateRunnerResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgAuthorization(ctx, uid, request.OrgId, authz.PermissionRunnerWrite); err != nil {
		return nil, err
	}
	logger := hlogger.TraceScopedLoggerFromCtx(s.Logger, ctx).With(logging.ZapRunnerId(request.Body.Id))
	var runnerConfigSecretPath *model.RunnerConfigurationSecret
	runnerType, runnerCfg, err := checkCreateBodyRunnerConfigurationStrict(request.Body.RunnerConfiguration)
	if err != nil {
		return CreateRunner400JSONResponse{Generate400Response(err.Error())}, nil
	}
	runnerCfg, runnerConfigSecretPath, err = s.purgeConfigurationFromSecretsAndStoreSecrets(ctx, request.OrgId, request.Body.Id, runnerType, runnerCfg)
	if err != nil {
		return CreateRunner400JSONResponse{Generate400Response(err.Error())}, nil
	}
	stateStorageType, stateStorageCfg := getStateStorage(request.Body.StateStorageConfiguration)

	if compatibleStateStorage := stateStorageCompatibilityMatrix[RunnerType(runnerType)]; !slices.Contains(compatibleStateStorage, StateStorageType(stateStorageType)) {
		return CreateRunner400JSONResponse{Generate400Response(
			fmt.Sprintf("state storage type '%s' is not compatible with runner type '%s', it must be one of %v", stateStorageType, runnerType, compatibleStateStorage)),
		}, nil
	}

	if tx, err := s.Database.BeginTx(ctx, nil); err != nil {
		return nil, errors.Wrap(err, "failed to begin transaction")
	} else {
		defer func() {
			if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
				logger.Error("failed to rollback transaction", zap.Error(err))
			}
		}()

		if jobCfg := getJobConfiguration(runnerCfg); jobCfg.PodTemplate != nil {
			if err := validatePodTemplate(*jobCfg.PodTemplate); err != nil {
				return CreateRunner400JSONResponse{Generate400Response(err.Error())}, nil
			}
		}

		runnerConfigJson, _ := json.Marshal(runnerCfg)
		var runnerConfiguration map[string]interface{}
		_ = json.Unmarshal(runnerConfigJson, &runnerConfiguration)

		now := time.Now().UTC()
		r, err := s.Database.CreateRunner(ctx, tx, &model.Runner{
			OrgId:                     request.OrgId,
			Id:                        request.Body.Id,
			Description:               opt.OfRef(request.Body.Description),
			CreatedAt:                 now,
			UpdatedAt:                 now,
			RunnerType:                runnerType,
			RunnerConfiguration:       runnerConfiguration,
			RunnerConfigurationSecret: runnerConfigSecretPath,
			StateStorageType:          stateStorageType,
			StateStorageConfiguration: stateStorageCfg,
		})
		if err != nil {
			if me, ok := model.IsErrConflict(err); ok {
				return CreateRunner409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(me)}, nil
			}
			return nil, errors.Wrap(err, "failed to create runner")
		} else if err := tx.Commit(); err != nil {
			return nil, errors.Wrap(err, "failed to commit transaction")
		}

		logger.Info("created new runner", logging.ZapRunnerId(r.Id))
		return CreateRunner201JSONResponse(apiRunnerFromDbRunner(*r)), nil
	}
}

// Get runner in the org
// (GET /orgs/{orgId}/runners/{runnerId})
func (s *Server) GetRunner(ctx context.Context, request GetRunnerRequestObject) (GetRunnerResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgAuthorization(ctx, uid, request.OrgId, authz.PermissionRunnerRead); err != nil {
		return nil, err
	}
	runner, err := s.Database.GetRunner(ctx, nil, request.OrgId, request.RunnerId)
	if err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return GetRunner404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
		}
		return nil, errors.Wrap(err, "failed to get runner")
	}
	return GetRunner200JSONResponse(apiRunnerFromDbRunner(*runner)), nil
}

// Get runner in the org with secret configuration as secret path
// (GET /internal/orgs/{orgId}/runners/{runnerId})
func (s *Server) GetInternalRunner(ctx context.Context, request GetInternalRunnerRequestObject) (GetInternalRunnerResponseObject, error) {
	runner, err := s.Database.GetRunner(ctx, nil, request.OrgId, request.RunnerId)
	if err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return GetInternalRunner404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
		}
		return nil, errors.Wrap(err, "failed to get runner")
	}
	return GetInternalRunner200JSONResponse(internalApiRunnerFromDbRunner(*runner)), nil
}

// Delete runner in the org
// (DELETE /orgs/{orgId}/runners/{runnerId})
func (s *Server) DeleteRunner(ctx context.Context, request DeleteRunnerRequestObject) (DeleteRunnerResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgAuthorization(ctx, uid, request.OrgId, authz.PermissionRunnerWrite); err != nil {
		return nil, err
	}
	ids, ctx := hlogger.EnsurePlatformOrchestratorIdsOnCtx(ctx)
	logger := hlogger.TraceScopedLoggerFromCtx(s.Logger, ctx).With(logging.ZapRunnerId(request.RunnerId)).WithLazy(ids.AsLogField())
	if tx, err := s.Database.BeginTx(ctx, nil); err != nil {
		return nil, errors.Wrap(err, "failed to begin transaction")
	} else {
		defer func() {
			if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
				logger.Error("failed to rollback transaction", zap.Error(err))
			}
		}()
		var deleteSecretFromVault bool
		if r, err := s.Database.GetRunner(ctx, tx, request.OrgId, request.RunnerId); err != nil {
			if me, ok := model.IsErrNotFound(err); ok {
				return DeleteRunner404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
			}
			return nil, errors.Wrap(err, "failed to retrieve runner")
		} else {
			if r.RunnerType == string(RunnerTypeKubernetes) {
				deleteSecretFromVault = true
			}
			var envs []string
			if page, _, err := s.Database.ListEnvironmentsByRunnerId(ctx, tx, request.OrgId, request.RunnerId, "", getEnvsByRunnerPaginationSize); err != nil {
				return nil, errors.Wrapf(err, "failed to list envs by runner %s ", request.RunnerId)
			} else {
				for _, env := range page {
					envs = append(envs, fmt.Sprintf("%s/%s", env.ProjectId, env.Id))
				}
			}

			if len(envs) > 0 {
				return DeleteRunner409JSONResponse{N409ConflictJSONResponse: Generate409Response(fmt.Sprintf("runner %s is in use by the environments: %s", request.RunnerId, strings.Join(envs, ", ")))}, nil
			}

			if err := s.Database.DeleteRunner(ctx, tx, request.OrgId, request.RunnerId); err != nil {
				if me, ok := model.IsErrNotFound(err); ok {
					return DeleteRunner404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
				} else if me, ok := model.IsErrConflict(err); ok {
					return DeleteRunner409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(me)}, nil
				}
				return nil, err
			} else if err := tx.Commit(); err != nil {
				return nil, errors.Wrap(err, "failed to commit transaction")
			} else {
				if deleteSecretFromVault {
					if err := s.Vault.DeleteSecret(ctx, credsRunnerSecretPath(request.OrgId, request.RunnerId)); err != nil && !errors.Is(err, vault.ErrSecretNotFound) {
						logger.Warn("failed to delete cluster credentials", zap.Error(err))
					}
				}
				logger.Info("runner deleted")
				return DeleteRunner204Response{}, nil
			}
		}
	}
}

// Update runner in the org
// (PATCH /orgs/{orgId}/runners/{runnerId})
func (s *Server) UpdateRunner(ctx context.Context, request UpdateRunnerRequestObject) (UpdateRunnerResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgAuthorization(ctx, uid, request.OrgId, authz.PermissionRunnerWrite); err != nil {
		return nil, err
	}
	ids, ctx := hlogger.EnsurePlatformOrchestratorIdsOnCtx(ctx)
	logger := hlogger.TraceScopedLoggerFromCtx(s.Logger, ctx).With(logging.ZapRunnerId(request.RunnerId)).WithLazy(ids.AsLogField())
	tx, err := s.Database.BeginTx(ctx, nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to begin transaction")
	}

	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			logger.Error("failed to rollback transaction", zap.Error(err))
		}
	}()

	// Get original runner
	runner, err := s.Database.GetRunner(ctx, tx, request.OrgId, request.RunnerId)
	if err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return UpdateRunner404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
		}
		return nil, errors.Wrap(err, "failed to get runner")
	}

	// Validate and prepare runner configuration patch
	patchRunnerCfg, patchRunnerCfgSecretPath, err := s.tryMakeRunnerConfigurationUpdate(
		ctx,
		request.OrgId,
		request.RunnerId,
		runner.RunnerType,
		request.Body.RunnerConfiguration,
	)
	if err != nil {
		return UpdateRunner400JSONResponse{Generate400Response(err.Error())}, nil
	}

	// Construct updated runner configuration
	updatedRunnerCfg := updateRunnerConfiguration(runner.RunnerConfiguration, patchRunnerCfg)

	// Construct updated runner
	patch := &model.RunnerPatch{
		UpdatedAt:                 time.Now().UTC(),
		Description:               opt.OfRef(request.Body.Description),
		RunnerConfiguration:       ref.Ref(updatedRunnerCfg),
		RunnerConfigurationSecret: patchRunnerCfgSecretPath,
	}
	if request.Body.StateStorageConfiguration != nil {
		sst, ssc := getStateStorage(*request.Body.StateStorageConfiguration)
		patch.StateStorageType = opt.Of(sst)
		patch.StateStorageConfiguration = ref.Ref(ssc)

		if compatibleStateStorage := stateStorageCompatibilityMatrix[RunnerType(runner.RunnerType)]; !slices.Contains(compatibleStateStorage, StateStorageType(sst)) {
			return UpdateRunner400JSONResponse{Generate400Response(
				fmt.Sprintf("state storage type '%s' is not compatible with runner type '%s', it must be one of %v", sst, runner.RunnerType, compatibleStateStorage)),
			}, nil
		}
	}

	// Update Runner
	if ret, err := s.Database.UpdateRunner(ctx, tx, request.OrgId, request.RunnerId, patch); err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return UpdateRunner404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
		}
		return nil, err
	} else {
		if err := tx.Commit(); err != nil {
			return nil, errors.Wrap(err, "failed to commit transaction")
		}
		logger.Info("runner updated")
		return UpdateRunner200JSONResponse(apiRunnerFromDbRunner(*ret)), nil
	}
}
