package fsm

import (
	"context"
	"log/slog"
	"strings"

	"github.com/polarisagi/polaris/configs"
	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/prompt"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// 五层前缀账本的公共写入器（ADR-0105 决策一）。四个阶段（Perceive/Plan/Reflect/Respond）
// × 两条路径（记忆路径 agent/context、降级路径 fsm）全部经由这里写 L1/L2/L0，
// 才能保证同一回合各阶段的 L0..L2 字节一致——任何一个阶段私自多写/少写/换序都会
// 让其后的全部内容失配。

// WriteSessionLayer 写入 L1 会话层：系统环境快照、核心记忆、子 Agent 画像、可信工作区
// 指令、扩展目录、不可信工作区上下文。写入顺序固定，与阶段无关。
//
// 信任分区不变：核心记忆走 ZoneCoreMemory（TaintHigh 块自带围栏）；子 Agent 画像走
// ZoneMutableSkill；仅 WorkspaceContextLoader 判定 Trusted 的内容进入指令区（GD-14-005）；
// 第三方扩展自述与默认通道的工作区文档一律 TaintHigh 进 ZoneExternalCatalog（S-02）。
// memory 为 nil（降级路径 / 测试）时跳过核心记忆。
func WriteSessionLayer(ctx context.Context, b *prompt.PromptBuilder, memory protocol.MemoryFacade, sCtx *StateContext) error {
	sCtx.Mu.RLock()
	sysEnv := sCtx.SysEnvSnapshot
	trusted := sCtx.WorkspaceContextTrusted
	untrusted := sCtx.WorkspaceContextUntrusted
	extInfo := sCtx.InstalledExtensionsInfo
	prefs := sCtx.SessionPreferencesBlock
	sCtx.Mu.RUnlock()

	if sysEnv != "" {
		b.WriteSystemEnvironment(sysEnv)
	}

	// [UP-03] 核心工作记忆：LLM 经 core_memory_edit 显式维护的任务核心状态，各阶段均需可见；
	// 读取失败按无核心记忆降级，不阻断组装。
	if memory != nil {
		if blocks, err := memory.ListCoreMemory(ctx, sCtx.AgentID, sCtx.SessionID); err == nil && len(blocks) > 0 {
			b.WriteCoreMemory(blocks)
		}
	}
	if prefs != "" {
		safe, err := taint.SanitizeToSafe(taint.NewTaintedString(
			prefs, taint.TaintSource{Module: "persona", OriginTaintLevel: types.TaintNone}, "session_preferences"))
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "WriteSessionLayer: sanitize session preferences", err)
		}
		b.SetLayer(protocol.LayerSession)
		b.WriteInstruction(safe)
		b.ResetLayer()
	}
	WriteAgentProfile(b, sCtx)

	// GD-14-005：用户显式声明信任的工作区约束文档，作为项目级系统指令。默认路径下本字段
	// 恒为空，AGENTS.md 走下方不可信通道。指令写入方法的默认层是 L3，这里显式落 L1。
	if trusted != "" {
		safe, err := taint.SanitizeToSafe(taint.NewTaintedString(
			trusted,
			taint.TaintSource{Module: "workspace", OriginTaintLevel: types.TaintNone},
			"workspace_context_trusted"))
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "WriteSessionLayer: sanitize trusted workspace context", err)
		}
		b.SetLayer(protocol.LayerSession)
		b.WriteInstruction(safe)
		b.ResetLayer()
	}

	// S-02：已安装扩展的自述信息来源不可信（第三方可控），TaintHigh 围栏。
	if extInfo != "" {
		b.WriteExternalCatalog("extensions", taint.NewTaintedString(
			extInfo,
			taint.TaintSource{Module: "extension", OriginTaintLevel: types.TaintHigh},
			"extension_catalog"))
	}

	// GD-14-005：工作区上下文默认通道。AGENTS.md/CLAUDE.md 在 clone 来的仓库中完全是攻击者
	// 可控的，威胁模型与第三方扩展自述一致，只进 ZoneExternalCatalog 并按 TaintHigh 围栏。
	if untrusted != "" {
		b.WriteExternalCatalog("workspace_context", taint.NewTaintedString(
			untrusted,
			taint.TaintSource{Module: "workspace", OriginTaintLevel: types.TaintHigh},
			"workspace_context"))
	}
	return nil
}

