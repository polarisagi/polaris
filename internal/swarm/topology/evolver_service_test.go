package topology

import (
	"testing"
	"time"
)

// MockTrafficSplitter for testing
type MockTrafficSplitter struct {
	Percent   int
	RouteVal  string
	Rollbacks int
}

func (m *MockTrafficSplitter) SetPercent(percent int) {
	m.Percent = percent
}

func (m *MockTrafficSplitter) Route(sessionID string) string {
	if m.RouteVal != "" {
		return m.RouteVal
	}
	return "baseline"
}

func (m *MockTrafficSplitter) Rollback() {
	m.Rollbacks++
}

func TestTopologyEvolverService_ProposeCandidateTopology(t *testing.T) {
	splitter := &MockTrafficSplitter{}
	svc := NewTopologyEvolverService("base", splitter)

	// Sample size too small
	svc.ProposeCandidateTopology("cand", &TopologyFitness{SampleSize: 40})
	if svc.phase != EvolverPhaseIdle {
		t.Errorf("Expected Idle phase, got %v", svc.phase)
	}

	// Valid proposal
	svc.ProposeCandidateTopology("cand", &TopologyFitness{SampleSize: 50, SuccessRate: 0.8})
	if svc.phase != EvolverPhaseShadow {
		t.Errorf("Expected Shadow phase, got %v", svc.phase)
	}
	if svc.candidate != "cand" {
		t.Errorf("Expected candidate 'cand', got %s", svc.candidate)
	}
	if splitter.Percent != 0 {
		t.Errorf("Expected splitter percent 0, got %d", splitter.Percent)
	}

	// Second proposal should be ignored
	svc.ProposeCandidateTopology("cand2", &TopologyFitness{SampleSize: 50})
	if svc.candidate != "cand" {
		t.Errorf("Expected candidate 'cand', got %s", svc.candidate)
	}
}

func TestTopologyEvolverService_RecordOutcome_Shadow(t *testing.T) {
	splitter := &MockTrafficSplitter{}
	svc := NewTopologyEvolverService("base", splitter)

	// Prepare baseline
	svc.evolver = &TopologyEvolver{}
	svc.evolver.RecordSample(&TopologyFitness{Topology: "base", TaskType: "t1", SuccessRate: 0.5, SampleSize: 100})

	svc.ProposeCandidateTopology("cand", &TopologyFitness{SampleSize: 50, SuccessRate: 0.5})

	// Record 49 outcomes for cand
	for range 49 {
		svc.RecordOutcome("cand", "t1", true, 10.0)
	}
	if svc.phase != EvolverPhaseShadow {
		t.Errorf("Expected Shadow phase, got %v", svc.phase)
	}

	// 50th outcome triggers evaluation
	// Cand will have high success rate (all true), base is 0.5. Should pass.
	svc.RecordOutcome("cand", "t1", true, 10.0)

	if svc.phase != EvolverPhaseAB {
		t.Errorf("Expected AB phase, got %v", svc.phase)
	}
	if splitter.Percent != 50 {
		t.Errorf("Expected splitter percent 50, got %d", splitter.Percent)
	}
}

func TestTopologyEvolverService_RecordOutcome_Shadow_FailedEval(t *testing.T) {
	splitter := &MockTrafficSplitter{}
	svc := NewTopologyEvolverService("base", splitter)

	// Prepare baseline with high success rate
	svc.evolver = &TopologyEvolver{}
	svc.evolver.RecordSample(&TopologyFitness{Topology: "base", TaskType: "t1", SuccessRate: 0.9, SampleSize: 100})

	svc.ProposeCandidateTopology("cand", &TopologyFitness{SampleSize: 50, SuccessRate: 0.9})

	// Cand outcomes all false => low success rate
	for range 50 {
		svc.RecordOutcome("cand", "t1", false, 10.0)
	}

	if svc.phase != EvolverPhaseIdle {
		t.Errorf("Expected Idle phase due to reset, got %v", svc.phase)
	}
	if svc.candidate != "" {
		t.Errorf("Expected empty candidate, got %s", svc.candidate)
	}
}

func TestTopologyEvolverService_RecordOutcome_AB(t *testing.T) {
	splitter := &MockTrafficSplitter{}
	svc := NewTopologyEvolverService("base", splitter)

	svc.ProposeCandidateTopology("cand", &TopologyFitness{SampleSize: 50, SuccessRate: 0.5})
	svc.phase = EvolverPhaseAB
	svc.abBaseline = 0.5

	// Need 50 tasks for cand
	for range 49 {
		svc.RecordOutcome("cand", "t1", true, 10.0)
	}
	if svc.phase != EvolverPhaseAB {
		t.Errorf("Expected AB phase, got %v", svc.phase)
	}

	// 50th task triggers graduation to Gradual (success rate 1.0 > 0.5)
	svc.RecordOutcome("cand", "t1", true, 10.0)

	if svc.phase != EvolverPhaseGradual {
		t.Errorf("Expected Gradual phase, got %v", svc.phase)
	}
	if splitter.Percent != 100 {
		t.Errorf("Expected splitter percent 100, got %d", splitter.Percent)
	}
}

