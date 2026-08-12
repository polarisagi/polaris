package protocol

import (
	"github.com/polarisagi/polaris/internal/security/taint"
)

// CodeActRequest CodeAct 执行请求。
type CodeActRequest struct {
	Language     string // "python" | "bash"
	Code         string // LLM 生成的代码文本
	CapabilityID string // 必须携带有效 CapabilityToken（inv_global_07）
	SessionID    string
	AgentID      string
	// 此处曾有 TaintLevel 字段，2026-08-12 删除（C-8）。它由调用方自报、全仓零读取点：
	// CodeAct 执行的是 LLM 生成代码，按定义恒为最高风险，Execute() 构造
	// sandbox.ExecRequest 时硬编码 TaintLevel:TaintHigh，从不采信入参。留着一个
	// "看起来很安全相关、实际无人读"的字段，只会诱导后来者拿它做安全判定。
	// StatefulSession 为 true 时（GD-4-002），同一 SessionID 的多次调用间通过
	// 状态快照文件（python: pickle；bash: declare -p）延续全局变量/环境，
	// 每次调用仍是独立的一次性沙箱执行，安全边界不变。默认 false（不启用，
	// 与既有一次性执行行为完全一致，不影响未显式选用此特性的调用方）。
	StatefulSession bool
}

// CodeActResult CodeAct 执行结果。
type CodeActResult struct {
	Output    taint.TaintedString // 恒为 TaintHigh：沙箱执行 LLM 生成代码的产物，见 code_act.go Execute
	ExitCode  int
	LatencyMs int64
}
