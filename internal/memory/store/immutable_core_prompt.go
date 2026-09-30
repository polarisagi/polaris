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

	"github.com/polarisagi/polaris/configs"
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

	// 按"变化频率"从低到高排列（字节前缀缓存：越靠前的内容越多请求共享，任何一处变化使其后全部失配；
	// 超过 maxSystemPromptBytes 时截断从尾部开始，即最先牺牲最易变的画像/偏好）：
	//   配置期（部署/配置变更才变）→ 安装期（装卸工具/扩展才变）→ 会话间演化（画像、偏好随使用变化）。
	// 阶段契约段（部署期常量）在此之前，作为独立的第一条 system 消息，见 StableMessagesWithContracts。
	var parts []string

	// —— 配置期 ——
	// 1. 身份（SoulMDContent 已由 server 按三层优先级填充）
	if ic.SoulMDContent != "" {
		parts = append(parts, ic.SoulMDContent)
	} else {
		// server 未注入时的最终兜底（不应触发，仅防御性保护）
		parts = append(parts, protocol.DefaultPolarisIdentityFallback)
	}

	// 2. 模型专属工具调用引导
	if ic.ModelGuidance != "" {
		parts = append(parts, ic.ModelGuidance)
	}

	// 3. 用户自定义追加指令（追加而非覆盖，保留产品基线行为）
	if ic.CustomInstructions != "" {
		parts = append(parts, ic.CustomInstructions)
	}

	// 4. 平台感知提示
	if ic.PlatformHint != "" {
		parts = append(parts, ic.PlatformHint)
	}

	// 5. 操作指令 (Memory Hygiene 等)：来自提示词管理器，随配置变更而非随会话变化
	if ic.OperationalDirectives != "" {
		parts = append(parts, ic.OperationalDirectives)
	}

	// —— 安装期 ——
	// 6. 工具/扩展感知摘要（仅名称，细节由 function schema 传递）
	if toolHint := ic.renderToolHint(); toolHint != "" {
		parts = append(parts, toolHint)
	}

	// —— 会话间演化 ——
	// 7. 用户画像 (L3 摘要)
	if ic.UserProfile != "" {
		parts = append(parts, ic.UserProfile)
	}

	// 8. 用户显式偏好画像（PersonaRefiner，M05 §2.3；与 7 的 UserProfile
	// 互补，见 chat/system_prompt.go 写入侧注释）。map 迭代顺序不确定，排序后拼接
	// 保证同一画像状态下渲染结果确定，避免打乱 LLM provider 的 prompt prefix cache。
	if prefsBlock := ic.renderUserPreferencesBlock(); prefsBlock != "" {
		parts = append(parts, prefsBlock)
	}

	// 6. volatile（VolatileBlock/AmbientContext）不在此渲染，见 PrependToMessages。

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

// StableMessage 渲染 L0 的可变稳定核：单条只含稳定层的 system 消息（含 maxSystemPromptBytes 截断）。
//
// 内核四阶段（agent/context 与 agent/fsm 的 prompt 构造）把它放在整个前缀账本的最前面
// （ADR-0105 决策一 L0），易变层不再紧随其后——见 VolatileContent。
//
// DeepSeek 前缀缓存以消息为单元整块匹配（api-docs guides/kv_cache："只有完整匹配一个
// 缓存前缀单元才会命中"）：稳定层任何字节变化都会使其后全部内容失配，因此本函数的输入
// 必须是确定的（集合有序、无时间/连接状态，见 chat/system_prompt.go）。
//
// 本方法**不含**阶段契约段：PrependToMessages（网关直连/cron/workflow 等非内核调用方）
// 走这里。这些路径不做 Perceive/Plan/Reflect/Respond 阶段调用，多付 ~7.5KB（≈2.5K token
// 命中价）毫无收益，还会在无选择器的对话里引入互相竞争的输出格式；故只有内核经
// StableMessagesWithContracts 得到含契约段的 L0（ADR-0105 决策九）。
func (ic *ImmutableCore) StableMessage() types.Message {
	stable := *ic
	stable.VolatileBlock = ""
	stable.AmbientContext = ""
	content := stable.renderSystemPrompt()

	// 去除多余的尾部换行
	content = strings.TrimRight(content, "\n")

	// 如果全部为空，给一个默认提示词
	if content == "" {
		content = "你是 Polaris AI Agent。"
	}

	// 系统提示词硬性截断：防止大量插件/工具文本撑爆 context window（仅限可变的稳定层部分）。
	// 渲染顺序按变化频率升序，截断从尾部开始，故最先牺牲最易变的画像/偏好。
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
	return types.Message{Role: "system", Content: content}
}