func TestTopologyEvolverService_RecordOutcome_AB_Rollback(t *testing.T) {
	splitter := &MockTrafficSplitter{}
	svc := NewTopologyEvolverService("base", splitter)

	svc.ProposeCandidateTopology("cand", &TopologyFitness{SampleSize: 50, SuccessRate: 0.9})
	svc.phase = EvolverPhaseAB
	svc.abBaseline = 0.9

	// Cand outcomes all false => rate 0.0 < 0.9 - 0.05
	for range 50 {
		svc.RecordOutcome("cand", "t1", false, 10.0)
	}

	if svc.phase != EvolverPhaseIdle {
		t.Errorf("Expected Idle phase due to rollback, got %v", svc.phase)
	}
	if splitter.Rollbacks != 1 {
		t.Errorf("Expected 1 rollback, got %d", splitter.Rollbacks)
	}
}

func TestTopologyEvolverService_RecordOutcome_Gradual(t *testing.T) {
	splitter := &MockTrafficSplitter{}
	svc := NewTopologyEvolverService("base", splitter)

	svc.phase = EvolverPhaseGradual
	svc.candidate = "cand"
	svc.gradualStart = time.Now().Add(-8 * 24 * time.Hour) // More than 7 days ago

	svc.RecordOutcome("cand", "t1", true, 10.0)

	if svc.phase != EvolverPhaseCommit {
		t.Errorf("Expected Commit phase, got %v", svc.phase)
	}
	if svc.baseline != "cand" {
		t.Errorf("Expected new baseline 'cand', got %s", svc.baseline)
	}
	if svc.candidate != "" {
		t.Errorf("Expected empty candidate, got %s", svc.candidate)
	}
}

func TestTopologyEvolverService_RouteTopology(t *testing.T) {
	splitter := &MockTrafficSplitter{RouteVal: "cand"}
	svc := NewTopologyEvolverService("base", splitter)
	svc.candidate = "cand"

	// Idle phase -> base
	if r := svc.RouteTopology("s1"); r != "base" {
		t.Errorf("Expected base, got %s", r)
	}

	// AB phase -> splitter
	svc.phase = EvolverPhaseAB
	if r := svc.RouteTopology("s1"); r != "cand" {
		t.Errorf("Expected cand, got %s", r)
	}
}

func TestTopologyEvolver_Evaluate(t *testing.T) {
	te := &TopologyEvolver{
		fitnessMap: make(map[string]*TopologyFitness),
	}

	cand := &TopologyFitness{Topology: "cand", TaskType: "t1", SampleSize: 9}
	if te.Evaluate(cand, "base") {
		t.Errorf("Expected false for small sample size")
	}

	cand = &TopologyFitness{Topology: "cand", TaskType: "t1", SampleSize: 10, SuccessRate: 0.8, AvgTokenCost: 100}

	// Base not found -> true
	if !te.Evaluate(cand, "base") {
		t.Errorf("Expected true for no base")
	}

	// Base found, cand < base + 0.05
	te.fitnessMap["base|t1"] = &TopologyFitness{Topology: "base", TaskType: "t1", SampleSize: 10, SuccessRate: 0.8, AvgTokenCost: 100}
	if te.Evaluate(cand, "base") {
		t.Errorf("Expected false for small success lead")
	}

	// Cand >= base + 0.05
	cand.SuccessRate = 0.86
	if !te.Evaluate(cand, "base") {
		t.Errorf("Expected true for success lead")
	}

	// Cand cost too high
	cand.AvgTokenCost = 111
	if te.Evaluate(cand, "base") {
		t.Errorf("Expected false for high cost")
	}
}

func TestTopologyEvolver_RecordSample(t *testing.T) {
	te := &TopologyEvolver{}

	te.RecordSample(&TopologyFitness{Topology: "cand", TaskType: "t1", SuccessRate: 1.0, AvgTokenCost: 100, SampleSize: 1})

	f := te.GetFitness("cand", "t1")
	if f == nil {
		t.Fatalf("Expected non-nil fitness")
	}
	if f.SuccessRate != 1.0 {
		t.Errorf("Expected rate 1.0, got %f", f.SuccessRate)
	}

	te.RecordSample(&TopologyFitness{Topology: "cand", TaskType: "t1", SuccessRate: 0.0, AvgTokenCost: 200, SampleSize: 1})

	f = te.GetFitness("cand", "t1")
	// EWMA: 1.0 * 0.9 + 0.0 * 0.1 = 0.9
	if f.SuccessRate != 0.9 {
		t.Errorf("Expected rate 0.9, got %f", f.SuccessRate)
	}
	// EWMA cost: 100 * 0.9 + 200 * 0.1 = 110
	if f.AvgTokenCost != 110 {
		t.Errorf("Expected cost 110, got %f", f.AvgTokenCost)
	}
	if f.SampleSize != 2 {
		t.Errorf("Expected sample size 2, got %d", f.SampleSize)
	}
}
