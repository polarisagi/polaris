package cognition

import (
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
)

// ─── 辅助构造 ─────────────────────────────────────────────────────────────────

func toolResult(id, content string) map[string]any {
	return map[string]any{
		"type":        "tool_result",
		"tool_use_id": id,
		"content":     content,
	}
}

func toolResultMultiBlock(id string, texts ...string) map[string]any {
	var blocks []any //nolint:prealloc
	for _, t := range texts {
		blocks = append(blocks, map[string]any{"type": "text", "text": t})
	}
	return map[string]any{
		"type":        "tool_result",
		"tool_use_id": id,
		"content":     blocks,
	}
}

func textPart(text string) map[string]any {
	return map[string]any{"type": "text", "text": text}
}

func imagePart(dataLen int) map[string]any {
	return map[string]any{
		"type": "image",
		"source": map[string]any{
			"type":       "base64",
			"media_type": "image/jpeg",
			"data":       strings.Repeat("A", dataLen),
		},
	}
}

func imagePartWithMeta(w, h float64) map[string]any {
	return map[string]any{
		"type":  "image",
		"_meta": map[string]any{"width": w, "height": h},
	}
}

// ─── PruneToolOutputs ─────────────────────────────────────────────────────────

func TestPruneToolOutputs_NoPrune_UnderThreshold(t *testing.T) {
	msgs := []protocol.Message{
		{Role: "user", Parts: []any{toolResult("id1", "small content")}},
	}
	out, count := PruneToolOutputs(msgs, 100)
	if count != 0 {
		t.Errorf("want 0 pruned, got %d", count)
	}
	p := out[0].Parts[0].(map[string]any)
	if p["content"] != "small content" {
		t.Error("content should not be changed when under threshold")
	}
}

func TestPruneToolOutputs_PrunesLargeString(t *testing.T) {
	big := strings.Repeat("x", 200)
	msgs := []protocol.Message{
		{Role: "user", Parts: []any{toolResult("id42", big)}},
	}
	out, count := PruneToolOutputs(msgs, 100)
	if count != 1 {
		t.Errorf("want 1 pruned, got %d", count)
	}
	p := out[0].Parts[0].(map[string]any)
	c, _ := p["content"].(string)
	if !strings.Contains(c, "pruned") {
		t.Errorf("content should contain 'pruned', got %q", c)
	}
	if !strings.Contains(c, "id42") {
		t.Errorf("content should contain tool_use_id, got %q", c)
	}
}

func TestPruneToolOutputs_MultiBlockContent(t *testing.T) {
	// Anthropic 多块格式: content = []any{{"type":"text","text":"..."}, ...}
	part := toolResultMultiBlock("id99", strings.Repeat("a", 150), strings.Repeat("b", 60))
	msgs := []protocol.Message{{Role: "user", Parts: []any{part}}}
	_, count := PruneToolOutputs(msgs, 100)
	if count != 1 {
		t.Errorf("multi-block content (210 bytes total): want 1 pruned, got %d", count)
	}
}

func TestPruneToolOutputs_PreservesNonToolResultParts(t *testing.T) {
	msgs := []protocol.Message{
		{Role: "assistant", Parts: []any{
			textPart("assistant text"),
			toolResult("id1", strings.Repeat("y", 200)),
		}},
	}
	out, count := PruneToolOutputs(msgs, 100)
	if count != 1 {
		t.Fatalf("want 1 pruned, got %d", count)
	}
	tp := out[0].Parts[0].(map[string]any)
	if tp["type"] != "text" || tp["text"] != "assistant text" {
		t.Error("text part must not be modified")
	}
}

func TestPruneToolOutputs_OriginalUnmodified(t *testing.T) {
	bigContent := strings.Repeat("z", 200)
	orig := []protocol.Message{
		{Role: "user", Parts: []any{toolResult("id1", bigContent)}},
	}
	_, _ = PruneToolOutputs(orig, 100)
	// 原始消息不可变
	p := orig[0].Parts[0].(map[string]any)
	if p["content"] != bigContent {
		t.Error("original messages must not be mutated by PruneToolOutputs")
	}
}

