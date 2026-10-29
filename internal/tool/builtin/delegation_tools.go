package builtin

import (
	"fmt"

	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/tool"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// RegisterDelegationTools 注册委派相关内置工具：list_agents（本地子 Agent + MCP A2A 目标）与
// transfer_to_agent（执行由 Agent 内核特判，见 transferToAgentTool）。lister 为 nil 时对应来源
// 不列出，工具仍注册，保持与其余工具一致的降级行为。
func RegisterDelegationTools(sbx *sandbox.InProcessSandbox, toolReg *tool.InMemoryToolRegistry, local LocalAgentLister, a2a A2AAgentLister) error {
	for _, reg := range []struct {
		t  types.Tool
		fn sandbox.InProcessFn
	}{
		{listAgentsTool(), MakeListAgentsFn(local, a2a)},
		{transferToAgentTool(), transferToAgentFn},
	} {
		sbx.Register(reg.t.Name, reg.fn)
		if err := toolReg.Register(reg.t); err != nil {
			return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("delegation_tools: register %s", reg.t.Name), err)
		}
	}
	return nil
}
