package kernel

import (
	"github.com/polarisagi/polaris/configs"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/substrate"
)

const (
	ZoneImmutable    = 0
	ZoneMutableSkill = 1
	ZoneTaintedData  = 2
)

// PromptBuilder 是系统内唯一合法的 LLM Prompt 组装构造器。
// 它通过 Go 语言类型系统强制实现指令数据隔离（M11 §3 规定）。
type PromptBuilder struct {
	zones [3][]protocol.Message
}

// NewPromptBuilder 创建一个新的 Prompt 构造器。
func NewPromptBuilder() *PromptBuilder {
	return &PromptBuilder{}
}

// WriteInstruction 将已经证实为安全的指令写入 System 角色。
// 由于参数被强制要求为 substrate.SafeString，只有 TaintNone 或被彻底清洗过的内容才能进入此处。
func (b *PromptBuilder) WriteInstruction(safe substrate.SafeString) {
	b.zones[ZoneImmutable] = append(b.zones[ZoneImmutable], protocol.Message{
		Role:    "system",
		Content: safe.Content(),
	})
}

// WriteSystemEnvironment 将系统静态上下文（通常在初始化时生成一次快照）注入 System 角色。
// 这保证了 Agent 能立刻感知其所处的 OS、架构、时区和用户身份。
func (b *PromptBuilder) WriteSystemEnvironment(snapshot string) {
	b.zones[ZoneImmutable] = append(b.zones[ZoneImmutable], protocol.Message{
		Role:    "system",
		Content: snapshot,
	})
}

// WriteSkillContext 将技能上下文写入 ZoneMutableSkill 区。
func (b *PromptBuilder) WriteSkillContext(ctxMsg protocol.Message) {
	b.zones[ZoneMutableSkill] = append(b.zones[ZoneMutableSkill], ctxMsg)
}

// WriteUserData 将不受信的外部输入写入 User 角色，并强制进行 Spotlighting 围栏保护。
// 这可以防止 LLM 将恶意用户文本解析为隐藏的控制指令（Prompt Injection）。
func (b *PromptBuilder) WriteUserData(ts substrate.TaintedString) {
	b.zones[ZoneTaintedData] = append(b.zones[ZoneTaintedData], protocol.Message{
		Role:    "user",
		Content: substrate.Spotlighting(ts),
	})
}

// WriteUserInstruction 允许将 SafeString 写入 User 角色。
// 用于某些特定场景下需要由 User 发起但内容确认为系统硬编码的安全指令。
func (b *PromptBuilder) WriteUserInstruction(safe substrate.SafeString) {
	b.zones[ZoneImmutable] = append(b.zones[ZoneImmutable], protocol.Message{
		Role:    "user",
		Content: safe.Content(),
	})
}

// Build 输出最终组装完毕可用于 InferRequest 的消息序列。
func (b *PromptBuilder) Build() []protocol.Message {
	var result []protocol.Message
	result = append(result, b.zones[ZoneImmutable]...)
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

	b.zones[ZoneImmutable] = append(b.zones[ZoneImmutable], protocol.Message{
		Role:    "system",
		Content: policy,
	})
}