func TestPruneToolOutputs_MultipleMessages(t *testing.T) {
	msgs := []protocol.Message{
		{Role: "user", Parts: []any{toolResult("t1", strings.Repeat("a", 200))}},
		{Role: "user", Parts: []any{toolResult("t2", "small")}},
		{Role: "user", Parts: []any{toolResult("t3", strings.Repeat("b", 300))}},
	}
	_, count := PruneToolOutputs(msgs, 100)
	if count != 2 {
		t.Errorf("want 2 pruned (t1, t3), got %d", count)
	}
}

func TestPruneToolOutputs_EmptyMessages(t *testing.T) {
	_, count := PruneToolOutputs(nil, 100)
	if count != 0 {
		t.Errorf("nil messages: want 0, got %d", count)
	}
}

// ─── EstimateImageTokens ──────────────────────────────────────────────────────

func TestEstimateImageTokens_NoImages(t *testing.T) {
	msgs := []protocol.Message{{Role: "user", Content: "no images here"}}
	if got := EstimateImageTokens(msgs); got != 0 {
		t.Errorf("no images: want 0, got %d", got)
	}
}

func TestEstimateImageTokens_LargeBase64(t *testing.T) {
	msgs := []protocol.Message{
		{Role: "user", Parts: []any{imagePart(100_000)}},
	}
	got := EstimateImageTokens(msgs)
	if got != imageTokensFullHD {
		t.Errorf("large base64: want %d (fullHD), got %d", imageTokensFullHD, got)
	}
}

func TestEstimateImageTokens_SmallBase64(t *testing.T) {
	msgs := []protocol.Message{
		{Role: "user", Parts: []any{imagePart(1_000)}},
	}
	got := EstimateImageTokens(msgs)
	if got != imageTokensSmall {
		t.Errorf("small base64: want %d (small), got %d", imageTokensSmall, got)
	}
}

func TestEstimateImageTokens_WithMeta_1024x1024(t *testing.T) {
	// tiles = ceil(1024/512) × ceil(1024/512) = 2×2 = 4; tokens = 4×170+85 = 765
	msgs := []protocol.Message{
		{Role: "user", Parts: []any{imagePartWithMeta(1024, 1024)}},
	}
	got := EstimateImageTokens(msgs)
	want := 4*170 + 85 // 765
	if got != want {
		t.Errorf("1024×1024 meta: want %d, got %d", want, got)
	}
}

func TestEstimateImageTokens_WithMeta_256x256(t *testing.T) {
	// tiles = ceil(256/512) × ceil(256/512) = 1×1 = 1; tokens = 1×170+85 = 255
	msgs := []protocol.Message{
		{Role: "user", Parts: []any{imagePartWithMeta(256, 256)}},
	}
	got := EstimateImageTokens(msgs)
	want := 1*170 + 85 // 255
	if got != want {
		t.Errorf("256×256 meta: want %d, got %d", want, got)
	}
}

func TestEstimateImageTokens_MultipleImages(t *testing.T) {
	msgs := []protocol.Message{
		{Role: "user", Parts: []any{
			imagePart(100_000),
			imagePart(100_000),
		}},
	}
	got := EstimateImageTokens(msgs)
	if got != 2*imageTokensFullHD {
		t.Errorf("two fullHD images: want %d, got %d", 2*imageTokensFullHD, got)
	}
}

func TestEstimateImageTokens_URLImage(t *testing.T) {
	part := map[string]any{
		"type":   "image",
		"source": map[string]any{"url": "https://example.com/img.jpg"},
	}
	msgs := []protocol.Message{{Role: "user", Parts: []any{part}}}
	got := EstimateImageTokens(msgs)
	if got != imageTokensFullHD {
		t.Errorf("URL image: want %d, got %d", imageTokensFullHD, got)
	}
}

