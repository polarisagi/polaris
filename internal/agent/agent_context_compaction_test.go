package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// stubSummaryProvider 返回固定摘要文本的 Provider，供 Stage 2 硬触发路径测试。
type stubSummaryProvider struct {
	summary string
}

func (p *stubSummaryProvider) Infer(_ context.Context, _ []types.Message, _ ...types.InferOption) (*types.ProviderResponse, error) {
	return &types.ProviderResponse{Content: p.summary}, nil
}

func (p *stubSummaryProvider) StreamInfer(_ context.Context, _ []types.Message, _ ...types.InferOption) (<-chan types.StreamEvent, error) {
	ch := make(chan types.StreamEvent, 1)
	ch <- types.StreamEvent{Type: types.StreamTextDelta, Content: p.summary}
	close(ch)
	return ch, nil
}

func (p *stubSummaryProvider) Capabilities() types.ProviderCapabilities {
	return types.ProviderCapabilities{}
}
func (p *stubSummaryProvider) Tokenizer() protocol.TokenizerAdapter { return nil }
func (p *stubSummaryProvider) ModelID() string                      { return "stub-summary" }

func bigToolMsg(n int) types.Message {
	return types.Message{Role: "tool", Content: strings.Repeat("x", n)}
}

// TestHotPathCompactIfNeeded_BelowThreshold_NoOp 验证使用率低于 70% 时原样返回，
// 且不触碰 ContextWindowManager 之外的任何依赖（cwm 为默认 90000 容量）。
func TestHotPathCompactIfNeeded_BelowThreshold_NoOp(t *testing.T) {
	protocol.SetReplayMode(false)
	a := NewAgentWithDefaults("sess-cwm-noop")

	msgs := []types.Message{{Role: "user", Content: "hello"}}
	out := a.hotPathCompactIfNeeded(context.Background(), msgs)

	if len(out) != len(msgs) || out[0].Content != "hello" {
		t.Errorf("expected unchanged messages below threshold, got %+v", out)
	}
	if a.cwm.NeedsCompaction() != 0 {
		t.Errorf("expected NeedsCompaction()==0 for small usage")
	}
}

// TestHotPathCompactIfNeeded_SoftTrigger_OffloadOnlyNoLLMCall 验证 >70%（软触发）
// 只执行 Stage 1（大 tool_result 卸载），绝不调用 Provider——LLM 摘要成本只应
// 在真正逼近上限（硬触发）时才产生。
func TestHotPathCompactIfNeeded_SoftTrigger_OffloadOnlyNoLLMCall(t *testing.T) {
	protocol.SetReplayMode(false)
	a := NewAgentWithDefaults("sess-cwm-soft")
	a.InjectContextWindowManager(NewContextWindowManager(1000)) // 1000 token 容量，便于构造超阈值输入
	fp := &failIfCalledProvider{}
	a.provider = fp

	// 4000 chars / 4 = 1000 token ≈ 100% usage，构造成一条大 tool_result，
	// 落在 >70% 且 <=90% 区间需要精确控制；这里直接验证"触发了压缩"与
	// "Provider 未被调用"这两个不变量，不严格卡在 70%-90% 精确区间
	// （NeedsCompaction 阈值判定已由 TestContextWindowManager_NeedsCompaction 覆盖）。
	toolMsg := bigToolMsg(3200) // 3200/4=800 token = 80% of 1000 → 软触发区间
	msgs := []types.Message{
		{Role: "user", Content: "task"},
		toolMsg,
	}

	out := a.hotPathCompactIfNeeded(context.Background(), msgs)

	if fp.callCount() != 0 {
		t.Errorf("expected no Provider call on soft trigger, got %d calls", fp.callCount())
	}
	// Stage 1 offloader 未注入（nil）时应保留原始消息不变（无 VFS 依赖）。
	if len(out) != len(msgs) {
		t.Errorf("expected same message count with nil offloader, got %d", len(out))
	}
}

// TestHotPathCompactIfNeeded_HardTrigger_SummarizesAndShrinks 验证 >90%（硬触发）
// 在 Stage 1 基础上追加 Stage 2 LLM 摘要，产生的消息数应显著减少且包含摘要前缀。
func TestHotPathCompactIfNeeded_HardTrigger_SummarizesAndShrinks(t *testing.T) {
	protocol.SetReplayMode(false)
	a := NewAgentWithDefaults("sess-cwm-hard")
	a.InjectContextWindowManager(NewContextWindowManager(1000))
	a.provider = &stubSummaryProvider{summary: "会话要点摘要"}

	// 构造多条消息，总量远超 90% 阈值（1000 token = 4000 chars）。
	msgs := make([]types.Message, 0, 20)
	for i := 0; i < 20; i++ {
		msgs = append(msgs, types.Message{Role: "user", Content: strings.Repeat("y", 500)}) // 500 chars ≈ 125 token 每条
	}

	out := a.hotPathCompactIfNeeded(context.Background(), msgs)

	if len(out) >= len(msgs) {
		t.Fatalf("expected compaction to reduce message count, before=%d after=%d", len(msgs), len(out))
	}
	if !strings.Contains(out[0].Content, "会话要点摘要") {
		t.Errorf("expected first message to contain the LLM summary, got %q", out[0].Content)
	}
}

// TestHotPathCompactIfNeeded_ReplayMode_ShortCircuits 验证回放模式下物理短路，
// 不产生任何压缩副作用（与其余 3 处 IsReplaying 短路点同一语义）。
func TestHotPathCompactIfNeeded_ReplayMode_ShortCircuits(t *testing.T) {
	protocol.SetReplayMode(true)
	defer protocol.SetReplayMode(false)

	a := NewAgentWithDefaults("sess-cwm-replay")
	a.InjectContextWindowManager(NewContextWindowManager(1000))
	fp := &failIfCalledProvider{}
	a.provider = fp

	msgs := []types.Message{bigToolMsg(4000)}
	out := a.hotPathCompactIfNeeded(context.Background(), msgs)

	if len(out) != 1 || out[0].Content != msgs[0].Content {
		t.Errorf("expected unchanged messages during replay, got %+v", out)
	}
	if fp.callCount() != 0 {
		t.Errorf("expected no Provider call during replay, got %d", fp.callCount())
	}
}

// TestNewContextWindowManager_Defaults 验证默认容量与阈值。
func TestNewContextWindowManager_Defaults(t *testing.T) {
	cwm := NewContextWindowManager(0)
	if cwm.MaxTokens() != defaultContextWindowMaxTokens {
		t.Errorf("expected default maxTokens=%d, got %d", defaultContextWindowMaxTokens, cwm.MaxTokens())
	}
	cwm.SetCurrentUsage(0)
	if cwm.NeedsCompaction() != 0 {
		t.Errorf("expected 0 at zero usage")
	}
}
