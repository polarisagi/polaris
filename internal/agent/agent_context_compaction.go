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
// （M04-Agent-Kernel.md §7；ADR-0060）：在每次 LLMFillEffect 组装完 reqMsgs、
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
	middle, tail := compact.SplitMessages(msgs, hotPathHardTailTokens)
	if len(middle) == 0 {
		// tail 已覆盖全部消息，无法进一步压缩（Stage 1 卸载结果已是最终结果）。
		return msgs
	}

	budget := compact.CalcSummaryBudget(middle, compact.DefaultSummaryRatio, compact.DefaultMinSummaryTokens, compact.DefaultMaxSummaryTokens)
	summary, err := compact.Summarize(ctx, middle, budget, a.provider)
	if err != nil {
		slog.Warn("agent: hot-path context compaction summarize failed, keeping Stage-1-only result",
			"agent_id", a.ID, "task_id", taskID, "err", err)
		return msgs
	}

	if a.memory != nil {
		summary = compact.InjectTaskCanvas(a.memory.RenderTaskCanvas(), summary)
	}

	summaryMsg := types.Message{
		Role:    "assistant",
		Content: compact.SummaryPrefix + "\n\n" + summary,
	}
	newMsgs := make([]types.Message, 0, 1+len(tail))
	newMsgs = append(newMsgs, summaryMsg)
	newMsgs = append(newMsgs, tail...)

	slog.Info("agent: hot-path context compaction (hard trigger)",
		"agent_id", a.ID, "task_id", taskID,
		"tokens_before", compact.RoughTokens(msgs), "tokens_after", compact.RoughTokens(newMsgs))

	return newMsgs
}
