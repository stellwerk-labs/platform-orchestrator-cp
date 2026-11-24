package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
)

var runnerToolID = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
var runnerToolName = regexp.MustCompile(`^[^\p{Cc}]{1,120}$`)
var runnerToolsDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)

type runnerExecutionConfiguration struct {
	Type               RunnerType      `json:"type"`
	Key                string          `json:"key"`
	MaxParallelJobs    *int            `json:"max_parallel_jobs"`
	NativeExecution    json.RawMessage `json:"native_execution"`
	Capabilities       []string        `json:"capabilities"`
	SupportedProtocols []string        `json:"supported_protocols"`
}

// Validate the final configuration: a PATCH may omit capabilities or protocols
// while preserving an existing catalogue that still depends on them.
func validateRunnerNativeExecution(configuration any) error {
	encoded, err := json.Marshal(configuration)
	if err != nil {
		return errors.New("invalid Runner execution configuration")
	}
	var config runnerExecutionConfiguration
	if json.Unmarshal(encoded, &config) != nil {
		return errors.New("invalid Runner execution configuration")
	}
	if config.Type == RunnerTypeNativeAgent {
		if err := validateNativeAgentRegistration(config); err != nil {
			return err
		}
	}
	if len(config.NativeExecution) == 0 {
		return nil
	}
	if len(config.NativeExecution) > 64<<10 {
		return errors.New("Runner execution catalogue exceeds 64 KiB")
	}
	var catalogue RunnerExecutionCatalogue
	decoder := json.NewDecoder(bytes.NewReader(config.NativeExecution))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&catalogue) != nil || !catalogue.ContractVersion.Valid() || !catalogue.RuntimeVersion.Valid() || !runnerToolsDigest.MatchString(catalogue.ToolsDigest) || catalogue.Tools == nil || len(catalogue.Tools) > 32 {
		return errors.New("invalid Runner execution catalogue")
	}
	if err := validateRunnerCatalogueProfile(config, catalogue); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, tool := range catalogue.Tools {
		if seen[tool.Id] {
			return errors.New("Runner execution catalogue tool IDs must be unique")
		}
		if err := validateRunnerExecutionTool(tool, config.Capabilities); err != nil {
			return err
		}
		seen[tool.Id] = true
	}
	return nil
}

func validateNativeAgentRegistration(config runnerExecutionConfiguration) error {
	if config.MaxParallelJobs == nil || *config.MaxParallelJobs < 1 {
		return errors.New("native Runner requires explicit positive max_parallel_jobs")
	}
	if err := validateEd25519PublicKey(config.Key); err != nil {
		return errors.New("native Runner requires a valid Ed25519 public key")
	}
	if slices.Contains(config.SupportedProtocols, "stellwerk.pipeline/v1") || slices.Contains(config.SupportedProtocols, "stellwerk.pipeline.plan-apply/v1") {
		return errors.New("native Runner cannot advertise legacy pipeline protocols")
	}
	return nil
}

func validateRunnerCatalogueProfile(config runnerExecutionConfiguration, catalogue RunnerExecutionCatalogue) error {
	protocol := "stellwerk.pipeline/v1"
	if catalogue.RuntimeVersion == StellwerkRunnerRolloutv2 {
		protocol = "stellwerk.pipeline/v2"
	}
	if !slices.Contains(config.SupportedProtocols, protocol) {
		return errors.New("Runner execution catalogue requires the native pipeline protocol")
	}
	if config.Type == RunnerTypeNativeAgent && (catalogue.RuntimeVersion != StellwerkRunnerRolloutv2 || !slices.Contains(config.SupportedProtocols, "stellwerk.pipeline.plan-apply/v2")) {
		return errors.New("native Runner requires the v2 runtime and saved-plan protocol")
	}
	return nil
}

func validateRunnerExecutionTool(tool RunnerExecutionTool, capabilities []string) error {
	if !runnerToolID.MatchString(tool.Id) || !runnerToolName.MatchString(tool.Name) || strings.TrimSpace(tool.Name) == "" {
		return errors.New("invalid Runner execution tool name or ID")
	}
	if !tool.Phase.Valid() || !tool.Retry.Valid() || (tool.Phase != PostApply && tool.Retry != RunnerExecutionToolRetryReadOnly) {
		return errors.New("invalid Runner execution tool phase or retry policy")
	}
	if tool.RequiredCapabilities == nil || validateRunnerCapabilities(tool.RequiredCapabilities) != nil {
		return errors.New("invalid Runner execution tool capability requirements")
	}
	for _, capability := range tool.RequiredCapabilities {
		if !slices.Contains(capabilities, capability) {
			return errors.New("Runner execution tool requires an undeclared capability")
		}
	}
	return nil
}
