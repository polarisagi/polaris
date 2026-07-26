package store

// ImmutableCore.Load / renderSystemPrompt 系列 / PrependToMessages 从 working_mem.go
// 拆出（R7 文件行数治理，2026-07-26）：系统提示词组装/渲染是与 ContextWindow/
// ScratchPad 管理正交的独立职责，物理迁移不改变任何逻辑。

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"text/template"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

func (ic *ImmutableCore) Load(ctx context.Context, userID, sessionID string) (types.ImmutableCoreView, error) {
	var prefs []types.UserPreference //nolint:prealloc
	for k, v := range ic.UserPreferences {
		prefs = append(prefs, types.UserPreference{
			Dimension:      k,
			PreferenceText: v,
			Confidence:     1.0,
		})
	}
	return types.ImmutableCoreView{
		SessionGoal: ic.GlobalGoal,
		UserPrefs:   prefs,
	}, nil
}

func (ic *ImmutableCore) renderSystemPrompt() string {
	// M9 / 用户自定义模板：全量委托给模板渲染，跳过三层组装
	if ic.SystemPromptTemplate != "" {
		return ic.renderSystemPromptFromTemplate()
	}

	// 三层组装：stable → model guidance → platform hint → volatile
	var parts []string

	// 1. stable — 身份（SoulMDContent 已由 server 按三层优先级填充）
	if ic.SoulMDContent != "" {
		parts = append(parts, ic.SoulMDContent)
	} else {
		// server 未注入时的最终兜底（不应触发，仅防御性保护）
		parts = append(parts, protocol.DefaultPolarisIdentityFallback)
	}

	// 2. stable — 模型专属工具调用引导
	if ic.ModelGuidance != "" {
		parts = append(parts, ic.ModelGuidance)
	}

	// 3. stable — 用户自定义追加指令（追加而非覆盖，保留产品基线行为）
	if ic.CustomInstructions != "" {
		parts = append(parts, ic.CustomInstructions)
	}

	// 4. stable — 平台感知提示
	if ic.PlatformHint != "" {
		parts = append(parts, ic.PlatformHint)
	}

	// 5. stable — 工具/扩展感知摘要（仅名称，细节由 function schema 传递）
	if toolHint := ic.renderToolHint(); toolHint != "" {
		parts = append(parts, toolHint)
	}

	// 5.5 stable — 用户画像 (L3 摘要)
	if ic.UserProfile != "" {
		parts = append(parts, ic.UserProfile)
	}

	// 5.6 stable — 操作指令 (Memory Hygiene 等)
	if ic.OperationalDirectives != "" {
		parts = append(parts, ic.OperationalDirectives)
	}

	// 5.7 stable — 用户显式偏好画像（PersonaRefiner，M05 §2.3；与 5.5 UserProfile
	// 互补，见 chat/system_prompt.go 写入侧注释）。map 迭代顺序不确定，排序后拼接
	// 保证同一画像状态下渲染结果确定，避免打乱 LLM provider 的 prompt prefix cache。
	if prefsBlock := ic.renderUserPreferencesBlock(); prefsBlock != "" {
		parts = append(parts, prefsBlock)
	}

	// 6. volatile — 时间戳 / 会话信息（精确到天，不破坏 prefix cache）
	if ic.VolatileBlock != "" {
		parts = append(parts, ic.VolatileBlock)
	}

	return strings.Join(parts, "\n\n")
}

// renderSystemPromptFromTemplate 委托给用户自定义 Go template 渲染系统提示词
// （从 renderSystemPrompt 拆出，gocyclo 治理，行为不变）。
func (ic *ImmutableCore) renderSystemPromptFromTemplate() string {
	t, err := template.New("sys").Parse(ic.SystemPromptTemplate)
	if err != nil {
		return "Error parsing system prompt: " + err.Error() + "\n"
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, ic); err != nil {
		return "Error rendering system prompt: " + err.Error() + "\n"
	}
	return buf.String()
}

// renderToolHint 渲染工具/扩展感知摘要（仅名称，细节由 function schema 传递）；
// 两者均为空时返回空字符串（从 renderSystemPrompt 拆出，gocyclo 治理，行为不变）。
func (ic *ImmutableCore) renderToolHint() string {
	if ic.BuiltinTools == "" && ic.InstalledPlugins == "" {
		return ""
	}
	var toolParts []string
	if ic.BuiltinTools != "" {
		toolParts = append(toolParts, "Built-in tools: "+ic.BuiltinTools)
	}
	if ic.InstalledPlugins != "" {
		toolParts = append(toolParts, "Extensions: "+ic.InstalledPlugins)
	}
	return "You have tools callable via the function-call API.\n" + strings.Join(toolParts, "\n")
}

// renderUserPreferencesBlock 渲染用户显式偏好画像（PersonaRefiner，M05 §2.3）；
// map 迭代顺序不确定，排序后拼接保证同一画像状态下渲染结果确定，避免打乱
// LLM provider 的 prompt prefix cache（从 renderSystemPrompt 拆出，gocyclo 治理，行为不变）。
func (ic *ImmutableCore) renderUserPreferencesBlock() string {
	if len(ic.UserPreferences) == 0 {
		return ""
	}
	keys := make([]string, 0, len(ic.UserPreferences))
	for k := range ic.UserPreferences {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys)+1)
	lines = append(lines, "## User Preferences")
	for _, k := range keys {
		lines = append(lines, fmt.Sprintf("- %s: %s", k, ic.UserPreferences[k]))
	}
	return strings.Join(lines, "\n")
}

// maxSystemPromptBytes 系统提示词单次渲染的字节数硬性上限（≈8K tokens at 4 chars/token）。
// 超出部分截断并记录 warn，防止大量插件/工具文本撑爆 LLM context window。
// ambient skill 全文注入有独立的 maxFullTextChars 预算，两者各自独立保护。
const maxSystemPromptBytes = 32_000

func (ic *ImmutableCore) PrependToMessages(msgs []types.Message) []types.Message {
	content := ic.renderSystemPrompt()

	// 去除多余的尾部换行
	content = strings.TrimRight(content, "\n")

	// 如果全部为空，给一个默认提示词
	if content == "" {
		content = "你是 Polaris AI Agent。"
	}

	// 系统提示词硬性截断：防止大量插件/工具文本撑爆 context window（仅限 stable+volatile 层）
	if len(content) > maxSystemPromptBytes {
		originalBytes := len(content)
		truncated := content[:maxSystemPromptBytes]
		// 截到最后一个完整段落（至少保留前半部分），避免在段中截断
		if idx := strings.LastIndex(truncated, "\n\n"); idx > maxSystemPromptBytes/2 {
			truncated = truncated[:idx]
		}
		content = truncated + "\n\n[...系统提示词已截断]"
		slog.Warn("system prompt truncated",
			"original_bytes", originalBytes, "cap_bytes", maxSystemPromptBytes)
	}

	// AmbientContext 在模板渲染完成后追加，不经过 Go template 解析器。
	// 这样 skill instructions 含 {{ }} 时不会破坏模板解析（Bug-fix: template injection）。
	// AmbientContext 有独立的 maxFullTextChars(128K) 预算，不纳入上方截断逻辑。
	if ic.AmbientContext != "" {
		content += ic.AmbientContext
	}

	return append([]types.Message{{Role: "system", Content: content}}, msgs...)
}
