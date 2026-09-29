package fsm

import (
	"context"

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
// 稳定核 = ImmutableCore 稳定层；易变层（日期/扩展连接状态/AmbientContext）放进 L3 末尾——
// 此前它紧贴稳定层成为第 2 条消息，每次问题变化都使其后全部内容失配。
// memory 为 nil 时无 L0/易变层（降级路径无 ImmutableCore）。
func FinishLayered(b *prompt.PromptBuilder, memory protocol.MemoryFacade) []types.Message {
	if memory != nil {
		if core := memory.ImmutableCore(); core != nil {
			b.WriteStable(core.StableMessage())
			b.WritePhaseSystem(core.VolatileContent())
		}
	}
	return b.BuildLayered()
}
