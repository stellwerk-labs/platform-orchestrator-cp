package model

import (
	"context"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/genclient"
	p "github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/pipelineapi"
)

// Historical representation is pure stored-evidence comparison. A released
// grant does not revoke the past; current owned-dispatch checks run separately.
func checkNativeExternalHistory(ctx context.Context, tx Tx, env *Environment, selected *NativeExecutionTargetReference) error {
	var unrepresented bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (
 SELECT 1 FROM external_operation_dispatches x
 WHERE x.org_id=$1 AND x.intent->>'environment_uuid'=$2 AND x.state='settled'
 AND (
  x.completion->>'ProjectUUID'=x.intent->>'project_uuid'
  AND x.completion->>'EnvironmentUUID'=x.intent->>'environment_uuid'
  AND x.completion->>'RequestDigest'=x.request_digest
  AND x.intent->>'org_id'=x.org_id
  AND x.intent->>'plugin_id'=x.plugin_id
  AND x.intent->>'operation_id'=x.operation_id::text
  AND x.intent->>'dispatch_id'=x.dispatch_id
  AND x.intent->>'fence'=x.fence::text
  AND x.intent->>'request_digest'=x.request_digest
  AND x.intent->>'actor_id'=x.actor_id::text
  AND x.intent->>'action'=x.action
  AND x.intent->>'project_uuid'=$3
  AND (
   (x.completion->'NoEffect'='true'::jsonb
    AND x.completion->>'DeploymentID'=$4 AND x.completion->>'Status'='')
   OR
   (x.completion->'NoEffect'='false'::jsonb
    AND jsonb_typeof(x.intent->'target_versions')='object' AND x.intent->'target_versions'<>'{}'::jsonb
    AND x.completion->>'DeploymentID' ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
    AND x.completion->>'DeploymentID'<>$4
    AND x.completion->>'Status' IN ('succeeded','failed','cancelled','terminated')
    AND EXISTS (
     SELECT 1 FROM deployment_admission_guards g
     WHERE g.org_id=x.org_id AND g.environment_uuid=$5 AND g.project_uuid=$6
      AND g.outcome IS DISTINCT FROM 'absent' AND g.outcome=x.completion->>'Status' AND g.settled_at IS NOT NULL
      AND g.intent->>'org_id'=x.org_id AND g.intent->>'environment_uuid'=x.intent->>'environment_uuid'
      AND g.intent->>'project_uuid'=x.intent->>'project_uuid' AND g.intent->>'actor_id'=x.intent->>'actor_id'
      AND jsonb_typeof(g.intent->'native_target')='object'
      AND g.intent->'native_target' @> jsonb_build_object('runner_uuid',$9::text,'state_backend_type',$10::text,'state_configuration_digest',$11::text,'runtime_version',$12::text,'pipeline_version',$13::text)
      AND jsonb_typeof(g.intent->'native_target'->'runner_id')='string' AND g.intent->'native_target'->>'runner_id' ~ $14
      AND jsonb_typeof(g.intent->'native_target'->'tools_digest')='string' AND g.intent->'native_target'->>'tools_digest' ~ $15
      AND g.intent->>'admission_id'=g.admission_id::text AND jsonb_typeof(g.intent->'request_digest')='string' AND g.intent->>'request_digest' ~ $15
      AND g.intent->'external_dispatch'->>'plugin_id'=x.plugin_id
      AND g.intent->'external_dispatch'->>'operation_id'=x.operation_id::text
      AND g.intent->'external_dispatch'->>'dispatch_id'=x.dispatch_id
      AND g.intent->'external_dispatch'->>'fence'=x.fence::text
      AND g.intent->'external_dispatch'->>'request_digest'=x.request_digest
      AND ((x.action='external-deployment.admit' AND g.intent->>'mode'='deploy')
        OR (x.action='external-deployment.rollback' AND g.intent->>'mode'='rollback'))
      AND g.intent->'module_versions' @> (x.intent->'target_versions')
      AND (
       ((COALESCE(x.intent->'pipeline','null'::jsonb)='null'::jsonb
         OR (x.intent->'pipeline'->>'builtin'=$7 AND x.intent->'pipeline'->>'digest'=$8
          AND COALESCE(x.intent->'pipeline'->'version_id','null'::jsonb)='null'::jsonb))
        AND COALESCE(g.intent->'pipeline','null'::jsonb)='null'::jsonb)
       OR
       (COALESCE(x.intent->'pipeline'->'builtin','null'::jsonb)='null'::jsonb
        AND g.intent->'pipeline'->>'version_id'=x.intent->'pipeline'->>'version_id'
        AND g.intent->'pipeline'->>'digest'=x.intent->'pipeline'->>'digest')
      )
    ))
  )
 ) IS NOT TRUE
)`, env.OrgId, env.Uuid.String(), env.ProjectUuid.String(), uuid.Nil.String(), env.Uuid, env.ProjectUuid, string(p.Default), p.BuiltinReference().Digest,
		selected.RunnerUUID.String(), selected.StateBackendType, selected.StateConfigurationDigest,
		string(genclient.NativeTargetRuntimeV2), string(genclient.NativeTargetPipelineV2), nativeTargetRunnerID.String(), nativeTargetDigest.String()).Scan(&unrepresented)
	if err != nil {
		return err
	}
	if unrepresented {
		return errNativeAdmissionHistory
	}
	return nil
}
