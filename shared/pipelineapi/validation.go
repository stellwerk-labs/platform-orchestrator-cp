package pipelineapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const localExecCapability = "local.exec"

var recipeIdentifier = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

// TrustedTools is an operator-owned catalogue. Recipes select handlers, never executables.
func TrustedTools() []PipelineTool {
	return []PipelineTool{
		{Id: "validate-manifest", Name: "Validate manifest", Phase: "pre_plan", Description: "Validate the captured manifest before planning.", RequiredCapabilities: []string{localExecCapability}},
		{Id: "resource-policy", Name: "Resource policy", Phase: "plan_check", Description: "Require a successful exact-plan resource-policy check.", RequiredCapabilities: []string{localExecCapability, "artifacts.read"}},
		{Id: "bootstrap-database", Name: "Bootstrap database", Phase: "post_apply", Description: "Run the operator-approved local database bootstrap task with its durable receipt.", RequiredCapabilities: []string{localExecCapability, "database.local"}},
		{Id: "bootstrap-lost-ack", Name: "Database bootstrap (recovery demo)", Phase: "post_apply", Description: "Demo task deliberately loses its acknowledgement; retry resumes only this task.", RequiredCapabilities: []string{localExecCapability, "database.local"}},
	}
}

func ValidateDraft(slug, name string, recipe PipelineRecipe) error {
	if !recipeIdentifier.MatchString(slug) {
		return fmt.Errorf("pipeline slug must contain 1-64 lowercase letters, digits or hyphens")
	}
	if strings.TrimSpace(name) == "" || utf8.RuneCountInString(name) > 120 {
		return fmt.Errorf("pipeline name must contain 1-120 characters")
	}
	return ValidateRecipe(recipe)
}

func ValidateRecipe(recipe PipelineRecipe) error {
	if !recipe.Strategy.Valid() || recipe.Steps == nil || len(recipe.Steps) > 16 {
		return fmt.Errorf("pipeline requires a known strategy and at most 16 steps")
	}
	if recipe.Approval != nil && (!recipe.Approval.Mode.Valid() || recipe.Strategy != Reviewed) {
		return fmt.Errorf("approval requires a reviewed pipeline and known mode")
	}
	if recipe.Cost != nil && recipe.Cost.Enabled && recipe.Strategy != Reviewed {
		return fmt.Errorf("cost estimation requires a reviewed pipeline")
	}
	if recipe.Approval != nil && recipe.Approval.Mode == CostNonIncreasing && (recipe.Cost == nil || !recipe.Cost.Enabled) {
		return fmt.Errorf("conditional approval requires enabled cost estimation")
	}
	tools := map[string]string{}
	for _, tool := range TrustedTools() {
		tools[tool.Id] = tool.Phase
	}
	seen := map[string]bool{}
	previous := 0
	phases := map[PipelineRecipeStepPhase]int{PrePlan: 1, PlanCheck: 2, PostApply: 3}
	for _, step := range recipe.Steps {
		phase, found := tools[string(step.ToolId)]
		if !found || phase != string(step.Phase) || !recipeIdentifier.MatchString(step.Id) || seen[step.Id] || step.Parameters == nil || len(step.Parameters) != 0 {
			return fmt.Errorf("steps require unique bounded IDs, approved tools in their declared phases and empty typed parameters")
		}
		if recipe.Strategy == Apply && step.Phase == PlanCheck {
			return fmt.Errorf("plan checks require reviewed strategy; direct Apply supports preparation and post-Apply tasks")
		}
		if phases[step.Phase] < previous {
			return fmt.Errorf("steps must follow preparation, checks, post-Apply order")
		}
		previous = phases[step.Phase]
		seen[step.Id] = true
	}
	return nil
}

// RecipeDigest is stable across JSON object key order and includes the frozen display name.
func RecipeDigest(name string, recipe PipelineRecipe) (string, error) {
	if err := ValidateRecipe(recipe); err != nil {
		return "", err
	}
	raw, err := json.Marshal(struct {
		Name       string         `json:"name"`
		Definition PipelineRecipe `json:"definition"`
	}{name, recipe})
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:]), nil
}
