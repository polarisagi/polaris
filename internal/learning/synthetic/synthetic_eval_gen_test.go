package synthetic

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// mockProvider 顺序返回预设 JSON 响应（线程安全）。
type mockProvider struct {
	mu        sync.Mutex
	responses []string
	idx       int
}

func (m *mockProvider) Infer(_ context.Context, _ []types.Message, _ ...types.InferOption) (*types.ProviderResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.idx >= len(m.responses) {
		return nil, fmt.Errorf("mockProvider: no more responses (idx=%d)", m.idx)
	}
	resp := m.responses[m.idx]
	m.idx++
	return &types.ProviderResponse{Content: resp}, nil
}

func (m *mockProvider) StreamInfer(_ context.Context, _ []types.Message, _ ...types.InferOption) (<-chan types.StreamEvent, error) {
	ch := make(chan types.StreamEvent)
	close(ch)
	return ch, nil
}

func (m *mockProvider) Capabilities() types.ProviderCapabilities {
	return types.ProviderCapabilities{}
}

func (m *mockProvider) Tokenizer() protocol.TokenizerAdapter { return nil }
func (m *mockProvider) ModelID() string                      { return "mock" }

// ── NewEvalGenerator ─────────────────────────────────────────────────────────

func TestNewEvalGenerator_Defaults(t *testing.T) {
	g := NewEvalGenerator(true, nil)
	if g == nil {
		t.Fatal("expected non-nil generator")
	}
	if g.TargetRatio != 0.05 {
		t.Errorf("expected TargetRatio=0.05, got %f", g.TargetRatio)
	}
	if !g.Enabled {
		t.Error("expected Enabled=true")
	}
}

func TestNewEvalGenerator_Disabled(t *testing.T) {
	g := NewEvalGenerator(false, nil)
	if g.Enabled {
		t.Error("expected Enabled=false")
	}
}

// ── GenerateCases 早期退出 ────────────────────────────────────────────────────

