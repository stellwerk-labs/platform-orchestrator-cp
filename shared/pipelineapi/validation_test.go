package pipelineapi

import "testing"

func TestTrustedRecipeRejectsExecutableAndPhaseEscapes(t *testing.T) {
	for _, step := range []PipelineRecipeStep{
		{Id: "exec", ToolId: "exec", Phase: PrePlan, Parameters: map[string]any{}},
		{Id: "policy", ToolId: ResourcePolicy, Phase: PostApply, Parameters: map[string]any{}},
		{Id: "policy", ToolId: ResourcePolicy, Phase: PlanCheck, Parameters: map[string]any{"url": "https://untrusted.invalid"}},
	} {
		recipe := PipelineRecipe{Strategy: Reviewed, Steps: []PipelineRecipeStep{step}}
		if err := ValidateRecipe(recipe); err == nil {
			t.Fatal("unsafe recipe accepted", step)
		}
	}
}
func TestConditionalCostApprovalRequiresReviewedComparableEvidence(t *testing.T) {
	recipe := PipelineRecipe{Strategy: Reviewed, Steps: []PipelineRecipeStep{}, Approval: &struct {
		Mode PipelineRecipeApprovalMode `json:"mode"`
	}{CostNonIncreasing}}
	if ValidateRecipe(recipe) == nil {
		t.Fatal("conditional approval without estimator accepted")
	}
	recipe.Cost = &struct {
		Enabled bool `json:"enabled"`
	}{true}
	if err := ValidateRecipe(recipe); err != nil {
		t.Fatal(err)
	}
	recipe.Strategy = Apply
	if ValidateRecipe(recipe) == nil {
		t.Fatal("direct Apply bypassed review")
	}
}
