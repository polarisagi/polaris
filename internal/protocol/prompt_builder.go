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

// 五层前缀账本（ADR-0105 决策一）：内核各阶段消息按"变化频率"由低到高排列，
// 使同一回合的全部阶段与相邻回合共享最长的字节一致前缀。层与 Zone 正交：
// Zone 决定信任级别与围栏，Layer 只决定输出次序。
const (
	// LayerStable L0 稳定核：ImmutableCore 稳定层（部署/配置变更才变）。
	LayerStable = 0
	// LayerSession L1 会话层：核心记忆、子 Agent 画像、工作区上下文、扩展目录（会话内低频）。
	LayerSession = 1
	// LayerHistory L2 历史层：对话历史，逐条真实消息，只追加（TaintHigh 围栏逐条）。
	LayerHistory = 2
	// LayerPhase L3 阶段层：阶段契约模板 + 易变层（日期/AmbientContext/压力提示/预算约束/ToolHints）。
	LayerPhase = 3
	// LayerTurn L4 回合层：召回、TaskModel、GroundingGap、观测、重规划反馈、执行结果、本轮意图。
	LayerTurn = 4
	// LayerCount 层数。
	LayerCount = 5
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
	// WriteToolHints 将工具自进化闭环产出的 <tool-hints> XML 块写入 ZoneMutableSkill（学习产物，信任度低于 ZoneImmutable）。
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
