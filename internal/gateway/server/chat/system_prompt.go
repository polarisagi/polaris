package chat

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/prompt"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

func (s *PromptAssemblyService) InjectSystemPrompt(ctx context.Context, agentCtrl protocol.AgentController, history []types.Message, userQuery string) []types.Message { //nolint:gocyclo,nestif
	if agentCtrl == nil || agentCtrl.Memory() == nil {
		return history
	}

	core := agentCtrl.Memory().ImmutableCore()
	if core == nil {
		return history
	}
	ic := core.Fields()

	// ── stable 层：身份 / 用户自定义指令 / 模型引导 / 平台提示 ────────────

	// 用户身份（三层优先级已在 LoadSoulMD 中处理，此处注入结果）。
	// 2026-07-08 加固：s.SoulMDContent 是 *string 硬依赖，生产唯一装配点
	// （server_lifecycle.go NewServer）恒传 &s.soulMDContent 不为 nil；
	// 但 ChatHandler 也可被测试/未来调用方以零值 Dependencies{} 构造，
	// 裸解引用会 panic（HTTP 路径有 withMiddleware 兜底但仍属不必要的
	// 500），改为 nil-safe 判空，保持与本文件其余可选字段同一防御风格。
	if s.SoulMDContent != nil {
		ic.SoulMDContent = *s.SoulMDContent
	}

	// 用户自定义追加指令（~/.polaris/config/prompts/custom_instructions.md）
	ic.CustomInstructions = s.PromptMgr.ReadPrompt("custom_instructions.md", "")

	// 用户画像（P0-2：消费 default 用户画像）
	if p, err := agentCtrl.Memory().GetUserProfile(ctx, "default"); err == nil && p != nil {
		// StableFacts/BehavioralPatterns 都是 map：直接 range 顺序随机，画像内容不变时
		// 稳定层也逐请求漂移（ADR-0105 决策一字节稳定规则 1），故按键排序后输出值。
		summary := sortedValueLines(p.StableFacts)
		summary = append(summary, sortedValueLines(p.BehavioralPatterns)...)
		if len(summary) > 0 {
			ic.UserProfile = "## User Profile (Context)\n" + strings.Join(summary, "\n")
		} else {
			ic.UserProfile = ""
		}
	}

	// 用户画像稳定维度不再写入共享的 ic.UserPreferences：它由内核在会话层首次渲染时快照
	// （ADR-0120 决策二）。此前每请求把含 interaction_summary 的画像写进稳定层，且 ic 跨会话
	// 共享，每回合都让稳定层字节变化、其后全部历史失去前缀缓存。
	// M9 激活的系统提示词优先覆盖（general taskType）
	// 三层组装时 SystemPromptTemplate 非空则全量走模板渲染，跳过 stable 层组装
	s.ActivatedSystemPromptMu.RLock()
	activatedPrompt := s.ActivatedSystemPrompt
	s.ActivatedSystemPromptMu.RUnlock()
	// 每轮重置为基础模板，防止 ambient 内容跨请求累积。
	// M9 激活提示词（activatedPrompt != ""）优先覆盖基础模板。
	if activatedPrompt != "" {
		ic.SystemPromptTemplate = activatedPrompt
	} else {
		ic.SystemPromptTemplate = s.BaseSystemPromptTpl
	}

	// 当前 Provider ModelID → 模型感知工具调用引导
	modelID := ""
	if p := s.Registry.PickProvider("default"); p != nil {
		modelID = p.ModelID()
	} else if p := s.Registry.PickProvider("general"); p != nil {
		modelID = p.ModelID()
	}
	ic.ModelID = modelID

	// 模型感知工具调用引导：模板模式（{{.ModelGuidance}}）和三层模式均需注入，移除旧的 "" 守卫。
	if prompt.NeedsToolUseEnforcement(modelID) {
		// 不再兜底注入"必须立即调用工具"：同义规则已在 operational/directives.md（ADR-0120 决策五：去重）。
		ic.ModelGuidance = s.PromptMgr.ModelSpecificGuidance(modelID)
	} else {
		ic.ModelGuidance = ""
	}

	ic.OperationalDirectives = loadOperationalDirectives(s.PromptMgr)

	// 平台感知提示
	ic.PlatformHint = s.PromptMgr.PlatformHintFor(s.ServerPlatform)

	// Built-in tools — 仅注入工具名列表；描述已由 function schema 传递，避免系统提示词冗余膨胀。
	// 名称显式排序：不依赖 ToolReg.List() 的实现是否有序（ToolRegistry 是接口，其他实现
	// 可能遍历 map）；稳定层任何集合都必须确定序（ADR-0105 决策一字节稳定规则 1）。
	// 无工具时必须清空：ic 是长生命周期共享对象，不清空会残留上一请求的工具名。
	ic.BuiltinTools = ""
	// 懒加载模式（ADR-0119）：核心工具名 + 未加载工具的一行索引，替代平铺全部工具名；索引字节只随工具集变化。
	if s.ToolIndex != nil {
		ic.BuiltinTools = s.ToolIndex.ToolsHint(ctx)
	}
	if ic.BuiltinTools == "" && s.ToolReg != nil {
		var names []string
		for _, t := range s.ToolReg.List() {
			names = append(names, t.Name)
		}
		sort.Strings(names)
		if len(names) > 0 {
			ic.BuiltinTools = fmt.Sprintf("%d: %s", len(names), strings.Join(names, ", "))
		}
	}

	// 扩展感知（插件 / MCP / App）：稳定层只放名称清单（确定序），连接状态 ✓/~/✗ 随
	// MCP 连接/断开变化，属易变量，并入下方易变层（ADR-0105 决策一字节稳定规则 2）。
	// 细节由 BuildToolSchemas() 注入 function schema。
	extSnap := s.snapshotExtensions(ctx)
	ic.InstalledPlugins = extSnap.extensionNames()

	// volatile 层（L3）：当前日期（精确到天）+ 扩展连接状态；会话信息由调用方追加。
	// 内核路径把它放在历史之后，网关直连路径放在最后一条消息之前，均不打断 L0..L2 前缀。
	ic.VolatileBlock = "当前日期：" + time.Now().Format("2006-01-02")
	if status := extSnap.extensionStatus(); status != "" {
		ic.VolatileBlock += "\n" + status
	}

	// Ambient skills 写入独立字段，不拼接进 SystemPromptTemplate。
	// 原因：skill instructions 可能含 {{ }} 语法（代码示例/Jinja/Handlebars），
	// 若拼入模板字符串会导致 template.Parse() 崩溃，系统提示词退化为报错文本。
	// PrependToMessages 在模板渲染完成后再追加 AmbientContext，彻底脱离模板解析器。
	// 与 BuiltinTools 同理，无 DB 时也要清空，避免残留上一请求的技能全文。
	ic.AmbientContext = ""
	if s.DB != nil {
		ic.AmbientContext = s.buildAmbientSkillsSection(ctx, userQuery)
	}

	return core.PrependToMessages(history)
}