func TestGenerateCases_Disabled_ReturnsNil(t *testing.T) {
	g := NewEvalGenerator(false, &mockProvider{})
	cases, err := g.GenerateCases(context.Background(), []string{"some chunk"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cases != nil {
		t.Error("disabled generator should return nil cases")
	}
}

func TestGenerateCases_NilProvider_ReturnsNil(t *testing.T) {
	g := NewEvalGenerator(true, nil)
	cases, err := g.GenerateCases(context.Background(), []string{"some chunk"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cases != nil {
		t.Error("nil provider should return nil cases")
	}
}

func TestGenerateCases_EmptyChunks_ReturnsNil(t *testing.T) {
	g := NewEvalGenerator(true, &mockProvider{})
	cases, err := g.GenerateCases(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cases != nil {
		t.Error("empty chunks should return nil cases")
	}

	cases, err = g.GenerateCases(context.Background(), []string{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cases != nil {
		t.Error("empty slice should return nil cases")
	}
}

// ── GenerateCases 全流水线（index 0 → 无 evolution）────────────────────────

func TestGenerateCases_SingleChunk_FactualResult(t *testing.T) {
	// index=0 → i%3==0 → 跳过 Stage 2；只需 Stage1 + Stage3 两次 LLM 调用
	provider := &mockProvider{
		responses: []string{
			`{"question":"光合作用的产物是什么？","answer":"光合作用的产物是氧气和葡萄糖。"}`,
			`{"grounded":true,"reason":"答案可从文本中直接找到"}`,
		},
	}
	g := NewEvalGenerator(true, provider)
	g.TargetRatio = 1.0 // 确保目标数量 >= 1

	cases, err := g.GenerateCases(context.Background(), []string{"光合作用将二氧化碳和水转化为氧气和葡萄糖。"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cases) != 1 {
		t.Fatalf("expected 1 case, got %d", len(cases))
	}
	c := cases[0]
	if c.Question == "" {
		t.Error("question should not be empty")
	}
	if c.GroundTruth == "" {
		t.Error("ground truth should not be empty")
	}
	if !c.ContextBound {
		t.Error("context_bound should be true after groundedness validation")
	}
	if c.Type != QTypeFactual {
		t.Errorf("expected factual type, got %q", c.Type)
	}
	if c.Difficulty != DiffEasy {
		t.Errorf("expected easy difficulty, got %q", c.Difficulty)
	}
	if !strings.HasPrefix(c.ID, "syn_") {
		t.Errorf("case ID should have syn_ prefix, got %q", c.ID)
	}
	if c.ChunkID == "" {
		t.Error("chunk_id should not be empty")
	}
}

func TestGenerateCases_ReasoningEvolution(t *testing.T) {
	// index=1 → i%3==1 → Stage1 + Stage2a(reasoning) + Stage3
	provider2 := &mockProvider{
		responses: []string{
			// chunk[0] index=0: Stage1 成功，Stage3 grounded=false → 丢弃
			`{"question":"a？","answer":"a"}`,
			`{"grounded":false,"reason":"不在文本中"}`,
			// chunk[1] index=1: Stage1 + Stage2a + Stage3
			`{"question":"牛顿第一定律说了什么？","answer":"物体保持匀速直线运动或静止。"}`,
			`{"question":"如果没有外力作用，一个运动的物体最终会怎样？","answer":"将永远保持匀速直线运动。"}`,
			`{"grounded":true,"reason":"答案源于牛顿第一定律文本"}`,
		},
	}
	g2 := NewEvalGenerator(true, provider2)
	g2.TargetRatio = 1.0

	chunks := []string{"无关文本", "牛顿第一定律：物体在没有外力作用下保持匀速直线运动或静止。"}
	cases, err := g2.GenerateCases(context.Background(), chunks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cases) != 1 {
		t.Fatalf("expected 1 case, got %d", len(cases))
	}
	if cases[0].Type != QTypeMultiHop {
		t.Errorf("expected multi_hop type after reasoning evolution, got %q", cases[0].Type)
	}
	if cases[0].Difficulty != DiffMedium {
		t.Errorf("expected medium difficulty, got %q", cases[0].Difficulty)
	}
}

func TestGenerateCases_GroundednessFail_Discards(t *testing.T) {
	provider := &mockProvider{
		responses: []string{
			`{"question":"太阳系有几个行星？","answer":"8个"}`,
			`{"grounded":false,"reason":"答案不在文本中"}`,
		},
	}
	g := NewEvalGenerator(true, provider)
	g.TargetRatio = 1.0

	cases, err := g.GenerateCases(context.Background(), []string{"这段文本不包含太阳系行星数量的信息。"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cases) != 0 {
		t.Errorf("groundedness failure should discard case, got %d cases", len(cases))
	}
}

func TestGenerateCases_EmptyChunkSkipped(t *testing.T) {
	// chunk[0]="" 跳过；chunk[1] 在 index=1 → i%3==1 → Stage1+Stage2a+Stage3
	provider := &mockProvider{
		responses: []string{
			`{"question":"Q？","answer":"A"}`,
			`{"question":"更复杂的Q？","answer":"A"}`,
			`{"grounded":true,"reason":"ok"}`,
		},
	}
	g := NewEvalGenerator(true, provider)
	g.TargetRatio = 1.0

	chunks := []string{"", "有效文本内容，包含可提问的信息。"}
	cases, err := g.GenerateCases(context.Background(), chunks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 空 chunk 跳过，有效 chunk 生成 1 条
	if len(cases) != 1 {
		t.Errorf("expected 1 case (empty chunk skipped), got %d", len(cases))
	}
}

// ── synChunkID ────────────────────────────────────────────────────────────────

func TestChunkID_Deterministic(t *testing.T) {
	id1 := synChunkID("hello world")
	id2 := synChunkID("hello world")
	if id1 != id2 {
		t.Errorf("synChunkID must be deterministic: %q != %q", id1, id2)
	}
}

func TestChunkID_Length(t *testing.T) {
	// SHA-256 前 8 字节 → 16 hex 字符
	id := synChunkID("some text")
	if len(id) != 16 {
		t.Errorf("expected 16 hex chars, got %d: %q", len(id), id)
	}
}

func TestChunkID_DifferentInputs(t *testing.T) {
	if synChunkID("a") == synChunkID("b") {
		t.Error("different inputs should (almost certainly) produce different IDs")
	}
}

func TestChunkID_Empty(t *testing.T) {
	id := synChunkID("")
	if len(id) != 16 {
		t.Errorf("empty string should still produce 16 hex chars, got %d", len(id))
	}
}

// ── synCaseID ─────────────────────────────────────────────────────────────────

func TestCaseID_Prefix(t *testing.T) {
	id := synCaseID("What is X?")
	if !strings.HasPrefix(id, "syn_") {
		t.Errorf("synCaseID should have syn_ prefix, got %q", id)
	}
}

func TestCaseID_Length(t *testing.T) {
	// "syn_" + 6 字节 × 2 hex = 4 + 12 = 16 字符
	id := synCaseID("test question")
	if len(id) != 16 {
		t.Errorf("expected 16 chars (syn_ + 12 hex), got %d: %q", len(id), id)
	}
}

func TestCaseID_Deterministic(t *testing.T) {
	id1 := synCaseID("q")
	id2 := synCaseID("q")
	if id1 != id2 {
		t.Error("synCaseID must be deterministic")
	}
}

// ── synNgramFingerprint ───────────────────────────────────────────────────────

func TestNgramFingerprint_Deterministic(t *testing.T) {
	fp1 := synNgramFingerprint("the quick brown fox", 3)
	fp2 := synNgramFingerprint("the quick brown fox", 3)
	if fp1 != fp2 {
		t.Error("synNgramFingerprint must be deterministic")
	}
}

func TestNgramFingerprint_DifferentTexts(t *testing.T) {
	fp1 := synNgramFingerprint("the quick brown fox", 3)
	fp2 := synNgramFingerprint("a slow red turtle", 3)
	if fp1 == fp2 {
		t.Error("different texts should (almost certainly) have different fingerprints")
	}
}

func TestNgramFingerprint_ShortText_FallsBackToFullHash(t *testing.T) {
	// 单词数 < n → 走 sha256 全文哈希路径
	fp := synNgramFingerprint("only", 3)
	if fp == 0 {
		t.Error("short text fingerprint should be non-zero")
	}
	// 单词 == 0（空字符串）
	fpEmpty := synNgramFingerprint("", 3)
	if fpEmpty == 0 {
		t.Error("empty text fingerprint should be non-zero")
	}
}

func TestNgramFingerprint_CaseInsensitive(t *testing.T) {
	fp1 := synNgramFingerprint("The Quick Brown", 3)
	fp2 := synNgramFingerprint("the quick brown", 3)
	if fp1 != fp2 {
		t.Error("synNgramFingerprint should be case-insensitive")
	}
}

// ── synTruncate ───────────────────────────────────────────────────────────────

func TestTruncate_ShortString_Unchanged(t *testing.T) {
	s := "hello"
	if synTruncate(s, 100) != s {
		t.Error("string shorter than maxBytes should be returned unchanged")
	}
}

func TestTruncate_ExactLength_Unchanged(t *testing.T) {
	s := "hello"
	if synTruncate(s, 5) != s {
		t.Error("string exactly at maxBytes should be returned unchanged")
	}
}

func TestTruncate_LongString_Truncated(t *testing.T) {
	s := "0123456789"
	out := synTruncate(s, 5)
	if len([]byte(out)) <= 5 {
		// 截断后追加了 "…"（3 字节 UTF-8），内容部分是 5 字节
		if !strings.HasSuffix(out, "…") {
			t.Errorf("truncated string should end with ellipsis, got %q", out)
		}
	}
	if out[:5] != "01234" {
		t.Errorf("truncated prefix should be first 5 bytes, got %q", out[:5])
	}
}

func TestTruncate_Empty(t *testing.T) {
	if synTruncate("", 10) != "" {
		t.Error("empty string should remain empty")
	}
}

// TestCaseSeverityConstants 验证 CaseSeverity 常量值与 governance/eval.Severity 语义对齐。
// ToEvalCase 转换逻辑已移至 pkg/governance/eval.SyntheticCaseToEvalCase（L3 侧 adapter）。
func TestCaseSeverityConstants(t *testing.T) {
	if CaseSeverityP0 != "P0" {
		t.Errorf("CaseSeverityP0 should be 'P0', got %q", CaseSeverityP0)
	}
	if CaseSeverityP1 != "P1" {
		t.Errorf("CaseSeverityP1 should be 'P1', got %q", CaseSeverityP1)
	}
	if CaseSeverityP2 != "P2" {
		t.Errorf("CaseSeverityP2 should be 'P2', got %q", CaseSeverityP2)
	}
}

func TestSyntheticCase_SeverityField(t *testing.T) {
	s := SyntheticCase{ID: "test-id", Severity: CaseSeverityP2}
	if s.Severity != CaseSeverityP2 {
		t.Errorf("expected %q, got %q", CaseSeverityP2, s.Severity)
	}
	// 默认零值为空字符串；adapter 层（governance/eval）负责将空值升为 P2
	var empty SyntheticCase
	if empty.Severity != "" {
		t.Errorf("zero value Severity should be empty string, got %q", empty.Severity)
	}
}
