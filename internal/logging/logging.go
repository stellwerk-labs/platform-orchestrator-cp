package logging

import (
	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/hlogger"
	"go.uber.org/zap"
)

func ZapOrgId(o string) zap.Field {
	return zap.String(hlogger.POOrgId, o)
}

func ZapOrgUuid(o uuid.UUID) zap.Field {
	return zap.Any("org_uuid", o.String())
}

func ZapEnvTypeId(o string) zap.Field {
	return zap.Any("env_type_id", o)
}

func ZapProjectId(o string) zap.Field {
	return zap.Any(hlogger.POProjectId, o)
}

func ZapProjectUuid(o uuid.UUID) zap.Field {
	return zap.Any("project_uuid", o.String())
}

func ZapEnvId(o string) zap.Field {
	return zap.Any(hlogger.POEnvId, o)
}

func ZapEnvUuid(o uuid.UUID) zap.Field {
	return zap.Any("env_uuid", o.String())
}

func ZapModuleDefinitionId(i string) zap.Field {
	return zap.String("definition_id", i)
}

func ZapModuleDefinitionVersionId(i string) zap.Field {
	return zap.String("version_id", i)
}

func ZapModuleRuleId(i string) zap.Field {
	return zap.String("rule_id", i)
}

func ZapRunnerId(i string) zap.Field {
	return zap.String(hlogger.PORunnerId, i)
}

func ZapRunnerRuleId(i string) zap.Field {
	return zap.String("runner_rule_id", i)
}