// ─── SessionCompressor.ShouldTrigger ─────────────────────────────────────────

func TestCompressor_ShouldTrigger_Below(t *testing.T) {
	sc := NewSessionCompressor(500)
	if sc.ShouldTrigger(600, 1000) {
		t.Error("60% usage should not trigger (threshold=65%)")
	}
}

func TestCompressor_ShouldTrigger_AtThreshold(t *testing.T) {
	sc := NewSessionCompressor(500)
	if !sc.ShouldTrigger(650, 1000) {
		t.Error("65% usage should trigger")
	}
}

func TestCompressor_ShouldTrigger_Above(t *testing.T) {
	sc := NewSessionCompressor(500)
	if !sc.ShouldTrigger(900, 1000) {
		t.Error("90% usage should trigger")
	}
}

func TestCompressor_ShouldTrigger_ZeroMax(t *testing.T) {
	sc := NewSessionCompressor(500)
	if sc.ShouldTrigger(1000, 0) {
		t.Error("zero maxTokens should not trigger (division guard)")
	}
}

// ─── SessionCompressor.Compress ──────────────────────────────────────────────

func TestCompressor_Compress_NotTriggered(t *testing.T) {
	sc := NewSessionCompressor(500)
	msgs := []protocol.Message{{Role: "user", Content: "hello"}}
	_, triggered := sc.Compress(msgs, 100, 1000) // 10% < 65%
	if triggered {
		t.Error("10% usage should not trigger compression")
	}
}

func TestCompressor_Compress_PrunesLargeTool(t *testing.T) {
	sc := NewSessionCompressor(500)
	big := strings.Repeat("x", 20_000)
	msgs := []protocol.Message{
		{Role: "user", Parts: []any{toolResult("t1", big)}},
	}
	out, triggered := sc.Compress(msgs, 700, 1000) // 70% > 65%
	if !triggered {
		t.Fatal("70% should trigger compression")
	}
	p := out[0].Parts[0].(map[string]any)
	c, _ := p["content"].(string)
	if !strings.Contains(c, "pruned") {
		t.Errorf("large tool output should be pruned, got %q", c)
	}
}

func TestCompressor_Antithrash_BlocksSecondCall(t *testing.T) {
	sc := NewSessionCompressor(500)
	msgs := []protocol.Message{
		{Role: "user", Parts: []any{toolResult("t1", strings.Repeat("x", 20_000))}},
	}
	_, ok1 := sc.Compress(msgs, 700, 1000)
	if !ok1 {
		t.Fatal("first compress should succeed")
	}
	// 立即第二次调用 — 应被冷却期拦截
	_, ok2 := sc.Compress(msgs, 700, 1000)
	if ok2 {
		t.Error("second immediate compress should be blocked by anti-thrash cooldown")
	}
}

func TestCompressor_Antithrash_AllowsAfterCooldown(t *testing.T) {
	sc := NewSessionCompressor(500)
	// 伪造上次压缩时间已过冷却期
	sc.mu.Lock()
	sc.lastCompressAt = time.Now().Add(-antithrashCooldown - time.Second)
	sc.mu.Unlock()

	msgs := []protocol.Message{
		{Role: "user", Parts: []any{toolResult("t1", strings.Repeat("x", 20_000))}},
	}
	_, ok := sc.Compress(msgs, 700, 1000)
	if !ok {
		t.Error("after cooldown, compress should succeed")
	}
}

func TestCompressor_SetAnchor(t *testing.T) {
	sc := NewSessionCompressor(500)
	sc.SetAnchor("决策: 使用 SQLite WAL 模式")
	if sc.Anchor() != "决策: 使用 SQLite WAL 模式" {
		t.Error("anchor should be stored and retrieved correctly")
	}
}
