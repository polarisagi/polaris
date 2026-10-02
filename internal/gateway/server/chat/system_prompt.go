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

	// 用户显式偏好画像（PersonaRefiner，M05 §2.3）：与上方 Stage3.5 UserProfile
	// 互补——UserProfile 是从 Episodic 事件自动合成的行为事实，PersonaRefiner 是
	// 结构化偏好维度（language_pref/response_style/output_format/expertise）+
	// 会话结束 LLM 摘要，二者数据来源与更新频率不同，分别写入 ImmutableCore 不同
	// 字段（2026-07-13 deadcode 复核：ToUserPreferences 此前构造了返回值但从未
	// 被任何调用方使用，ic.UserPreferences 也从未在 renderSystemPrompt 中渲染，
	// 见 working_mem.go §5.7 补齐）。
	if s.PersonaRefiner != nil {
		if ic.UserPreferences == nil {
			ic.UserPreferences = make(map[string]string)
		}
		for _, p := range s.PersonaRefiner.ToUserPreferences() {
			ic.UserPreferences[p.Dimension] = p.PreferenceText
		}
	}

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
		ic.ModelGuidance = s.PromptMgr.ModelSpecificGuidance(modelID)
		if ic.ModelGuidance == "" {
			// 通用工具调用强制引导（兜底）
			ic.ModelGuidance = "有工具可用时必须立即调用，禁止仅输出执行计划或说明性描述。"
		}
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
	if s.ToolReg != nil {
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
	var opDirectives []string

	if op := pm.ReadPrompt("operational/tool_use.md", ""); op != "" {
		opDirectives = append(opDirectives, op)
	}
	if op := pm.ReadPrompt("operational/task_completion.md", ""); op != "" {
		opDirectives = append(opDirectives, op)
	}
	if op := pm.ReadPrompt("operational/execution_discipline.md", ""); op != "" {
		opDirectives = append(opDirectives, op)
	}
	if op := pm.ReadPrompt("operational/memory_hygiene.md", ""); op != "" {
		opDirectives = append(opDirectives, op)
	}
	if op := pm.ReadPrompt("operational/coding_style.md", ""); op != "" {
		opDirectives = append(opDirectives, op)
	}
	if op := pm.ReadPrompt("operational/output_efficiency.md", ""); op != "" {
		opDirectives = append(opDirectives, op)
	}
	if op := pm.ReadPrompt("operational/risky_actions.md", ""); op != "" {
		opDirectives = append(opDirectives, op)
	}

	if len(opDirectives) > 0 {
		return strings.Join(opDirectives, "\n\n")
	}
	return ""
}