// HasPhaseContracts 报告本进程能否渲染出完整的阶段契约段（四个嵌入模板均可读）。
// 内核据此决定各阶段 L3 写选择器还是回退写完整模板，二者必须与 L0 是否含契约段一致。
func (ic *ImmutableCore) HasPhaseContracts() bool {
	return configs.PhaseContractsSection() != ""
}

// StableMessagesWithContracts 渲染内核前缀账本的 L0：两条 system 消息，按稳定度降序——
//
//  1. "# PHASE CONTRACTS" 阶段契约段（部署期常量，configs.PhaseContractsSection，同一二进制
//     字节恒定，与会话/用户/阶段/时间无关，约 7.9KB，L0 里最大且最稳定的一块）；
//  2. StableMessage() 的可变稳定核（配置期身份/指令/平台 → 安装期工具名 → 会话间演化的画像/偏好）。
//
// 为什么契约在前且单独成消息：此前契约追加在可变部分之后，画像/偏好/工具名任一变化都使契约段
// 缓存失配，而契约是最大的稳定块；放到最前，所有会话、所有用户共享同一份契约前缀缓存
// （DeepSeek/OpenAI 的前缀缓存按字节前缀匹配；Anthropic 适配器恒把首个 system block 作为第一个
// 缓存断点，契约独立成块才能被跨会话共享，见 anthropic_request.go applyPromptCaching）。
//
// 截断优先级：maxSystemPromptBytes 只约束第 2 条（可变部分，截断自尾部，最先牺牲画像与偏好）；
// 契约段是独立的消息，不计入该上限，因此无论用户自定义指令多长都不会被截掉——契约是四阶段输出
// 格式的唯一来源，被截断会直接导致解析失败，比多付固定 ~7.9KB 严重得多。
// 契约库不可用（模板读取失败，构建缺陷）时只返回可变稳定核，调用方经 HasPhaseContracts 回退 L3 全文模板。
func (ic *ImmutableCore) StableMessagesWithContracts() []types.Message {
	variable := ic.StableMessage()
	section := configs.PhaseContractsSection()
	if section == "" {
		return []types.Message{variable}
	}
	return []types.Message{{Role: "system", Content: section}, variable}
}

// VolatileContent 返回易变层文本（日期 VolatileBlock、扩展连接状态、按本轮问题挑选的
// AmbientContext）；无易变内容时返回空串。调用方负责把它放进 L3 阶段层（历史之后）。
func (ic *ImmutableCore) VolatileContent() string {
	return ic.volatileSystemContent()
}

// PrependToMessages 供不经内核前缀账本的调用方（网关直连 LLM、cron/workflow 等）使用：
// 在 msgs 前插入稳定层 system 消息，易变层作为 system 消息插在**最后一条消息之前**
// （即历史之后、本轮输入之前），保证 [稳定层 + 历史] 前缀跨请求字节一致。
// msgs 为空时易变层紧随稳定层。
//
// 注意：orchestrator 依赖"返回值末条是本轮用户消息"（history[:len-1]），所以易变层
// 不能追加在最末尾。
func (ic *ImmutableCore) PrependToMessages(msgs []types.Message) []types.Message {
	out := make([]types.Message, 0, len(msgs)+2)
	out = append(out, ic.StableMessage())
	volatile := ic.VolatileContent()
	if volatile == "" {
		return append(out, msgs...)
	}
	vmsg := types.Message{Role: "system", Content: volatile}
	if len(msgs) == 0 {
		return append(out, vmsg)
	}
	out = append(out, msgs[:len(msgs)-1]...)
	out = append(out, vmsg)
	return append(out, msgs[len(msgs)-1])
}

// volatileSystemContent 渲染易变层。AmbientContext 不经过 Go template 解析器——skill
// instructions 含 {{ }} 时不会破坏模板解析（Bug-fix: template injection）；它有独立的
// maxFullTextChars 预算，不纳入稳定层截断。
func (ic *ImmutableCore) volatileSystemContent() string {
	var parts []string
	if ic.VolatileBlock != "" {
		parts = append(parts, "# VOLATILE CONTEXT\n"+ic.VolatileBlock)
	}
	if a := strings.TrimSpace(ic.AmbientContext); a != "" {
		parts = append(parts, a)
	}
	return strings.Join(parts, "\n\n")
}
