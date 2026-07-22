package protocol

import (
	"fmt"

	"github.com/polarisagi/polaris/configs"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/types"
)

const (
	ZoneImmutable    = 0
	ZoneCoreMemory   = 1
	ZoneMutableSkill = 2
	ZoneTaintedData  = 3
)

// PromptBuilder 是系统内唯一合法的 LLM Prompt 组装构造器。
// 它通过 Go 语言类型系统强制实现指令数据隔离（M11 §3 规定）。
type PromptBuilder struct {
	zones [4][]types.Message
}

// NewPromptBuilder 创建一个新的 Prompt 构造器。
func NewPromptBuilder() *PromptBuilder {
	return &PromptBuilder{}
}

// WriteInstruction 将已经证实为安全的指令写入 System 角色。
// 由于参数被强制要求为 taint.SafeString，只有 TaintNone 或被彻底清洗过的内容才能进入此处。
func (b *PromptBuilder) WriteInstruction(safe taint.SafeString) {
	b.zones[ZoneImmutable] = append(b.zones[ZoneImmutable], safe.IntoMessage("system"))
}

// WriteSystemEnvironment 将系统静态上下文（通常在初始化时生成一次快照）注入 System 角色。
// 这保证了 Agent 能立刻感知其所处的 OS、架构、时区和用户身份。
func (b *PromptBuilder) WriteSystemEnvironment(snapshot string) {
	b.zones[ZoneImmutable] = append(b.zones[ZoneImmutable], types.Message{
		Role:    "system",
		Content: snapshot,
	})
}

// WriteCoreMemory 将核心工作记忆写入 ZoneCoreMemory 区。
func (b *PromptBuilder) WriteCoreMemory(blocks []types.CoreMemoryBlock) {
	for _, block := range blocks {
		content := fmt.Sprintf("<core_memory block=\"%s\">\n%s\n</core_memory>", block.BlockKey, block.Content)
		if block.TaintLevel >= types.TaintHigh {
			content = taint.Spotlighting(taint.NewTaintedString(content, taint.TaintSource{OriginTaintLevel: block.TaintLevel}, "core_memory"))
		}

		b.zones[ZoneCoreMemory] = append(b.zones[ZoneCoreMemory], types.Message{
			Role:    "system",
			Content: content,
		})
	}
}

// WriteUserData 将不受信的外部输入写入 User 角色，并强制进行 Spotlighting 围栏保护。
// 这可以防止 LLM 将恶意用户文本解析为隐藏的控制指令（Prompt Injection）。
func (b *PromptBuilder) WriteUserData(ts taint.TaintedString) {
	b.zones[ZoneTaintedData] = append(b.zones[ZoneTaintedData], types.Message{
		Role:    "user",
		Content: taint.Spotlighting(ts),
	})
}

// WriteUserImages 将图片等媒体块写入 User 角色。
func (b *PromptBuilder) WriteUserImages(imgs []types.ImagePart) {
	if len(imgs) == 0 {
		return
	}
	parts := make([]any, 0, len(imgs))
	for _, img := range imgs {
		parts = append(parts, img)
	}
	b.zones[ZoneTaintedData] = append(b.zones[ZoneTaintedData], types.Message{
		Role:  "user",
		Parts: parts,
	})
}

// Build 输出最终组装完毕可用于 InferRequest 的消息序列。
func (b *PromptBuilder) Build() []types.Message {
	var result []types.Message //nolint:prealloc
	result = append(result, b.zones[ZoneImmutable]...)
	result = append(result, b.zones[ZoneCoreMemory]...)
	result = append(result, b.zones[ZoneMutableSkill]...)
	result = append(result, b.zones[ZoneTaintedData]...)
	return result
}

// WriteComputerUsePolicy 写入电脑操控权限的系统指令。
func (b *PromptBuilder) WriteComputerUsePolicy(mode string, anyAppEnabled, chromeEnabled bool) {
	if mode == "" {
		mode = "auto_review"
	}

	data := map[string]any{
		"Mode":          mode,
		"AnyAppEnabled": anyAppEnabled,
		"ChromeEnabled": chromeEnabled,
	}

	policy, err := configs.LoadPromptTemplate("kernel/computer_use_policy.md", data)
	if err != nil {
		// Fallback safely if configs missing, though this should not happen in production.
		policy = "Computer Use Confirmations Policy: mode=" + mode
	}

	b.zones[ZoneImmutable] = append(b.zones[ZoneImmutable], types.Message{
		Role:    "system",
		Content: policy,
	})
}

// WriteToolHints 将工具自进化闭环（action.PolicyEvolver）产出的 <tool-hints> XML
// 块写入 ZoneImmutable。内容 100% 由系统内部工具调用统计生成（非用户/外部输入），
// 与 WriteComputerUsePolicy 同属"内部可信策略文本"，故直接进 ZoneImmutable，不走
// SafeString/taint 清洗路径（该路径服务于外部可能不可信的输入）。hint 为空时不写入
// （PolicyEvolver.BuildSystemHintBlock 冷启动/无数据时返回空串，调用方不应注入噪声）。
func (b *PromptBuilder) WriteToolHints(hint string) {
	if hint == "" {
		return
	}
	b.zones[ZoneImmutable] = append(b.zones[ZoneImmutable], types.Message{
		Role:    "system",
		Content: hint,
	})
}

// DefaultPolarisIdentityFallback 是极简兜底文本。
const DefaultPolarisIdentityFallback = "你是 Polaris，一个开源自托管 AI Agent。你直接高效，有工具时立即调用。"