// WritePlanHints 把状态机刷新的 Plan 运行期提示块写入 L3（见 StateContext.PlanHintBlocks）。
func WritePlanHints(b *prompt.PromptBuilder, sCtx *StateContext) {
	sCtx.Mu.RLock()
	blocks := sCtx.PlanHintBlocks
	sCtx.Mu.RUnlock()
	for _, blk := range blocks {
		b.WritePhaseSystem(blk)
	}
}

// FinishLayered 补齐 L0 稳定核与 L3 易变层并按五层输出。
//
// 稳定核 = ImmutableCore 稳定层（契约段 + 可变稳定核两条 system，见 StableMessagesWithContracts）；易变层（日期/扩展连接状态/AmbientContext）放进 L3 末尾——
// 此前它紧贴稳定层成为第 2 条消息，每次问题变化都使其后全部内容失配。
// memory 为 nil 时无 L0/易变层（降级路径无 ImmutableCore）。
func FinishLayered(b *prompt.PromptBuilder, memory protocol.MemoryFacade) []types.Message {
	if memory != nil {
		if core := memory.ImmutableCore(); core != nil {
			if pc := phaseContractCoreOf(memory); pc != nil {
				for _, m := range pc.StableMessagesWithContracts() {
					b.WriteStable(m)
				}
			} else {
				b.WriteStable(core.StableMessage())
			}
			b.WritePhaseSystem(core.VolatileContent())
		}
	}
	return b.BuildLayered()
}

// phaseContractCore 是 ImmutableCore 的可选能力：渲染含阶段契约段的 L0（ADR-0105 决策九）。
// 用可选接口而非扩充 protocol.ImmutableCore，是因为只有内核前缀账本需要它，
// 网关直连等非内核调用方刻意只拿到不含契约段的 StableMessage。
type phaseContractCore interface {
	HasPhaseContracts() bool
	StableMessagesWithContracts() []types.Message
}

// phaseContractCoreOf 仅当开关开启、存在 ImmutableCore 且其能渲染完整契约库时返回非 nil。
// FinishLayered（决定 L0 是否含契约段）与 WritePhaseContract（决定 L3 写选择器还是全文模板）
// 共用这一个判据，保证二者永不脱节：L0 没有契约而 L3 只有选择器会让模型看不到输出格式。
func phaseContractCoreOf(memory protocol.MemoryFacade) phaseContractCore {
	if memory == nil || !config.CurrentThresholds().M4Kernel.PromptPhaseContractsInCore {
		return nil
	}
	core := memory.ImmutableCore()
	if core == nil {
		return nil
	}
	pc, ok := core.(phaseContractCore)
	if !ok || !pc.HasPhaseContracts() {
		return nil
	}
	return pc
}

// PhaseContractsInCore 报告本次 prompt 组装是否把阶段契约放进了 L0。
func PhaseContractsInCore(memory protocol.MemoryFacade) bool {
	return phaseContractCoreOf(memory) != nil
}

// WritePhaseContract 写入阶段 L3 契约位：契约已在 L0 时只写一条简短选择器
// （configs.PhaseSelector），否则回退写完整模板（开关关闭/降级路径无 ImmutableCore，
// 行为与并入前完全一致）。name 为 kernel/<phase>.md。
func WritePhaseContract(b *prompt.PromptBuilder, memory protocol.MemoryFacade, name, fallback string) {
	if PhaseContractsInCore(memory) {
		if phase, ok := configs.PhaseOfTemplate(name); ok {
			safe, _ := taint.SanitizeToSafe(taint.NewTaintedString(
				configs.PhaseSelector(phase), taint.TaintSource{OriginTaintLevel: types.TaintNone}, "system_prompt"))
			b.WriteInstruction(safe)
			return
		}
	}
	WriteKernelInstruction(b, name, fallback)
}

// WriteAgenticUsage 写入 T7 计量行（ADR-0115 / WP2）。
func WriteAgenticUsage(b *prompt.PromptBuilder, tokens int, cost float64, wallSeconds float64) {
	b.WritePhaseSystem(formatUsageLine(tokens, 0, 0, 0, cost, cost, wallSeconds))
}

