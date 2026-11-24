package model

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/genclient"
)

var nativeTargetRunnerID = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,127}$`)
var nativeTargetDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)

type NativeExecutionTargetReference struct {
	RunnerID                 string    `json:"runner_id"`
	RunnerUUID               uuid.UUID `json:"runner_uuid"`
	RuntimeVersion           string    `json:"runtime_version"`
	PipelineVersion          string    `json:"pipeline_version"`
	ToolsDigest              string    `json:"tools_digest"`
	StateBackendType         string    `json:"state_backend_type"`
	StateConfigurationDigest string    `json:"state_configuration_digest"`
}

// The caller holds the fresh Environment row and admission locks. Classify
// without locking legacy Runners, whose deletion can wait on that Environment FK.
func checkNativeExecutionTarget(ctx context.Context, db Databaser, tx Tx, env *Environment, target *NativeExecutionTargetReference) (*Runner, error) {
	if env.NativeFirstEligible != (target != nil) {
		return nil, NewErrConflict("Native execution requires an eligible Native-first Environment and exact Native target")
	}
	if !env.RunnerId.IsSet() {
		if target != nil {
			return nil, NewErrConflict("native target requires the exact assigned Runner")
		}
		return nil, nil
	}
	runner, err := db.GetRunner(ctx, tx, env.OrgId, env.RunnerId.Must(), GetModeDefault)
	if err != nil {
		return nil, err
	}
	if runner.RunnerType != string(genclient.RunnerTypeNativeAgent) {
		if target != nil {
			return nil, NewErrConflict("native target does not match the assigned Runner type")
		}
		return nil, nil
	}
	if target == nil {
		return nil, NewErrConflict("native Runner admission requires an exact native target")
	}
	runner, err = db.GetRunner(ctx, tx, env.OrgId, env.RunnerId.Must(), GetModeForUpdate)
	if err != nil {
		return nil, err
	}
	if runner.RunnerType != string(genclient.RunnerTypeNativeAgent) || runner.Uuid != target.RunnerUUID || runner.Id != target.RunnerID {
		return nil, NewErrConflict("native Runner authority changed before admission")
	}
	encoded, err := json.Marshal(runner.RunnerConfiguration)
	if err != nil {
		return nil, NewErrConflict("native Runner configuration is unavailable")
	}
	var config genclient.NativeAgentRunnerConfiguration
	if json.Unmarshal(encoded, &config) != nil || config.Type != genclient.RunnerTypeNativeAgent ||
		config.NativeExecution == nil || config.SupportedProtocols == nil {
		return nil, NewErrConflict("native Runner catalogue is disabled or unavailable")
	}
	catalogue := config.NativeExecution
	if string(catalogue.ContractVersion) != "stellwerk.runner.catalogue/v1" || string(catalogue.RuntimeVersion) != target.RuntimeVersion ||
		catalogue.ToolsDigest != target.ToolsDigest || !slices.Contains(*config.SupportedProtocols, target.PipelineVersion) ||
		!slices.Contains(*config.SupportedProtocols, "stellwerk.pipeline.plan-apply/v2") {
		return nil, NewErrConflict("native Runner profile changed before admission")
	}
	state, err := json.Marshal(runner.StateStorageConfiguration)
	if err != nil {
		return nil, NewErrConflict("native Runner state configuration is unavailable")
	}
	kind, digest, err := genclient.StateStorageConfigurationDigest(state)
	if err != nil || string(kind) != runner.StateStorageType || string(kind) != target.StateBackendType || digest != target.StateConfigurationDigest {
		return nil, NewErrConflict("native Runner state backend changed before admission")
	}
	return runner, nil
}
