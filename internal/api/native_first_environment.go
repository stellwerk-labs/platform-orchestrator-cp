package api

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/genclient"
)

func (s *Server) nativeFirstEnvironmentRunner(ctx context.Context, tx model.Tx, org, project, kind string) (*model.Runner, error) {
	rules, err := s.getMatchingRunners(ctx, tx, org, project, kind)
	if err != nil {
		return nil, err
	}
	if len(rules) == 0 {
		return nil, nil
	}
	candidates := map[string]*model.Runner{}
	native := ""
	for _, rule := range rules {
		if rule.Specificity != rules[0].Specificity {
			break
		}
		if _, exists := candidates[rule.Rule.RunnerId]; exists {
			continue
		}
		runner, err := s.Database.GetRunner(ctx, tx, org, rule.Rule.RunnerId, model.GetModeDefault)
		if err != nil {
			return nil, err
		}
		candidates[runner.Id] = runner
		if runner.RunnerType == string(RunnerTypeNativeAgent) {
			native = runner.Id
		}
	}
	if native == "" {
		return nil, nil
	}
	if len(candidates) != 1 {
		return nil, model.NewErrConflict("Native-first Runner rules must select one unambiguous registration")
	}
	runner, err := s.Database.GetRunner(ctx, tx, org, native, model.GetModeForUpdate)
	if err != nil {
		return nil, err
	}
	if err := validateNativeFirstRunner(runner); err != nil {
		return nil, err
	}
	return runner, nil
}

func validateNativeFirstRunner(runner *model.Runner) error {
	if runner.RunnerType != string(RunnerTypeNativeAgent) || runner.Uuid == uuid.Nil || runner.RunnerConfiguration["native_execution"] == nil {
		return model.NewErrConflict("Native-first Runner catalogue is disabled or unavailable")
	}
	if err := validateRunnerNativeExecution(runner.RunnerConfiguration); err != nil {
		return model.NewErrConflict("Native-first Runner configuration is unavailable: " + err.Error())
	}
	raw, err := json.Marshal(runner.StateStorageConfiguration)
	if err != nil {
		return model.NewErrConflict("Native-first Runner state configuration is unavailable")
	}
	kind, _, err := genclient.StateStorageConfigurationDigest(raw)
	if err != nil || string(kind) != runner.StateStorageType {
		return model.NewErrConflict("Native-first Runner state configuration is unavailable")
	}
	return nil
}
