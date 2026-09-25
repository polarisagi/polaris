package agent

import (
	"context"
	"errors"
	"log/slog"
	"sort"

	"github.com/polarisagi/polaris/internal/llm/safecall"
	"github.com/polarisagi/polaris/internal/memory/compact"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
	"github.com/polarisagi/polaris/pkg/util"
)

const (
	// overflowPruneRatio 溢出恢复把可修剪消息的总字节缩到原来的这一比例。
	overflowPruneRatio = 0.5
	// overflowPruneMinMessageBytes 单条消息修剪后的下限：低于它首尾片段已不足以
	// 保留污点围栏标记与可读内容。
	overflowPruneMinMessageBytes = 512
)

// streamInferWithOverflowRecovery 发起流式推理；Provider 以 ErrContextOverflow 拒绝时，
// 确定性修剪一次后重试（DeepSeek Harness 溢出恢复的做法：有界、单次、须证明请求确已
// 缩小才重试，否则如实上抛原错误）。
//
// 热路径压缩（hotPathCompactIfNeeded）按固定容量估算做事前预防，估算与实际 Provider
// 窗口不符时（小窗口本地模型、CJK 字节/token 比偏差）请求仍会被拒；此前该错误被路由
// 当作 Provider 故障逐个 failover 并计入熔断，最终报"所有 Provider 耗尽"，Agent 误计
// ProviderSuspendCount，从未尝试缩减请求。
//
// 修剪不调用 LLM 摘要：摘要会把 TaintHigh 围栏内的数据改写成 assistant 角色消息，
// 等于污点提权；首尾保留截断保持原角色与围栏标记。preTokenize 是 PII 令牌化之前的
// 消息，修剪后重新令牌化，保证令牌映射与发出的请求一致。返回实际发出的消息供事件落盘。
func (a *Agent) streamInferWithOverflowRecovery(ctx context.Context, preTokenize, reqMsgs []types.Message,
	opts []types.InferOption, audience protocol.LLMAudience) (*types.ProviderResponse, []types.Message, error) {
	resp, err := a.streamInferOnce(ctx, reqMsgs, opts, audience)
	if !errors.Is(err, protocol.ErrContextOverflow) {
		return resp, reqMsgs, err
	}
	pruned, ok := pruneForOverflow(preTokenize)
	if !ok {
		slog.WarnContext(ctx, "kernel: context overflow, nothing prunable outside pinned head",
			"agent_id", a.ID, "session", a.sCtx.SessionID, "tokens_est", compact.RoughTokens(preTokenize))
		return nil, reqMsgs, err
	}
	retryMsgs, tokErr := a.tokenizeMessagesForLLM(ctx, pruned)
	if tokErr != nil {
		return nil, reqMsgs, err
	}
	metrics.GlobalContextOverflowRecoveryTotal.Add(1)
	slog.WarnContext(ctx, "kernel: context overflow, retrying once with pruned request",
		"agent_id", a.ID, "session", a.sCtx.SessionID,
		"tokens_before", compact.RoughTokens(preTokenize), "tokens_after", compact.RoughTokens(pruned))
	resp, err = a.streamInferOnce(ctx, retryMsgs, opts, audience)
	return resp, retryMsgs, err
}

func (a *Agent) streamInferOnce(ctx context.Context, msgs []types.Message, opts []types.InferOption,
	audience protocol.LLMAudience) (*types.ProviderResponse, error) {
	ch, err := safecall.StreamInfer(ctx, a.provider, msgs, opts...)
	if err != nil {
		return nil, err //nolint:wrapcheck // 保持错误链原样，调用方以 errors.Is 判别 ErrContextOverflow
	}
	return a.doStreamInfer(ctx, ch, audience)
}

// pruneForOverflow 把开头连续 system 消息（固定前缀：内核指令、安全规约，同
// compact.SplitPinnedHead）之后的 system 以外消息按"注水"上限做首尾保留截断，使其
// 总字节约降到 overflowPruneRatio。最大的消息先被截，短消息（如用户本轮意图）原样保留。
// 返回 false 表示没有可修剪内容或无法取得进展。
func pruneForOverflow(msgs []types.Message) ([]types.Message, bool) {
	head, body := compact.SplitPinnedHead(msgs)
	var sizes []int
	total := 0
	for _, m := range body {
		if m.Role != "system" {
			sizes = append(sizes, len(m.Content))
			total += len(m.Content)
		}
	}
	limit := waterLevel(sizes, int(float64(total)*overflowPruneRatio))
	limit = max(limit, overflowPruneMinMessageBytes)

	out := make([]types.Message, 0, len(msgs))
	out = append(out, head...)
	changed := false
	for _, m := range body {
		if m.Role != "system" && len(m.Content) > limit {
			m.Content = util.ElideMiddle(m.Content, limit)
			changed = true
		}
		out = append(out, m)
	}
	return out, changed
}

// waterLevel 返回使 Σmin(size, level) ≤ target 的最大 level。
func waterLevel(sizes []int, target int) int {
	sorted := append([]int(nil), sizes...)
	sort.Ints(sorted)
	remaining := target
	for i, s := range sorted {
		n := len(sorted) - i
		if s*n > remaining {
			return remaining / n
		}
		remaining -= s
	}
	if len(sorted) == 0 {
		return 0
	}
	return sorted[len(sorted)-1]
}
