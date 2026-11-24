package model

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/genclient"
)

// Native assignment follows the same admission-lock-before-row order as a
// writer. Legacy assignment keeps its existing row-lock boundary.
func GetEnvironmentForRunnerAssignment(ctx context.Context, db Databaser, tx Tx, org, project, id string) (*Environment, error) {
	if tx == nil {
		return nil, NewErrBadRequest("Runner assignment requires a transaction")
	}
	before, err := db.GetEnvironment(ctx, tx, org, project, id, GetModeDefault)
	if err != nil {
		return nil, err
	}
	if before.NativeFirstEligible {
		if err := lockExternalDispatchEnvironment(ctx, tx, before.Uuid); err != nil {
			return nil, err
		}
	}
	current, err := db.GetEnvironment(ctx, tx, org, project, id, GetModeForUpdate)
	if err != nil {
		return nil, err
	}
	if current.Uuid != before.Uuid {
		return nil, NewErrConflict("Environment lifecycle changed during Runner assignment")
	}
	return current, nil
}

func CheckNativeFirstRunnerAssignment(ctx context.Context, tx Tx, env *Environment, runner *Runner) error {
	if tx == nil || !env.NativeFirstEligible {
		return NewErrBadRequest("Native-first assignment requires a locked eligible Environment")
	}
	if runner.RunnerType != string(genclient.RunnerTypeNativeAgent) {
		return NewErrConflict("Native-first Environment requires a Native Runner")
	}
	var encoded []byte
	err := tx.QueryRowContext(ctx, `SELECT intent FROM deployment_admission_guards
  WHERE org_id=$1 AND environment_uuid=$2 AND outcome IS DISTINCT FROM 'absent'
  ORDER BY epoch LIMIT 1`, env.OrgId, env.Uuid).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var prior DeploymentAdmission
	if json.Unmarshal(encoded, &prior) != nil || validateDeploymentAdmission(prior) != nil || prior.NativeTarget == nil ||
		prior.OrgID != env.OrgId || prior.ProjectUUID != env.ProjectUuid || prior.EnvironmentUUID != env.Uuid {
		return NewErrConflict("Native-first Environment custody history cannot be verified")
	}
	state, err := json.Marshal(runner.StateStorageConfiguration)
	if err != nil {
		return err
	}
	kind, digest, err := genclient.StateStorageConfigurationDigest(state)
	if err != nil || string(kind) != runner.StateStorageType || prior.NativeTarget.RunnerUUID != runner.Uuid ||
		prior.NativeTarget.StateBackendType != string(kind) || prior.NativeTarget.StateConfigurationDigest != digest {
		return NewErrConflict("Native-first Runner custody changed; explicit handoff is required")
	}
	return nil
}
