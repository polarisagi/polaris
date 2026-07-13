package chat

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/prompt"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

func (s *ChatHandler) InjectSystemPrompt(ctx context.Context, agentCtrl protocol.AgentController, history []types.Message, userQuery string) []types.Message { //nolint:gocyclo,nestif
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

	// 用户自定义追加指令（~/.polarisagi/polaris/config/prompts/custom_instructions.md）
	ic.CustomInstructions = s.PromptMgr.ReadPrompt("custom_instructions.md", "")

	// 用户画像（P0-2：消费 default 用户画像）
	if p, err := agentCtrl.Memory().GetUserProfile(ctx, "default"); err == nil && p != nil {
		var summary []string
		for _, sf := range p.StableFacts {
			summary = append(summary, "- "+fmt.Sprint(sf))
		}
		for _, bp := range p.BehavioralPatterns {
			summary = append(summary, "- "+fmt.Sprint(bp))
		}
		if len(summary) > 0 {
			ic.UserProfile = "## User Profile (Context)\n" + strings.Join(summary, "\n")
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

	// volatile 层：当前日期（精确到天，不破坏 prefix cache），会话信息由调用方追加
	ic.VolatileBlock = "当前日期：" + time.Now().Format("2006-01-02")

	// Built-in tools — 仅注入工具名列表；描述已由 function schema 传递，避免系统提示词冗余膨胀。
	if s.ToolReg != nil {
		var names []string
		for _, t := range s.ToolReg.List() {
			names = append(names, t.Name)
		}
		if len(names) > 0 {
			ic.BuiltinTools = fmt.Sprintf("%d: %s", len(names), strings.Join(names, ", "))
		}
	}

	// 扩展感知（插件 / MCP / App）— 仅名称 + 连接状态摘要，细节由 BuildToolSchemas() 注入 function schema。
	ic.InstalledPlugins = s.buildExtensionSummary(ctx)

	// Ambient skills 写入独立字段，不拼接进 SystemPromptTemplate。
	// 原因：skill instructions 可能含 {{ }} 语法（代码示例/Jinja/Handlebars），
	// 若拼入模板字符串会导致 template.Parse() 崩溃，系统提示词退化为报错文本。
	// PrependToMessages 在模板渲染完成后再追加 AmbientContext，彻底脱离模板解析器。
	if s.DB != nil {
		ic.AmbientContext = s.buildAmbientSkillsSection(ctx, userQuery)
	}

	return core.PrependToMessages(history)
}

const (
	maxFullTextChars   = 128_000 // 全文注入总预算（128K字符 ≈ 32K tokens）
	relevanceThreshold = 0.05    // 关键词词元重叠阈值（5%）
)

// Ambient skills 相关性判定/文本注入 (relevanceScore/skillTextKey/
// cachedSkillEmbed/isSkillRelevant/buildAmbientSkillsSection/
// SetActivatedSystemPrompt) 见 system_prompt_ambient.go；插件/MCP/App 感知
// 摘要 (buildExtensionSummary/queryPluginSummary/queryAppSummary/
// standaloneMCPSummary) 见 system_prompt_extensions.go（均为 R7 拆分）。

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
