package policy

import (
	"strings"

	"github.com/polarisagi/polaris/pkg/types"
)

// WritesState 判定工具是否产生写状态或副作用。
// standard 档与只读角色下，仅放行纯只读且幂等的工具。
// 规则：
//  1. code_act 恒视为可写（任意代码执行）；
//  2. 未知工具（!known）fail-closed 视为可写；
//  3. 技能（ToolSkill）与 MCP 工具（ToolMCP）fail-closed 视为可写
//     （MCP 工具注册时无能力等级，零值恰为 CapReadOnly，必须防范写类 MCP 越权）；
//  4. Capability > CapReadOnly 视为可写。
func WritesState(name string, tool types.Tool, known bool) bool {
	return strings.HasPrefix(name, "code_act:") || !known || tool.Source == types.ToolSkill || tool.Source == types.ToolMCP ||
		tool.Capability > types.CapReadOnly
}
