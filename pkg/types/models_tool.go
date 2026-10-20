package types

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// NewToolUIViewID 生成 MCP Apps 视图 ID（"view_" + 16 字节随机十六进制，crypto/rand）。
// crypto/rand 失败（极罕见）时退化为纳秒时间戳，不阻断工具结果发布。
func NewToolUIViewID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("view_%d", time.Now().UnixNano())
	}
	return "view_" + hex.EncodeToString(b)
}

type

// SandboxSpec 沙箱执行规格（Sbx-L1/L2/L3 共用入参）。
SandboxSpec struct {
	ImageOrBinary    []byte
	Args             []string
	Env              map[string]string
	StdinJSON        []byte
	CPUQuotaPct      int
	MemoryLimitMB    int
	WallClockTimeout int64 // seconds
	NetworkEgress    bool
}

type

// SandboxResult 沙箱执行结果。
SandboxResult struct {
	Output     []byte
	ExitCode   int
	LatencyMs  int64
	MemoryPeak int64
}

type

// ToolResult 工具调用的统一返回结构。
ToolResult struct {
	Success    bool
	Output     []byte
	LatencyMs  int64
	Error      string
	TaintLevel TaintLevel
	// Suspended 表示工具执行使当前任务挂起（如 spawn_planner）。
	Suspended bool
	// ImageParts 工具执行返回的图片内容（MCP type="image" content block 等）。
	// nil 表示无图片输出，现有工具无需修改。
	ImageParts []ImagePart
	// MCPRaw 仅 MCP 工具调用非空：原始 CallToolResult JSON（content/structuredContent/
	// _meta/isError），供关联 MCP Apps UI 视图时随 tool_ui 事件透传给前端宿主
	// （M8f-1）。不参与 Output 的文本裁剪，不得据此推导模型可见输出。
	MCPRaw json.RawMessage
}

// WithTools 设置提供的工具列表。
func WithTools(tools []ToolSchema) InferOption {
	return func(o *InferOptions) { o.Tools = tools }
}

type Tool struct {
	Name         string
	Description  string
	Version      string
	InputSchema  any // JSON Schema
	OutputSchema any // JSON Schema
	Capability   CapabilityLevel
	SideEffects  []SideEffect
	RiskLevel    RiskLevel
	SandboxTier  SandboxTier
	TrustTier    TrustTier
	Source       ToolSource
	SourceURI    string
	// ReadOnlyHint 仅 Source==ToolMCP 时有意义：服务器声明 readOnlyHint=true 且未声明 destructiveHint=true。
	ReadOnlyHint bool
	// UndoFn 已删除（ADR-0088 决策一）：全仓从未被赋值（tool.yaml 加载器无对应
	// 字段映射），且工具定义层拿不到本次调用的实参，无法表达"撤销刚才那次具体
	// 操作"。Saga 补偿改由 protocol.ExecNode.Compensation 单一承载——它携带
	// 本次调用的实参，由 execute/dag.runCompensation 唯一执行。
	Timeout     time.Duration
	RetryPolicy *RetryPolicy
}

// EffectiveCapability 返回用于污点/审批判定的有效能力等级（fail-closed）。
// MCP 与技能工具注册时没有 manifest 能力声明，Capability 零值恰为 CapReadOnly，
// 直接使用会让写类外部工具被当作只读放行：
//   - MCP：默认按网络写；仅"服务器声明只读且来自受信服务器（注册时风险等级 <= Medium）"
//     才降为读取型联网（annotations 只是提示，未受信服务器不采信）；
//   - 技能：至少本地写。
func (t Tool) EffectiveCapability() CapabilityLevel {
	switch t.Source {
	case ToolMCP:
		if t.ReadOnlyHint && t.RiskLevel <= RiskMedium {
			return max(t.Capability, CapReadNetwork)
		}
		return max(t.Capability, CapWriteNetwork)
	case ToolSkill:
		return max(t.Capability, CapWriteLocal)
	}
	return t.Capability
}

type ToolCallRequest struct {
	ID             string
	ToolName       string
	Args           []byte
	InputTaint     TaintLevel
	CapabilityID   string
	SandboxLevel   int
	DeadlineNs     int64
	IdempotencyKey IdempotencyKey
}
