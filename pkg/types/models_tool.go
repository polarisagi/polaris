package types

import "time"

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
	UndoFn       string // 补偿工具的名称 (ISSUE-03)
	Timeout      time.Duration
	RetryPolicy  *RetryPolicy
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
