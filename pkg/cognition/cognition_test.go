package cognition

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
)

// ─── ContextAssembler ────────────────────────────────────────────────────────

func TestBuildContext_ZoneCount(t *testing.T) {
	layout := BuildContext(nil, 8000)
	if layout == nil {
		t.Fatal("expected non-nil layout")
	}
	if len(layout.zones) != 5 {
		t.Errorf("expected 5 zones, got %d", len(layout.zones))
	}
}

func TestBuildContext_TokenBudget(t *testing.T) {
	layout := BuildContext(nil, 1000)
	total := 0
	for _, z := range layout.zones {
		total += z.MaxTokens
	}
	// 5 zones: 10+15+35+25+15 = 100% → should sum to 100% of 1000
	if total > 1000 {
		t.Errorf("zone tokens exceed budget: %d > 1000", total)
	}
}

func TestBuildContext_FirstZoneIsImmutable(t *testing.T) {
	layout := BuildContext(nil, 4096)
	if len(layout.zones) == 0 {
		t.Fatal("no zones")
	}
	if layout.zones[0].Zone != ZoneImmutable {
		t.Errorf("expected first zone to be Immutable, got %d", layout.zones[0].Zone)
	}
}

func TestBuildContext_LastZoneIsOutput(t *testing.T) {
	layout := BuildContext(nil, 4096)
	last := layout.zones[len(layout.zones)-1]
	if !last.Output {
		t.Error("expected last zone to be output buffer")
	}
}

func TestAssembleContext_Order(t *testing.T) {
	ctx := context.Background()
	result, _ := AssembleContext(ctx, "IMMUTABLE", "SKILL", "DATA", protocol.TaintNone, nil, "")
	expected := "IMMUTABLESKILLDATA"
	if result != expected {
		t.Errorf("expected %q, got %q", expected, result)
	}
}

func TestAssembleContext_EmptyParts(t *testing.T) {
	ctx := context.Background()
	result, _ := AssembleContext(ctx, "", "", "", protocol.TaintNone, nil, "")
	if result != "" {
		t.Errorf("expected empty string, got %q", result)
	}
}

func TestValidateZoneWrite_Immutable_RejectsHigh(t *testing.T) {
	err := ValidateZoneWrite(ZoneImmutable, protocol.TaintHigh)
	if err == nil {
		t.Error("expected error for TaintHigh in ZoneImmutable")
	}
}

func TestValidateZoneWrite_Immutable_AcceptsNone(t *testing.T) {
	err := ValidateZoneWrite(ZoneImmutable, protocol.TaintNone)
	if err != nil {
		t.Errorf("unexpected error for TaintNone in ZoneImmutable: %v", err)
	}
}

func TestValidateZoneWrite_Immutable_AcceptsLow(t *testing.T) {
	err := ValidateZoneWrite(ZoneImmutable, protocol.TaintLow)
	if err != nil {
		t.Errorf("unexpected error for TaintLow in ZoneImmutable: %v", err)
	}
}

func TestValidateZoneWrite_MutableSkill_RejectsMedium(t *testing.T) {
	err := ValidateZoneWrite(ZoneMutableSkill, protocol.TaintMedium)
	if err == nil {
		t.Error("expected error for TaintMedium in ZoneMutableSkill")
	}
}

func TestValidateZoneWrite_TaintedData_AcceptsAny(t *testing.T) {
	for _, tl := range []protocol.TaintLevel{
		protocol.TaintNone, protocol.TaintLow, protocol.TaintMedium,
		protocol.TaintHigh, protocol.TaintUserReviewed,
	} {
		if err := ValidateZoneWrite(ZoneTaintedData, tl); err != nil {
			t.Errorf("unexpected error for taint=%s in ZoneTaintedData: %v", tl.String(), err)
		}
	}
}

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
