package builtin

import (
	"fmt"

	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/tool"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// RegisterA2ATools 注册 ADR-0084 引入的 MCP A2A 相关内置工具（目前仅
// list_a2a_agents；transfer_to_agent 本身在此之前已注册，见 agent_execute_dag.go
// 对该工具名的特判分发，不经由本函数）。lister 为 nil 时工具仍注册，执行时
// 返回空列表（见 MakeListA2AAgentsFn），保持与其余工具一致的降级行为。
func RegisterA2ATools(sbx *sandbox.InProcessSandbox, toolReg *tool.InMemoryToolRegistry, lister A2AAgentLister) error {
	t := listA2AAgentsTool()
	sbx.Register(t.Name, MakeListA2AAgentsFn(lister))
	if err := toolReg.Register(t); err != nil {
		return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("a2a_tools: register %s", t.Name), err)
	}
	return nil
}
