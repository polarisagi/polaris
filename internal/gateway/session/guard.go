package session

import (
	"github.com/polarisagi/polaris/internal/security/guard"
)

// newTurnSystemPromptGuard 构造交互式路径每轮专用的 SystemPromptGuard——同时
// 注册 FSM 内核阶段模板（静态指令主体）与 activatedSysPrompt（M9 GEPA 动态
// 激活提示词，可能为空），覆盖两类"系统提示词"来源，不只挡后者。
// 原 chat/sse.go handleAgentStreamFSM 内联逻辑原样迁入。
func newTurnSystemPromptGuard(activatedSysPrompt string) *guard.SystemPromptGuard {
	g := guard.NewSystemPromptGuard(0)
	for _, frag := range guard.KernelPromptFragments() {
		g.AddFragment(frag)
	}
	g.AddFragment(activatedSysPrompt)
	return g
}

// [A-03 Step5 决策修正] 本文件曾计划把 internal/agent/pool.go 的
// headlessPromptGuard 单例整体迁入本包，并让 pool.go 删除原版、"回归纯 Agent
// 生命周期原语"。核实 Step5 真实调用面后发现该计划有安全空洞：
// internal/eval/red_team.go:160、internal/execute/orchestrator/
// default_worker.go:130 两处直接调用 AgentPool.AcquireHeadless，完全不经过
// session.Orchestrator.RunTurn（它们不是"会话轮次"——无 sessionID/持久化/多轮
// 历史语义，是一次性 Agent 探测/DAG 任务执行）。若真的从 pool.go 删除该单例，
// 这两处会静默失去 SystemPromptGuard 保护（OWASP LLM07 系统提示词泄露）。
// 结论：保留 pool.go 内联单例作为 AcquireHeadless 的唯一/规范扫描点——它已经
// 覆盖当前及未来任何直接调用 AcquireHeadless 的场景，是比"每个调用方各自记得
// 扫一遍"更安全的收敛方式（fail-safe 默认值 vs. opt-in）。runHeadless
// （orchestrator_headless.go）不再重复扫描：AcquireHeadless 返回的 res.Output
// 在到达这里之前已经过 pool.go 的净化，本包无需（也不应）二次持有一份重复的
// 单例。详见 red_team.go/default_worker.go 对应行的豁免说明注释。
