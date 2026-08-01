package protocol

import (
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/types"
)

const (
	ZoneImmutable    = 0
	ZoneCoreMemory   = 1
	ZoneMutableSkill = 2
	// ZoneExternalCatalog 第三方工具/扩展目录区（S-02，M11 §3）：内容由 MCP 服务器
	// / 已安装扩展自述，来源不可信但语义上是"功能性目录"，信任度低于 ZoneMutableSkill
	// 但优先级高于纯用户数据（模型必须先看到可用能力才能规划）。
	ZoneExternalCatalog = 3
	ZoneTaintedData     = 4
	zoneCount           = 5
)

// PromptBuilder 是系统内唯一合法的 LLM Prompt 组装构造器。
// 它通过 Go 语言类型系统强制实现指令数据隔离（M11 §3 规定）。
type PromptBuilder interface {
	// WriteInstruction 将已经证实为安全的指令写入 System 角色。
	WriteInstruction(safe taint.SafeString)
	// WriteSystemEnvironment 将系统静态上下文注入 System 角色。
	WriteSystemEnvironment(snapshot string)
	// WriteCoreMemory 将核心工作记忆写入 ZoneCoreMemory 区。
	WriteCoreMemory(blocks []types.CoreMemoryBlock)
	// WriteUserData 将不受信的外部输入写入 User 角色，并强制进行 Spotlighting 围栏保护。
	WriteUserData(ts taint.TaintedString)
	// WriteUserImages 将图片等媒体块写入 User 角色。
	WriteUserImages(imgs []types.ImagePart)
	// WriteComputerUsePolicy 写入电脑操控权限的系统指令。
	WriteComputerUsePolicy(mode string, anyAppEnabled, chromeEnabled bool)
	// WriteToolHints 将工具自进化闭环产出的 <tool-hints> XML 块写入 ZoneImmutable。
	WriteToolHints(hint string)
	// WriteExternalCatalog 写入第三方来源的工具/扩展目录（S-02）。
	// kind 为目录类别（"tools" | "extensions"），ts 为渲染后的目录正文及其来源污点。
	// level >= TaintMedium 时内部强制 Spotlighting 包裹，禁止调用方自行绕过；
	// 空内容（ts.Value()==""）直接跳过。
	WriteExternalCatalog(kind string, ts taint.TaintedString)
	// Build 输出最终组装完毕可用于 InferRequest 的消息序列。
	Build() []types.Message
}

// DefaultPolarisIdentityFallback 是极简兜底文本。
const DefaultPolarisIdentityFallback = "你是 Polaris，一个开源自托管 AI Agent。你直接高效，有工具时立即调用。"