const (
	// defaultAmbientMaxChars ambient skill 全文注入总预算的兜底值（字符）。
	// 权威值来自 spec/state.yaml §thresholds.m13_interface.ambient_skill_max_chars
	// → cfg.Thresholds.M13Interface.AmbientSkillMaxChars → ChatHandler.AmbientMaxChars；
	// 此常量仅在未注入（如单元测试直接构造 ChatHandler）时生效。
	//
	// 为何不是原先硬编码的 128_000：128K 字符 ≈ 32K tokens，在 Tier-0（2GB VPS，
	// 小上下文窗口模型）上单靠 ambient skill 就能打爆整个 prompt 预算，与 M13-bis §3
	// "不得占用超过 ~10% 上下文窗口"的设计约束相差 32 倍（DR-4-005）。
	defaultAmbientMaxChars = 4000
	relevanceThreshold     = 0.05 // 关键词词元重叠阈值（5%）
)

// Ambient skills 相关性判定/文本注入 (relevanceScore/skillTextKey/
// cachedSkillEmbed/isSkillRelevant/buildAmbientSkillsSection/
// SetActivatedSystemPrompt) 见 system_prompt_ambient.go；插件/MCP 感知
// 摘要 (snapshotExtensions/extensionNames/extensionStatus/queryPluginEntries/
// standaloneMCPEntries) 见 system_prompt_extensions.go（均为 R7 拆分）。

// sortedValueLines 按键升序把 map 的值渲染为 "- 值" 行（确定序）。
func sortedValueLines(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		lines = append(lines, "- "+fmt.Sprint(m[k]))
	}
	return lines
}

func loadOperationalDirectives(pm PromptManager) string {
	// 七份运营指令已合并去重为 operational/directives.md（ADR-0119：约 2k → 约 0.9k token/轮）。
	return pm.ReadPrompt("operational/directives.md", "")
}
