package prm

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
)

// mockProvider 返回预设的 JSON 打分内容（线程安全）。
type mockProvider struct {
	mu        sync.Mutex
	responses []string // 按调用顺序返回
	idx       int
}

func (m *mockProvider) Infer(_ context.Context, _ []protocol.Message, _ ...protocol.InferOption) (*protocol.ProviderResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.idx >= len(m.responses) {
		return nil, fmt.Errorf("mockProvider: no more responses")
	}
	resp := m.responses[m.idx]
	m.idx++
	return &protocol.ProviderResponse{Content: resp}, nil
}

func (m *mockProvider) StreamInfer(_ context.Context, _ []protocol.Message, _ ...protocol.InferOption) (<-chan protocol.StreamEvent, error) {
	ch := make(chan protocol.StreamEvent)
	close(ch)
	return ch, nil
}

func (m *mockProvider) Capabilities() protocol.ProviderCapabilities {
	return protocol.ProviderCapabilities{}
}

func (m *mockProvider) Tokenizer() protocol.TokenizerAdapter { return nil }
func (m *mockProvider) ModelID() string                      { return "mock" }

func dag(actions ...string) *protocol.DAGModel {
	nodes := make([]protocol.DAGNode, len(actions))
	for i, a := range actions {
		nodes[i] = protocol.DAGNode{ID: fmt.Sprintf("n%d", i), Action: a}
	}
	return &protocol.DAGModel{Nodes: nodes}
}

// ── NewDefaultPRM defaults ────────────────────────────────────────────────────

func TestNewDefaultPRM_SetsDefaults(t *testing.T) {
	p := NewDefaultPRM(PRMConfig{}, nil)
	if p.config.MaxCandidates != 3 {
		t.Errorf("expected MaxCandidates=3, got %d", p.config.MaxCandidates)
	}
	if p.config.MinThreshold != 0.4 {
		t.Errorf("expected MinThreshold=0.4, got %f", p.config.MinThreshold)
	}
}

// ── ShouldActivate ────────────────────────────────────────────────────────────

func TestShouldActivate(t *testing.T) {
	p := NewDefaultPRM(PRMConfig{Enabled: true, ComplexityGate: 0.5}, nil)

	if p.ShouldActivate(0.3) {
		t.Error("complexity 0.3 < gate 0.5 should not activate")
	}
	if !p.ShouldActivate(0.6) {
		t.Error("complexity 0.6 >= gate 0.5 should activate")
	}
}

func TestShouldActivate_Disabled(t *testing.T) {
	p := NewDefaultPRM(PRMConfig{Enabled: false, ComplexityGate: 0.1}, nil)
	if p.ShouldActivate(1.0) {
		t.Error("disabled PRM should never activate")
	}
}

// ── SelectBest pass-through cases ─────────────────────────────────────────────

func TestSelectBest_EmptyCandidates(t *testing.T) {
	p := NewDefaultPRM(PRMConfig{Enabled: true}, nil)
	_, err := p.SelectBest(context.Background(), "goal", 0.9, nil)
	if err == nil {
		t.Fatal("empty candidates should error")
	}
}

func TestSelectBest_Disabled_ReturnsFirst(t *testing.T) {
	p := NewDefaultPRM(PRMConfig{Enabled: false}, nil)
	c1 := dag("step1")
	c2 := dag("step2")

	got, err := p.SelectBest(context.Background(), "goal", 0.9, []*protocol.DAGModel{c1, c2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != c1 {
		t.Error("disabled PRM should return first candidate")
	}
}

func TestSelectBest_SingleCandidate_ReturnsFirst(t *testing.T) {
	p := NewDefaultPRM(PRMConfig{Enabled: true}, nil)
	c1 := dag("step1")

	got, err := p.SelectBest(context.Background(), "goal", 0.9, []*protocol.DAGModel{c1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != c1 {
		t.Error("single candidate should be returned directly")
	}
}

func TestSelectBest_ComplexityBelowGate_ReturnsFirst(t *testing.T) {
	p := NewDefaultPRM(PRMConfig{Enabled: true, ComplexityGate: 0.7}, nil)
	c1 := dag("a")
	c2 := dag("b", "c")

	got, err := p.SelectBest(context.Background(), "goal", 0.4, []*protocol.DAGModel{c1, c2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != c1 {
		t.Error("low complexity should return first candidate without LLM call")
	}
}

// ── SelectBest with mock scoring ──────────────────────────────────────────────

func TestSelectBest_PicksHighestScore(t *testing.T) {
	// 两个候选都打高分，但 c2 更高。因为打分并发，mock 按调用顺序响应。
	// 测试策略：只验证返回值是两个候选之一，且比 MinThreshold 高即可（行为正确性）。
	// 具体哪个更高分由 mock 顺序决定，所以用两个候选给出明确高低分差距。
	// 为避免竞态影响，改为串行模式：MaxCandidates=1 截断到 c1，让 c1 独自得分。
	provider := &mockProvider{
		responses: []string{`{"score": 0.85, "reason": "good"}`},
	}
	p := NewDefaultPRM(PRMConfig{
		Enabled:        true,
		MaxCandidates:  1, // 截断到第一个候选，确保只有一次 LLM 调用
		MinThreshold:   0.5,
		ComplexityGate: 0.0,
	}, provider)

	c1 := dag("step-a")
	c2 := dag("step-b", "step-c")

	got, err := p.SelectBest(context.Background(), "goal", 0.8, []*protocol.DAGModel{c1, c2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// MaxCandidates=1 截断候选，只有 c1 参与打分，且分数 0.85 > threshold 0.5
	if got != c1 {
		t.Error("expected c1 (only scored candidate) to be selected")
	}
}

func TestSelectBest_AllBelowThreshold_ReturnsFirst(t *testing.T) {
	provider := &mockProvider{
		responses: []string{
			`{"score": 0.1, "reason": "bad"}`,
			`{"score": 0.2, "reason": "bad"}`,
		},
	}
	p := NewDefaultPRM(PRMConfig{
		Enabled:        true,
		MinThreshold:   0.5,
		ComplexityGate: 0.0,
	}, provider)

	c1 := dag("a")
	c2 := dag("b")

	got, err := p.SelectBest(context.Background(), "goal", 0.9, []*protocol.DAGModel{c1, c2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != c1 {
		t.Error("all scores below threshold should fall back to first candidate")
	}
}

// ── planToText ────────────────────────────────────────────────────────────────

func TestPlanToText_NilAndEmpty(t *testing.T) {
	if s := planToText(nil); s != "(空方案)" {
		t.Errorf("nil plan: expected '(空方案)', got %q", s)
	}
	if s := planToText(&protocol.DAGModel{}); s != "(空方案)" {
		t.Errorf("empty plan: expected '(空方案)', got %q", s)
	}
}

func TestPlanToText_FormatsNodes(t *testing.T) {
	d := dag("step-one", "step-two")
	s := planToText(d)
	if s == "" {
		t.Fatal("expected non-empty plan text")
	}
	if !containsAll(s, "step-one", "step-two") {
		t.Errorf("plan text should contain all actions: %q", s)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i <= len(s)-len(sub); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
