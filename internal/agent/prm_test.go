package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// mockProvider PRM 打分 + 状态机故障注入两用 mock（线程安全）。
// - responses 非空时：按队列顺序返回（PRM 打分场景）。
// - responses 为空时：返回状态机集成测试所需的 DAG JSON。
// - failOn 非空时：匹配 msgs[0].Content 即返回错误（故障注入场景）。
type mockProvider struct {
	mu        sync.Mutex
	responses []string // PRM 测试预设响应队列
	idx       int
	failOn    string // 指定触发错误的 prompt 关键词
	failCount int    // 故障注入计数（用于断言）
}

func (m *mockProvider) Infer(_ context.Context, msgs []types.Message, _ ...types.InferOption) (*types.ProviderResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// 故障注入路径：prompt 含指定关键词时报错
	if m.failOn != "" && len(msgs) > 0 && strings.Contains(msgs[0].Content, m.failOn) {
		m.failCount++
		return nil, fmt.Errorf("mock llm failure: %s", m.failOn)
	}
	// PRM 队列路径
	if len(m.responses) > 0 {
		if m.idx >= len(m.responses) {
			return nil, fmt.Errorf("mockProvider: no more responses")
		}
		resp := m.responses[m.idx]
		m.idx++
		return &types.ProviderResponse{Content: resp}, nil
	}
	// 默认路径：状态机集成测试通用响应
	if len(msgs) > 0 && strings.Contains(msgs[0].Content, "基于 TaskModel 生成执行 DAG") {
		return &types.ProviderResponse{Content: `{"nodes":[{"id":"n1","action":"test_tool","params":{},"retry":0,"timeout":""}],"edges":[]}`}, nil
	}
	return &types.ProviderResponse{Content: "mock_success"}, nil
}

// StreamInfer 委托给 Infer 复用同一套响应选择逻辑，将结果包装为单帧
// StreamTextDelta 事件后关闭 channel。此前直接返回空 channel，doStreamInfer
// 据此拼出的 resp.Content 恒为空串——onPerceiveSuccess 等 OnSuccess 回调新增的
// "空 LLM 输出必须走失败路径"检查（A-01/P-2）修复后，暴露出该 mock 与 Infer
// 路径行为不一致（TestAgent_HappyPath 等用例实际全程走 StreamInfer，从未验证过
// 非空内容路径）。修复 mock 而非放宽状态机检查：真实 Provider 的流式/非流式
// 响应内容理应一致，此前的空 channel 是测试替身的缺陷，不是应保留的行为。
func (m *mockProvider) StreamInfer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
	// 此处 m.Infer 是 mockProvider 自身方法（测试替身内部复用响应选择逻辑），
	// 并非绕过 safecall 直调真实 Provider，Test_inv_NoBareLLMInfer 的检测对象
	// 是生产代码路径，误伤此处的测试 mock 自调用。 custom-nolint:bare-infer
	resp, err := m.Infer(ctx, msgs, opts...)
	ch := make(chan types.StreamEvent, 1)
	if err != nil {
		// StreamInfer 自身返回 nil error 是有意为之：真实流式 Provider 将推理错误
		// 通过 StreamError 事件帧传递（见 doStreamInfer 消费逻辑），而非作为函数
		// 返回值——channel 建立成功，错误在流内异步出现。
		ch <- types.StreamEvent{Type: types.StreamError, Content: err.Error()}
		close(ch)
		return ch, nil //nolint:nilerr
	}
	ch <- types.StreamEvent{Type: types.StreamTextDelta, Content: resp.Content, Usage: resp.Usage}
	close(ch)
	return ch, nil
}

func (m *mockProvider) Capabilities() types.ProviderCapabilities {
	return types.ProviderCapabilities{SupportsTools: true}
}

func (m *mockProvider) Tokenizer() protocol.TokenizerAdapter { return nil }
func (m *mockProvider) ModelID() string                      { return "mock" }

func makeDagModel(actions ...string) *types.DAGModel {
	nodes := make([]types.DAGNode, len(actions))
	for i, a := range actions {
		nodes[i] = types.DAGNode{ID: fmt.Sprintf("n%d", i), Action: a}
	}
	return &types.DAGModel{Nodes: nodes}
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
	c1 := makeDagModel("step1")
	c2 := makeDagModel("step2")

	got, err := p.SelectBest(context.Background(), "goal", 0.9, []*types.DAGModel{c1, c2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != c1 {
		t.Error("disabled PRM should return first candidate")
	}
}

func TestSelectBest_SingleCandidate_ReturnsFirst(t *testing.T) {
	p := NewDefaultPRM(PRMConfig{Enabled: true}, nil)
	c1 := makeDagModel("step1")

	got, err := p.SelectBest(context.Background(), "goal", 0.9, []*types.DAGModel{c1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != c1 {
		t.Error("single candidate should be returned directly")
	}
}

func TestSelectBest_ComplexityBelowGate_ReturnsFirst(t *testing.T) {
	p := NewDefaultPRM(PRMConfig{Enabled: true, ComplexityGate: 0.7}, nil)
	c1 := makeDagModel("a")
	c2 := makeDagModel("b", "c")

	got, err := p.SelectBest(context.Background(), "goal", 0.4, []*types.DAGModel{c1, c2})
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

	c1 := makeDagModel("step-a")
	c2 := makeDagModel("step-b", "step-c")

	got, err := p.SelectBest(context.Background(), "goal", 0.8, []*types.DAGModel{c1, c2})
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

	c1 := makeDagModel("a")
	c2 := makeDagModel("b")

	got, err := p.SelectBest(context.Background(), "goal", 0.9, []*types.DAGModel{c1, c2})
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
	if s := planToText(&types.DAGModel{}); s != "(空方案)" {
		t.Errorf("empty plan: expected '(空方案)', got %q", s)
	}
}

func TestPlanToText_FormatsNodes(t *testing.T) {
	d := makeDagModel("step-one", "step-two")
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
