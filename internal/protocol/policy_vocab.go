package protocol

// 工具执行授权请求的词汇。执行闸门（sandbox.ExecEnvelope）与 S_VALIDATE L1 预检
// （execute/dag.validatePolicyGate）共用：预检若换一套 principal/action 发问，策略
// 模型里没有规则认识它，deny-by-default 会拒绝一切工具（2026-09-25 实测，ADR-0098）。
const (
	PolicyPrincipalAgent    = "agent"
	PolicyActionToolExecute = "tool_execute"
)
