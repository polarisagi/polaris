package cognition

import (
	"testing"
)

// ─── BudgetManager ───────────────────────────────────────────────────────────

func TestBudgetManager_SelectBudget_SimpleTask(t *testing.T) {
	bm := NewBudgetManager()
	mode := bm.SelectBudget("classification", 0.5, true, 0)
	if mode != BudgetFixed {
		t.Errorf("expected BudgetFixed for classification task, got %d", mode)
	}
}

func TestBudgetManager_SelectBudget_Adaptive(t *testing.T) {
	bm := NewBudgetManager()
	mode := bm.SelectBudget("complex_research", 0.7, true, 0)
	if mode != BudgetAdaptive {
		t.Errorf("expected BudgetAdaptive for complex interactive task, got %d", mode)
	}
}

func TestBudgetManager_SelectBudget_ThrottleDowngrade(t *testing.T) {
	bm := NewBudgetManager()
	mode := bm.SelectBudget("complex", 0.7, true, 1) // burnStage=1 → throttle
	if mode != BudgetFixed {
		t.Errorf("expected BudgetFixed when throttled, got %d", mode)
	}
}

func TestContextWindowManager_NeedsCompaction(t *testing.T) {
	cwm := &ContextWindowManager{
		maxTokens:   1000,
		softTrigger: 0.70,
		hardTrigger: 0.90,
	}
	cwm.currentUsage = 500
	if cwm.NeedsCompaction() != 0 {
		t.Error("expected no compaction at 50%")
	}
	cwm.currentUsage = 750
	if cwm.NeedsCompaction() != 1 {
		t.Error("expected soft compaction at 75%")
	}
	cwm.currentUsage = 950
	if cwm.NeedsCompaction() != 2 {
		t.Error("expected hard compaction at 95%")
	}
}

// ─── StepScorer ──────────────────────────────────────────────────────────────

func TestStepScorer_SuccessAllPassed(t *testing.T) {
	scorer := &StepScorer{
		toolSuccessWeight: 0.4,
		schemaCheckWeight: 0.3,
		latencyWeight:     0.2,
		tokenEfficiencyWt: 0.1,
	}
	ctx := StepContext{ToolResult: true, SchemaPassed: true, LatencyMs: 0, TokensUsed: 0}
	score := scorer.Score(ctx)
	if score != 1.0 {
		t.Errorf("expected perfect score 1.0, got %f", score)
	}
}

func TestStepScorer_FailureLowersScore(t *testing.T) {
	scorer := &StepScorer{
		toolSuccessWeight: 0.4,
		schemaCheckWeight: 0.3,
		latencyWeight:     0.2,
		tokenEfficiencyWt: 0.1,
	}
	ctxOK := StepContext{ToolResult: true, SchemaPassed: true}
	ctxFail := StepContext{ToolResult: false, SchemaPassed: true}
	if scorer.Score(ctxFail) >= scorer.Score(ctxOK) {
		t.Error("failure score should be lower than success score")
	}
}

func TestStepScorer_SchemaFailLowersScore(t *testing.T) {
	scorer := &StepScorer{
		toolSuccessWeight: 0.4,
		schemaCheckWeight: 0.3,
		latencyWeight:     0.2,
		tokenEfficiencyWt: 0.1,
	}
	ctxPassed := StepContext{ToolResult: true, SchemaPassed: true}
	ctxFailed := StepContext{ToolResult: true, SchemaPassed: false}
	if scorer.Score(ctxFailed) >= scorer.Score(ctxPassed) {
		t.Error("schema failure score should be lower")
	}
}
