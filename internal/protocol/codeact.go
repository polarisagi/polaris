package protocol

import "github.com/polarisagi/polaris/pkg/types"

// CodeActRequest CodeAct 执行请求。
type CodeActRequest struct {
	Language     string // "python" | "bash"
	Code         string // LLM 生成的代码文本
	CapabilityID string // 必须携带有效 CapabilityToken（inv_global_07）
	SessionID    string
	AgentID      string
	TaintLevel   types.TaintLevel
	// StatefulSession 为 true 时（GD-4-002），同一 SessionID 的多次调用间通过
	// 状态快照文件（python: pickle；bash: declare -p）延续全局变量/环境，
	// 每次调用仍是独立的一次性沙箱执行，安全边界不变。默认 false（不启用，
	// 与既有一次性执行行为完全一致，不影响未显式选用此特性的调用方）。
	StatefulSession bool
}

// CodeActResult CodeAct 执行结果。
type CodeActResult struct {
	Output    []byte
	ExitCode  int
	LatencyMs int64
}
