package knowledge

import (
	"testing"
	"time"
)

func TestArbitrate_Empty(t *testing.T) {
	a := NewKnowledgeConflictArbiter()
	winner, reason := a.Arbitrate(nil)
	if winner != nil {
		t.Error("expected nil winner for empty candidates")
	}
	if reason != "no candidates" {
		t.Errorf("expected 'no candidates', got %s", reason)
	}
}

func TestArbitrate_Single(t *testing.T) {
	a := NewKnowledgeConflictArbiter()
	cand := ConflictCandidate{Content: "only one", SourceType: "blog"}
	winner, reason := a.Arbitrate([]ConflictCandidate{cand})
	if winner == nil {
		t.Fatal("expected winner for single candidate")
	}
	if reason != "single_candidate" {
		t.Errorf("expected 'single_candidate', got %s", reason)
	}
}

func TestArbitrate_AuthorityTier(t *testing.T) {
	a := NewKnowledgeConflictArbiter()
	candidates := []ConflictCandidate{
		{Content: "blog says X", SourceType: "blog", UpdatedAt: time.Now()},
		{Content: "official says Y", SourceType: "official", UpdatedAt: time.Now()},
	}
	winner, reason := a.Arbitrate(candidates)
	if winner == nil {
		t.Fatal("expected winner")
	}
	if winner.SourceType != "official" {
		t.Errorf("expected official to win by authority, got %s (reason=%s)", winner.SourceType, reason)
	}
	if reason != "authority_tier" {
		t.Errorf("expected authority_tier reason, got %s", reason)
	}
}

func TestArbitrate_Recency_SameTier(t *testing.T) {
	a := NewKnowledgeConflictArbiter()
	old := time.Now().Add(-48 * time.Hour)
	recent := time.Now()
	candidates := []ConflictCandidate{
		{Content: "old blog", SourceType: "blog", UpdatedAt: old},
		{Content: "new blog", SourceType: "blog", UpdatedAt: recent},
	}
	winner, reason := a.Arbitrate(candidates)
	if winner == nil {
		t.Fatal("expected winner")
	}
	if winner.Content != "new blog" {
		t.Errorf("expected newer blog to win, got %s (reason=%s)", winner.Content, reason)
	}
	if reason != "recency" {
		t.Errorf("expected recency reason, got %s", reason)
	}
}

func TestArbitrate_Consensus(t *testing.T) {
	a := NewKnowledgeConflictArbiter()
	now := time.Now()
	// 3 blogs with same tier + same time → consensus
	candidates := []ConflictCandidate{
		{Content: "apples are red fruit", SourceType: "blog", UpdatedAt: now},
		{Content: "apples are red fruit too", SourceType: "blog", UpdatedAt: now},
		{Content: "oranges are orange", SourceType: "blog", UpdatedAt: now},
	}
	winner, reason := a.Arbitrate(candidates)
	if winner == nil {
		t.Fatal("expected winner from consensus")
	}
	if reason != "consensus" {
		t.Errorf("expected consensus reason, got %s", reason)
	}
	// majority content should contain "apples"
	if winner.Content == "oranges are orange" {
		t.Error("oranges should not win - only 1 vote vs 2 for apples")
	}
}

func TestAuthorityTier_Order(t *testing.T) {
	pairs := []struct {
		higher, lower string
	}{
		{"official", "book"},
		{"book", "blog"},
		{"blog", "episodic"},
		{"spec", "kb_web"},
	}
	for _, p := range pairs {
		if authorityTier(p.higher) <= authorityTier(p.lower) {
			t.Errorf("%s should have higher tier than %s", p.higher, p.lower)
		}
	}
}

func TestArbitrateChunks_NoConflict(t *testing.T) {
	a := NewKnowledgeConflictArbiter()
	chunks := []Chunk{
		{ID: "c1", DocID: "d1", SectionPath: []string{"intro"}, Content: "hello"},
		{ID: "c2", DocID: "d2", SectionPath: []string{"body"}, Content: "world"},
	}
	result := a.ArbitrateChunks(chunks)
	if len(result) != 2 {
		t.Errorf("expected 2 chunks (different sections), got %d", len(result))
	}
}

func TestArbitrateChunks_ConflictResolved(t *testing.T) {
	a := NewKnowledgeConflictArbiter()
	chunks := []Chunk{
		{ID: "c1", DocID: "d1", SectionPath: []string{"sec1"}, Content: "fact A", TaintSource: "official"},
		{ID: "c2", DocID: "d2", SectionPath: []string{"sec1"}, Content: "fact B", TaintSource: "blog"},
	}
	result := a.ArbitrateChunks(chunks)
	if len(result) != 1 {
		t.Fatalf("expected 1 chunk after conflict resolution, got %d", len(result))
	}
	if result[0].Content != "fact A" {
		t.Errorf("expected official source to win, got %s", result[0].Content)
	}
}

func TestArbitrateChunks_Empty(t *testing.T) {
	a := NewKnowledgeConflictArbiter()
	result := a.ArbitrateChunks(nil)
	if result != nil {
		t.Errorf("expected nil for empty input, got %v", result)
	}
}
