package agent

import (
	"context"
	"log/slog"

	"github.com/polarisagi/polaris/internal/memory/compact"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// hotPathHardTailTokens 硬触发（>90%）Stage 2 压缩时保留的尾部原文 token 数。
// 软触发（>70%）不执行 Stage 2（见 hotPathCompact 注释），故只需一个尾部常量。
const hotPathHardTailTokens = 1024

// InjectContextWindowManager 覆盖默认（90000 token）的 M4 热路径上下文窗口
// 管理器，供需要非默认容量的场景使用（如按 Provider 上下文窗口差异配置）。
func (a *Agent) InjectContextWindowManager(cwm *ContextWindowManager) {
	if cwm != nil {
		a.cwm = cwm
	}
}

// InjectToolRefOffloader 注入 Stage 1 大 tool_result 卸载依赖（M05 §11.3），
// 与网关 Compressor 共用同一个 internal/memory.ToolRefOffloader 实例。
// nil 时 Stage 1 静默跳过，仅执行 Stage 2/3（与网关侧 nil-offloader 语义一致）。
func (a *Agent) InjectToolRefOffloader(off compact.Offloader) {
	a.toolOffloader = off
}

// hotPathCompactIfNeeded 是 M4 ContextWindowManager 热路径压缩的驱动入口
// （M04-Agent-Kernel.md §7；ADR-0033）：在每次 LLMFillEffect 组装完 reqMsgs、
// 发起真实推理前调用，更新 currentUsage 并按 >70%/>90% 阈值触发压缩。
//
// 2026-07-22 一致性审查修复背景：此前 ContextWindowManager 从未被构造、
// currentUsage 从未被赋值，M4 唯一的"预算保护"是 agent_execute_effect.go
// 里的 50/75/100% 三级检测——那三级检测操作的是*任务级累计 token 预算*
// （sCtx.TokensUsed/TokenBudget，决定是否收紧 DAG 规模/直接判任务失败），
// 与本函数操作的*单次 LLM 调用的 reqMsgs 实际大小*是两个不同维度：前者
// 防的是"整个任务话多轮消耗掉太多 token 预算"，后者防的是"单轮 S_EXECUTE
// 多轮工具调用导致这一次请求本身超出 Provider 上下文窗口"。二者互补，不
// 互相替代。
//
// 复用 internal/memory/compact 的 Stage1(大 tool_result 卸载)/Stage2(LLM 锚点
// 摘要)/Stage3(TaskMermaidCanvas 注入) 算法——与 M5/网关 SessionCompressor
// 共享同一套实现（见该包 doc 注释），不重复发明。软触发（>70%）只做 Stage 1
// （便宜、无需 LLM 调用）；硬触发（>90%）在 Stage 1 基础上追加 Stage 2/3（LLM
// 摘要有真实推理成本，只在真正逼近上限时才动用，避免每次越过 70% 线就触发
// 一次额外 LLM 调用，反而在任务已经吃紧时雪上加霜）。
//
// ReplayMode 下物理短路：回放期间禁止任何会改变消息内容/触发 LLM 调用的
// 副作用（与 recordLLMFillEffectMemory 等其余 3 处 IsReplaying 短路点同一语义）。
func (a *Agent) hotPathCompactIfNeeded(ctx context.Context, msgs []types.Message) []types.Message {
	if a.cwm == nil || protocol.IsReplaying() {
		return msgs
	}
	a.cwm.SetCurrentUsage(compact.RoughTokens(msgs))
	level := a.cwm.NeedsCompaction()
	if level == 0 {
		return msgs
	}
	return a.hotPathCompact(ctx, msgs, level)
}

// hotPathCompact 执行实际压缩。level=1 只做 Stage 1；level=2 追加 Stage 2/3。
// 任一阶段失败均保留原消息、不阻断推理流程（与网关 Compressor 的失败兜底策略
// 一致：压缩是尽力而为的优化，绝不能因为压缩失败而拖垮正常推理）。
func (a *Agent) hotPathCompact(ctx context.Context, msgs []types.Message, level int) []types.Message {
	taskID := a.memoryPartitionKey()

	// Stage 1：大 tool_result 卸载（offloader 为 nil 时静默跳过，语义与网关侧一致）。
	msgs = compact.OffloadLargeToolResults(ctx, taskID, msgs, a.toolOffloader)

	if level == 1 || a.provider == nil {
		return msgs
	}

	// Stage 2/3：仅硬触发（>90%）执行，需要真实 LLM 调用生成锚点摘要。
	//
	// 固定前缀 = L0..L2（最后一条 CacheBreakpoint 之前）∪ 开头连续 system：内核前缀账本的缓存前缀
	// 由决策二的分块跳窗独占管理，压缩只能作用于其后的回合内容（L3/L4）。此前只固定开头连续 system，
	// L2 历史被卷进摘要——每个请求都让 LLM 重写一遍（非确定）、缓存前缀逐请求改变，且历史被降级成
	// assistant 摘要；同时 L3 阶段选择器（system）也被卷进摘要，模型看不到当前阶段。
	head, body := msgs[:pinnedPrefixLen(msgs)], msgs[pinnedPrefixLen(msgs):]
	middle, tail := compact.SplitMessages(body, hotPathHardTailTokens)
	if len(middle) > 0 && len(tail) == 0 {
		// 最后一条（本轮意图/观测）本身就超过尾部预算：不得把它卷进摘要——那等于用摘要替换用户本轮的请求。
		tail, middle = middle[len(middle)-1:], middle[:len(middle)-1]
	}
	// L3 的 system 消息（阶段选择器、易变层、压力提示、工具目录）原样保留：摘要会把指令降级成
	// assistant 文本，且选择器丢失后模型不知道当前该按哪份阶段契约输出。
	var keep, squash []types.Message
	for _, m := range middle {
		if m.Role == "system" {
			keep = append(keep, m)
		} else {
			squash = append(squash, m)
		}
	}
	if len(squash) == 0 {
		// 可压缩内容为空（Stage 1 卸载结果已是最终结果）。
		return msgs
	}

	budget := compact.CalcSummaryBudget(squash, compact.DefaultSummaryRatio, compact.DefaultMinSummaryTokens, compact.DefaultMaxSummaryTokens)
	summary, err := compact.Summarize(ctx, squash, budget, a.provider)
	if err != nil {
		slog.Warn("agent: hot-path context compaction summarize failed, keeping Stage-1-only result",
			"agent_id", a.ID, "task_id", taskID, "err", err)
		return msgs
	}

	if a.memory != nil {
		summary = compact.InjectTaskCanvas(a.memory.RenderTaskCanvas(ctx, a.sCtx.SessionID), summary)
	}

	summaryMsg := types.Message{
		Role:    "assistant",
		Content: compact.SummaryPrefix + "\n\n" + summary,
	}
	newMsgs := make([]types.Message, 0, len(head)+len(keep)+1+len(tail))
	newMsgs = append(newMsgs, head...) // 固定前缀字节级不变（GD-14-001）
	newMsgs = append(newMsgs, keep...)
	newMsgs = append(newMsgs, summaryMsg)
	newMsgs = append(newMsgs, tail...)

	slog.Info("agent: hot-path context compaction (hard trigger)",
		"agent_id", a.ID, "task_id", taskID,
		"tokens_before", compact.RoughTokens(msgs), "tokens_after", compact.RoughTokens(newMsgs))

	return newMsgs
}

// cachedPrefixLen 返回 L0..L2 缓存前缀的消息数：最后一条带 CacheBreakpoint 的消息（含）之前。
// prompt 组装方（PromptBuilder.BuildLayered）只在 L0 内部边界与 L0..L2 共享前缀末置位，
// 所以"最后一个断点"就是 L2 末（L2 为空时为 L1 末）。无任何标记（非内核前缀账本的请求）返回 0。
func cachedPrefixLen(msgs []types.Message) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].CacheBreakpoint {
			return i + 1
		}
	}
	return 0
}

// pinnedPrefixLen 是压缩/修剪绝不改写的前缀长度：缓存前缀与开头连续 system（GD-14-001）取较长者。
func pinnedPrefixLen(msgs []types.Message) int {
	head, _ := compact.SplitPinnedHead(msgs)
	return max(len(head), cachedPrefixLen(msgs))
}
