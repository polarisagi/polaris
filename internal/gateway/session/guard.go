package session

import (
	"sync"

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

// headlessPromptGuard 供 runHeadless（Cron/Workflow/Webhook 触发路径的唯一
// 收敛入口）扫描最终输出，堵住此前 SSE 交互路径早已接入 SystemPromptGuard、
// 但 headless 路径从未调用的缺口（OWASP LLM07 系统提示词逐字泄露）。原
// internal/agent/pool.go 同名单例迁入本包，随 A-03 Step5 一并从 pool.go 删除
// （AcquireHeadless 回归"纯 Agent 生命周期原语"，该领域职责上移至此）。
// headless 场景不像交互式路径有逐会话的 M9 GEPA 激活提示词可注册，只注册
// 内核阶段模板——这是"系统提示词"的静态主体，覆盖面已经是从 0 到有。
var headlessPromptGuard = sync.OnceValue(func() *guard.SystemPromptGuard { //nolint:gochecknoglobals // sync.OnceValue 懒加载单例，SystemPromptGuard 内部自带锁，无外部可变状态
	g := guard.NewSystemPromptGuard(0)
	for _, frag := range guard.KernelPromptFragments() {
		g.AddFragment(frag)
	}
	return g
})