// FinishAgenticLayered 补齐 L0 稳定核，按 L0..L4 组装前缀，并在 L4 回合消息之后追加调用级易变尾巴（R3-01）。
//
// L0 = 可变稳定核（StableMessage）+ 循环指令 loopInstr（kernel/loop.md）。loop.md 是静态文本，
// 此前位于历史之后的 L3，每个新回合都随历史后移而落到前缀之外（ADR-0120 决策二第 4 条）。
// L0 **不带** "# PHASE CONTRACTS" 阶段契约段：契约是 Perceive/Plan/Reflect/Respond 四阶段的
// 输出格式，agentic 循环没有阶段，带上只会误导模型并白付约 2.5K token（2026-10-20 实测）。
//
// 调用级尾巴只含 T7 计量行与收尾告警（每次调用都变，必须在最末、且不写入 LoopMessages）。
// 日期、扩展状态、ambient 技能等回合级易变信息由回合首条 user 消息携带（Agent.turnOpeningContentLocked），
// 回合内位置固定，第二次调用起即可命中缓存（ADR-0120 决策二第 5 条）。
func FinishAgenticLayered(b *prompt.PromptBuilder, memory protocol.MemoryFacade, loopInstr *types.Message, usageText, warnPrompt string) []types.Message {
	if memory != nil {
		if core := memory.ImmutableCore(); core != nil {
			b.WriteStable(core.StableMessage())
		}
	}
	if loopInstr != nil {
		b.WriteStable(*loopInstr)
	}
	msgs := b.BuildLayered()

	var tail []string
	if usageText != "" {
		tail = append(tail, usageText)
	}
	if warnPrompt != "" {
		tail = append(tail, warnPrompt)
	}
	if len(tail) > 0 {
		msgs = append(msgs, types.Message{
			Role:    "user",
			Content: strings.Join(tail, "\n\n"),
		})
	}
	return msgs
}

// loopInstruction 返回循环模式指令（kernel/loop.md）的 L0 system 消息。
func loopInstruction() *types.Message {
	tmpl, err := configs.LoadPromptTemplate("kernel/loop.md", nil)
	if err != nil || strings.TrimSpace(tmpl) == "" {
		tmpl = "Loop mode: the turn ends when you answer without a tool call, or when you call finish with a summary."
	}
	safe, _ := taint.SanitizeToSafe(taint.NewTaintedString(
		tmpl, taint.TaintSource{OriginTaintLevel: types.TaintNone}, "system_prompt"))
	m := safe.IntoMessage("system")
	return &m
}

// writeFrozenSessionLayer 写入会话层（L1）。会话内首次调用时完整渲染并冻结到 sCtx，之后原样重放
// （ADR-0120 决策二第 3 条）。渲染失败不冻结，下次调用重试。
func writeFrozenSessionLayer(ctx context.Context, b *prompt.PromptBuilder, memory protocol.MemoryFacade, sCtx *StateContext) error {
	sCtx.Mu.RLock()
	snap := sCtx.SessionLayerSnapshot
	sCtx.Mu.RUnlock()
	if snap == nil {
		tmp := prompt.NewPromptBuilder()
		if err := WriteSessionLayer(ctx, tmp, memory, sCtx); err != nil {
			return err
		}
		snap = tmp.LayerMessages(protocol.LayerSession)
		if snap == nil {
			snap = []types.Message{}
		}
		sCtx.Mu.Lock()
		sCtx.SessionLayerSnapshot = snap
		sCtx.Mu.Unlock()
	}
	b.WriteSessionSnapshot(snap)
	return nil
}

// BuildAgenticPrompt 组装 Agentic 循环单轮请求的提示词：
// 复用 BuildLayered 的稳定层 (L0)、会话层 (L1)、历史层 (L2)、阶段层 (L3) 和回合层 (L4)，
// 并将易变信息作为单条 user 消息追加在 L4 之后（R3-01）。
func BuildAgenticPrompt(ctx context.Context, memory protocol.MemoryFacade, sCtx *StateContext, roundMsgs []types.Message, usageText string) ([]types.Message, error) {
	b := prompt.NewPromptBuilder()

	// 1. L1 会话层
	if err := writeFrozenSessionLayer(ctx, b, memory, sCtx); err != nil {
		slog.Warn("prompt: agentic session layer write failed", "err", err)
	}

	// 2. L2 历史层
	WriteConversationHistory(b, sCtx)

	// 4. L4 回合层：如果 roundMsgs 为空，写入初始 RawIntentTS；否则逐条写入 roundMsgs
	if len(roundMsgs) == 0 {
		sCtx.Mu.RLock()
		intent := sCtx.RawIntentTS
		sCtx.Mu.RUnlock()
		if !intent.IsEmpty() {
			b.WriteUserData(intent)
		}
	} else {
		for _, m := range roundMsgs {
			b.WriteTurnMessage(m)
		}
	}

	sCtx.Mu.RLock()
	warnPrompt := sCtx.LoopWarningPrompt
	sCtx.Mu.RUnlock()

	return FinishAgenticLayered(b, memory, loopInstruction(), usageText, warnPrompt), nil
}
